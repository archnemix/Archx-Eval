package bank

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeBank(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const codeRow = `{"id":"py-e-1","category":"python","difficulty":"easy","kind":"code","language":"python","entrypoint":"solve","prompt":"Return 1","reference_solution":"def solve(): return 1","tests":["assert solve() == 1"]}`

const qaRow = `{"id":"py-m-1","category":"python","difficulty":"medium","kind":"qa","prompt":"What is 2+2?","expected_answer":"4"}`

func TestLoadAndValidate(t *testing.T) {
	dir := writeBank(t, map[string]string{
		"python/easy.jsonl":   codeRow + "\n",
		"python/medium.jsonl": qaRow + "\n",
	})
	m, err := Load(dir, 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Size() != 2 {
		t.Fatalf("expected 2 examples, got %d", m.Size())
	}
	if cats := m.Categories(); len(cats) != 1 || cats[0] != "python" {
		t.Fatalf("unexpected categories: %v", cats)
	}
	ex, ok := m.Get("py-e-1")
	if !ok || ex.Language != "python" {
		t.Fatal("expected to find py-e-1")
	}
}

func TestCategoryMismatchRejected(t *testing.T) {
	dir := writeBank(t, map[string]string{
		"python/easy.jsonl": qaRow + "\n", // qa row has category python but difficulty medium
	})
	if _, err := Load(dir, 3); err == nil {
		t.Fatal("expected difficulty mismatch to fail")
	}
}

func TestDuplicateIDRejected(t *testing.T) {
	dir := writeBank(t, map[string]string{
		"python/easy.jsonl": codeRow + "\n" + codeRow + "\n",
	})
	if _, err := Load(dir, 3); err == nil {
		t.Fatal("expected duplicate id to fail")
	}
}

func TestMissingSolutionRejected(t *testing.T) {
	bad := `{"id":"x","category":"python","difficulty":"easy","kind":"code","language":"python","prompt":"p","tests":["t"]}`
	dir := writeBank(t, map[string]string{"python/easy.jsonl": bad + "\n"})
	if _, err := Load(dir, 3); err == nil {
		t.Fatal("expected missing solution to fail")
	}
}

func TestEmptyBankIsLegal(t *testing.T) {
	m, err := Load(t.TempDir(), 3)
	if err != nil {
		t.Fatalf("empty bank should load: %v", err)
	}
	if m.Size() != 0 {
		t.Fatal("expected empty bank")
	}
}

func TestUsageMarksStaleAfterThreshold(t *testing.T) {
	dir := writeBank(t, map[string]string{"python/easy.jsonl": codeRow + "\n"})
	m, err := Load(dir, 2)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	now := time.Now()
	if got := len(m.Eligible("python", "easy")); got != 1 {
		t.Fatalf("expected 1 eligible, got %d", got)
	}
	m.MarkUsed([]string{"py-e-1"}, now)
	if got := len(m.Eligible("python", "easy")); got != 1 {
		t.Fatalf("expected 1 eligible after 1 use, got %d", got)
	}
	m.MarkUsed([]string{"py-e-1"}, now)
	if got := len(m.Eligible("python", "easy")); got != 0 {
		t.Fatalf("expected 0 eligible after 2 uses, got %d", got)
	}
	if stale := m.Stale(); len(stale) != 1 || stale[0].ID != "py-e-1" {
		t.Fatalf("expected py-e-1 to be stale, got %v", stale)
	}
	inv := m.Inventory()
	if inv.Total != 1 || inv.StaleCount != 1 || inv.Usable != 0 {
		t.Fatalf("bad inventory: %+v", inv)
	}
}

func TestUsagePersists(t *testing.T) {
	dir := writeBank(t, map[string]string{"python/easy.jsonl": codeRow + "\n"})
	m, err := Load(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	m.MarkUsed([]string{"py-e-1"}, time.Now())
	if err := m.SaveUsage(); err != nil {
		t.Fatalf("save usage: %v", err)
	}
	again, err := Load(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Uses("py-e-1"); got != 1 {
		t.Fatalf("expected 1 use after reload, got %d", got)
	}
}

func TestExampleValidateCoversKinds(t *testing.T) {
	code := Example{ID: "a", Category: "c", Difficulty: "easy", Kind: KindCode, Prompt: "p", Language: "go", Solution: "x", Tests: []string{"t"}}
	if err := code.Validate(); err != nil {
		t.Fatalf("valid code example rejected: %v", err)
	}
	qa := Example{ID: "b", Category: "c", Difficulty: "easy", Kind: KindQA, Prompt: "p", Answer: "a"}
	if err := qa.Validate(); err != nil {
		t.Fatalf("valid qa example rejected: %v", err)
	}
	logic := Example{ID: "c", Category: "c", Difficulty: "easy", Kind: KindLogic, Prompt: "p"}
	if err := logic.Validate(); err == nil {
		t.Fatal("logic example without answer should fail")
	}
}
