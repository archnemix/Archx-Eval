// Package bank loads and serves the evaluation question bank.
//
// On-disk shape (BANK_DIR), category first, then difficulty:
//
//	BANK_DIR/python/easy.jsonl
//	BANK_DIR/python/medium.jsonl
//	BANK_DIR/python/hard.jsonl
//	BANK_DIR/logic/easy.jsonl
//
// Each line is one JSON object. The loader validates the file layout against
// the row contents, so a misfiled example fails loudly instead of quietly
// polluting a category.
package bank

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Difficulties in ascending order.
var Difficulties = []string{"easy", "medium", "hard"}

// Kind values.
const (
	KindCode  = "code"
	KindQA    = "qa"
	KindLogic = "logic"
)

// Example is one banked question.
type Example struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Difficulty string `json:"difficulty"`
	Kind       string `json:"kind"`

	Prompt  string `json:"prompt"`
	Context string `json:"context,omitempty"`

	// Code examples
	Language   string   `json:"language,omitempty"`
	Entrypoint string   `json:"entrypoint,omitempty"`
	Starter    string   `json:"starter_code,omitempty"`
	Solution   string   `json:"reference_solution,omitempty"`
	Tests      []string `json:"tests,omitempty"`

	// Non-code examples
	Answer      string   `json:"expected_answer,omitempty"`
	Rubric      []string `json:"rubric,omitempty"`
	Explanation string   `json:"explanation,omitempty"`

	// Provenance. GeneratedAt/VerifiedBy matter because the whole promise of
	// this bank is that a human checked the facts, not that a model guessed.
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generated_at,omitempty"`
	VerifiedAt  time.Time `json:"verified_at,omitempty"`
	VerifiedBy  string    `json:"verified_by,omitempty"`
	Source      string    `json:"source,omitempty"`

	Tags []string `json:"tags,omitempty"`
}

// Validate enforces the invariants every banked example must satisfy.
func (e *Example) Validate() error {
	if e.ID == "" {
		return errors.New("missing id")
	}
	if e.Category == "" {
		return fmt.Errorf("%s: missing category", e.ID)
	}
	if !validDifficulty(e.Difficulty) {
		return fmt.Errorf("%s: bad difficulty %q", e.ID, e.Difficulty)
	}
	if strings.TrimSpace(e.Prompt) == "" {
		return fmt.Errorf("%s: empty prompt", e.ID)
	}
	switch e.Kind {
	case KindCode:
		if e.Language == "" {
			return fmt.Errorf("%s: code example without language", e.ID)
		}
		if e.Solution == "" {
			return fmt.Errorf("%s: code example without reference solution", e.ID)
		}
		if len(e.Tests) == 0 {
			return fmt.Errorf("%s: code example without tests", e.ID)
		}
	case KindQA, KindLogic:
		if e.Answer == "" {
			return fmt.Errorf("%s: %s example without expected answer", e.ID, e.Kind)
		}
	case "":
		return fmt.Errorf("%s: missing kind", e.ID)
	default:
		return fmt.Errorf("%s: unknown kind %q", e.ID, e.Kind)
	}
	return nil
}

func validDifficulty(d string) bool {
	for _, v := range Difficulties {
		if d == v {
			return true
		}
	}
	return false
}

// Usage counts how often an example has been handed out.
type Usage struct {
	Count    int       `json:"count"`
	LastUsed time.Time `json:"last_used,omitempty"`
}

// Manager owns the in-memory bank and the usage counters.
type Manager struct {
	mu         sync.RWMutex
	dir        string
	usagePath  string
	byID       map[string]*Example
	byCategory map[string][]*Example
	usage      map[string]Usage
	staleAfter int
}

