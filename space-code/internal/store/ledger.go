package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// This file holds the two things that are not identity records:
//
//	subnets/<ab>/<hash>.json   bans on a network prefix
//	ledger/<YYYY-MM-DD>.jsonl  which questions were issued against which request
//
// Both are keyed by hashes. The subnet file never receives a prefix, only the
// hash of one; the ledger never receives a prompt or an answer, only ids.

// subnetMu guards the subnet ban map and its directory.
var subnetMu = struct {
	sync.Mutex
	once bool
}{}

// SubnetBanPath returns the on-disk path for a subnet ban.
func SubnetBanPath(root, netHash string) string {
	return filepath.Join(root, "subnets", netHash[:shardLen(netHash)], netHash+".json")
}

func shardLen(s string) int {
	if len(s) < 2 {
		return len(s)
	}
	return 2
}

// PutSubnetBan writes a subnet ban and updates the in-memory mirror.
func (s *Store) PutSubnetBan(b *SubnetBan) error {
	if b == nil || b.NetHash == "" {
		return nil
	}
	buf, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	p := SubnetBanPath(s.dir, b.NetHash)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	s.mu.Lock()
	s.subnetBans[b.NetHash] = b
	s.mu.Unlock()
	return nil
}

// GetSubnetBan returns a stored subnet ban.
func (s *Store) GetSubnetBan(netHash string) (*SubnetBan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.subnetBans[netHash]
	return b, ok
}

// loadSubnetBans reads the subnet ban directory at startup.
func (s *Store) loadSubnetBans() error {
	root := filepath.Join(s.dir, "subnets")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, shard := range entries {
		if !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, shard.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if filepath.Ext(f.Name()) != ".json" {
				continue
			}
			buf, err := os.ReadFile(filepath.Join(root, shard.Name(), f.Name()))
			if err != nil {
				continue
			}
			var b SubnetBan
			if json.Unmarshal(buf, &b) == nil && b.NetHash != "" {
				s.subnetBans[b.NetHash] = &b
			}
		}
	}
	return nil
}

// PruneSubnetBans forgets bans that expired more than keep ago. They are
// retained briefly after expiry so the escalation ladder still sees them.
func (s *Store) PruneSubnetBans(now time.Time, keep time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for hash, b := range s.subnetBans {
		if b.BannedUntil.IsZero() || b.BannedUntil.After(now.Add(-keep)) {
			continue
		}
		p := SubnetBanPath(s.dir, hash)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			continue
		}
		delete(s.subnetBans, hash)
		removed++
	}
	return removed, nil
}

// ---------------------------------------------------------------- ledger ---

// LedgerEntry records one draw: which identity received which questions, under
// which request id. Feedback is only accepted when it matches an entry here.
//
// The ledger is the integrity mechanism. Without it, /v1/feedback accepts any
// invented request id and any fabricated score, and the results dataset becomes
// whatever the last script to POST decided.
type LedgerEntry struct {
	RequestID  string    `json:"request_id"`
	IdentityID string    `json:"identity_id"`
	ExampleIDs []string  `json:"example_ids"`
	IssuedAt   time.Time `json:"issued_at"`
	// ReportedAt is set once valid feedback is accepted, which is what makes a
	// second submission for the same request id a replay.
	ReportedAt time.Time `json:"reported_at,omitempty"`
}

// LedgerPath is the append log for a day.
func LedgerPath(root string, at time.Time) string {
	return filepath.Join(root, "ledger", at.UTC().Format("2006-01-02")+".jsonl")
}

// AppendLedger writes one entry.
func (s *Store) AppendLedger(e LedgerEntry) error {
	p := LedgerPath(s.dir, e.IssuedAt)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	buf, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(buf, '\n')); err != nil {
		return err
	}
	s.mu.Lock()
	s.ledger[e.RequestID] = e
	s.mu.Unlock()
	return nil
}

// LeaseLedger returns the entry for a request id, if this process knows it.
func (s *Store) LeaseLedger(requestID string) (LedgerEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.ledger[requestID]
	return e, ok
}

// MarkLedgerReported records that feedback for this request id was accepted, so
// a replay is refused. It rewrites the entry in memory and appends a tombstone
// line so a restart cannot resurrect the unreported state.
func (s *Store) MarkLedgerReported(requestID string, at time.Time) error {
	s.mu.Lock()
	e, ok := s.ledger[requestID]
	if !ok {
		s.mu.Unlock()
		return os.ErrNotExist
	}
	e.ReportedAt = at
	s.ledger[requestID] = e
	s.mu.Unlock()

	p := LedgerPath(s.dir, e.IssuedAt)
	buf, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(buf, '\n'))
	return err
}

// loadLedger rebuilds the in-memory ledger from the most recent day files.
//
// It selects days by filename order, not by comparing against the wall clock.
// That matters in two ways: a container whose clock is skewed would otherwise
// load the wrong days, and tests that drive the service with a fake clock would
// silently load nothing at all.
func (s *Store) loadLedger(_ time.Time, days int) error {
	if days <= 0 {
		days = 3
	}
	dir := filepath.Join(s.dir, "ledger")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".jsonl" {
			names = append(names, e.Name())
		}
	}
	// Filenames are YYYY-MM-DD.jsonl, so lexical order is chronological.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > days {
		names = names[:days]
	}
	for _, name := range names {
		buf, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		for _, line := range splitLines(buf) {
			if len(line) == 0 {
				continue
			}
			var e LedgerEntry
			if json.Unmarshal(line, &e) == nil && e.RequestID != "" {
				// Later lines win, which is how a tombstone overrides the
				// original entry after a restart.
				s.ledger[e.RequestID] = e
			}
		}
	}
	return nil
}

func splitLines(buf []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range buf {
		if b == '\n' {
			out = append(out, buf[start:i])
			start = i + 1
		}
	}
	if start < len(buf) {
		out = append(out, buf[start:])
	}
	return out
}

// PruneLedger deletes day files older than keepDays.
func (s *Store) PruneLedger(now time.Time, keepDays int) (int, error) {
	if keepDays <= 0 {
		keepDays = 7
	}
	dir := filepath.Join(s.dir, "ledger")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.AddDate(0, 0, -keepDays)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".jsonl" {
			continue
		}
		day, err := time.Parse("2006-01-02", e.Name()[:len(e.Name())-len(".jsonl")])
		if err != nil || !day.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
			removed++
		}
	}
	return removed, nil
}

// ReadLedgerDay returns the raw ledger file for a day, for publishing to the
// dataset. Feedback stays verifiable across a Space restart only if the ledger
// is durable somewhere other than the container.
func (s *Store) ReadLedgerDay(at time.Time) ([]byte, error) {
	buf, err := os.ReadFile(LedgerPath(s.dir, at))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return buf, err
}