// Load reads every category/difficulty file under dir.
func Load(dir string, staleAfter int) (*Manager, error) {
	if staleAfter <= 0 {
		staleAfter = 3
	}
	m := &Manager{
		dir:        dir,
		usagePath:  filepath.Join(dir, "usage.json"),
		byID:       map[string]*Example{},
		byCategory: map[string][]*Example{},
		usage:      map[string]Usage{},
		staleAfter: staleAfter,
	}
	if err := m.scan(); err != nil {
		return nil, err
	}
	if err := m.loadUsage(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) scan() error {
	entries, err := os.ReadDir(m.dir)
	if errors.Is(err, os.ErrNotExist) {
		// An empty bank is legal: the relay serves 503 until data lands.
		return nil
	}
	if err != nil {
		return fmt.Errorf("bank: read dir: %w", err)
	}
	for _, cat := range entries {
		if !cat.IsDir() || strings.HasPrefix(cat.Name(), ".") {
			continue
		}
		category := cat.Name()
		for _, diff := range Difficulties {
			path := filepath.Join(m.dir, category, diff+".jsonl")
			if err := m.loadFile(path, category, diff); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) loadFile(path, category, difficulty string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bank: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	line := 0
	for sc.Scan() {
		line++
		raw := strings.TrimSpace(sc.Text())
		if raw == "" || strings.HasPrefix(raw, "//") {
			continue
		}
		var ex Example
		if err := json.Unmarshal([]byte(raw), &ex); err != nil {
			return fmt.Errorf("bank: %s:%d: %w", path, line, err)
		}
		// Path is the source of truth for placement; mismatches are bugs.
		if ex.Category != category {
			return fmt.Errorf("bank: %s:%d: category %q does not match directory %q", path, line, ex.Category, category)
		}
		if ex.Difficulty != difficulty {
			return fmt.Errorf("bank: %s:%d: difficulty %q does not match file %q", path, line, ex.Difficulty, difficulty)
		}
		if err := ex.Validate(); err != nil {
			return fmt.Errorf("bank: %s:%d: %w", path, line, err)
		}
		if _, dup := m.byID[ex.ID]; dup {
			return fmt.Errorf("bank: %s:%d: duplicate id %q", path, line, ex.ID)
		}
		clone := ex
		if clone.Version == 0 {
			clone.Version = 1
		}
		m.byID[clone.ID] = &clone
		m.byCategory[category] = append(m.byCategory[category], &clone)
	}
	return sc.Err()
}

func (m *Manager) loadUsage() error {
	buf, err := os.ReadFile(m.usagePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("bank: read usage: %w", err)
	}
	var wrapper struct {
		Usage map[string]Usage `json:"usage"`
	}
	if err := json.Unmarshal(buf, &wrapper); err != nil {
		return fmt.Errorf("bank: parse usage: %w", err)
	}
	for id, u := range wrapper.Usage {
		m.usage[id] = u
	}
	return nil
}

// SaveUsage writes counters to disk atomically.
func (m *Manager) SaveUsage() error {
	m.mu.RLock()
	buf, err := json.MarshalIndent(struct {
		Usage map[string]Usage `json:"usage"`
	}{Usage: m.usage}, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dir, ".usage-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(append(buf, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, m.usagePath)
}

// Categories lists the categories present, sorted.
func (m *Manager) Categories() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.byCategory))
	for c := range m.byCategory {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Size is the total number of examples loaded.
func (m *Manager) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}

// Get returns an example by ID.
func (m *Manager) Get(id string) (*Example, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ex, ok := m.byID[id]
	if !ok {
		return nil, false
	}
	clone := *ex
	return &clone, true
}

// Uses reports how many times an example has been handed out.
func (m *Manager) Uses(id string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.usage[id].Count
}

// Eligible returns the examples in a category/difficulty pair that still have
// uses left. Stale examples (used StaleAfter times) are excluded: they need
// replacing or rephrasing before they go back into circulation.
func (m *Manager) Eligible(category, difficulty string) []*Example {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Example, 0, 16)
	for _, ex := range m.byCategory[category] {
		if ex.Difficulty != difficulty {
			continue
		}
		if m.usage[ex.ID].Count >= m.staleAfter {
			continue
		}
		out = append(out, ex)
	}
	return out
}

// MarkUsed bumps the counters for issued examples.
func (m *Manager) MarkUsed(ids []string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		u := m.usage[id]
		u.Count++
		u.LastUsed = at
		m.usage[id] = u
	}
}

// Stale lists examples that have exhausted their allowed uses.
func (m *Manager) Stale() []Example {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Example
	for id, u := range m.usage {
		if u.Count < m.staleAfter {
			continue
		}
		if ex, ok := m.byID[id]; ok {
			out = append(out, *ex)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Stats summarises the bank for health reporting.
func (m *Manager) Stats() map[string]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int, len(m.byCategory)+1)
	out["_total"] = len(m.byID)
	for c, list := range m.byCategory {
		out[c] = len(list)
	}
	return out
}

// Counts is a per-category breakdown of total vs still-usable examples.
type Counts struct {
	Total      int            `json:"total"`
	ByCat      map[string]int `json:"by_category"`
	StaleCount int            `json:"stale"`
	Usable     int            `json:"usable"`
}

// Inventory reports how much of the bank is still in circulation.
func (m *Manager) Inventory() Counts {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c := Counts{Total: len(m.byID), ByCat: map[string]int{}}
	for cat, list := range m.byCategory {
		c.ByCat[cat] = len(list)
		for _, ex := range list {
			if m.usage[ex.ID].Count >= m.staleAfter {
				c.StaleCount++
			} else {
				c.Usable++
			}
		}
	}
	return c
}
