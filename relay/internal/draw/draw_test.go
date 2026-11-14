package draw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/bank"
)

func row(id, cat, diff string) string {
	return `{"id":"` + id + `","category":"` + cat + `","difficulty":"` + diff + `","kind":"qa","prompt":"p","expected_answer":"a"}`
}

func bankWith(t *testing.T, counts map[string]int) *bank.Manager {
	t.Helper()
	dir := t.TempDir()
	for cat, n := range counts {
		var b strings.Builder
		for i := 0; i < n; i++ {
			b.WriteString(row(cat+"-"+string(rune('a'+i)), cat, "easy") + "\n")
		}
		path := filepath.Join(dir, cat, "easy.jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	m, err := bank.Load(dir, 3)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return m
}

func TestSelectRespectsSize(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 100})
	res, err := Select(m, Request{Categories: []string{"python"}, Size: 75})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(res.Examples) != 75 {
		t.Fatalf("expected 75, got %d", len(res.Examples))
	}
	if res.ByCat["python"] != 75 {
		t.Fatalf("bad per-category count: %v", res.ByCat)
	}
}

func TestSelectNeverDuplicates(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 50})
	res, err := Select(m, Request{Categories: []string{"python"}, Size: 50})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, ex := range res.Examples {
		if seen[ex.ID] {
			t.Fatalf("duplicate example %s", ex.ID)
		}
		seen[ex.ID] = true
	}
}

func TestSelectSplitsAcrossCategories(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 60, "logic": 60})
	res, err := Select(m, Request{Categories: []string{"python", "logic"}, Size: 40})
	if err != nil {
		t.Fatal(err)
	}
	if res.ByCat["python"] == 0 || res.ByCat["logic"] == 0 {
		t.Fatalf("both categories should be represented: %v", res.ByCat)
	}
	if len(res.Examples) != 40 {
		t.Fatalf("expected 40 total, got %d", len(res.Examples))
	}
}

func TestSelectHonoursMix(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 100, "logic": 100})
	res, err := Select(m, Request{
		Categories: []string{"python", "logic"},
		Mix:        map[string]float64{"python": 0.75, "logic": 0.25},
		Size:       40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ByCat["python"] != 30 || res.ByCat["logic"] != 10 {
		t.Fatalf("expected 30/10, got %v", res.ByCat)
	}
}

func TestSelectReportsShortfall(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 10})
	res, err := Select(m, Request{Categories: []string{"python"}, Size: 75})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Examples) != 10 {
		t.Fatalf("expected 10, got %d", len(res.Examples))
	}
	if res.Shortfall != 65 {
		t.Fatalf("expected shortfall 65, got %d", res.Shortfall)
	}
	if len(res.Notes) == 0 {
		t.Fatal("expected a note about limited stock")
	}
}

func TestSelectRejectsUnknownCategory(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 5})
	if _, err := Select(m, Request{Categories: []string{"cobol"}, Size: 5}); err == nil {
		t.Fatal("expected unknown category to fail")
	}
}

func TestSelectRejectsBadInput(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 5})
	if _, err := Select(m, Request{Categories: []string{"python"}, Size: 0}); err == nil {
		t.Fatal("expected zero size to fail")
	}
	if _, err := Select(m, Request{Categories: nil, Size: 5}); err == nil {
		t.Fatal("expected empty categories to fail")
	}
}

func TestSelectSkipsStaleExamples(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 5})
	// Exhaust two examples.
	for i := 0; i < 3; i++ {
		m.MarkUsed([]string{"python-a", "python-b"}, nowUTC())
	}
	res, err := Select(m, Request{Categories: []string{"python"}, Size: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range res.Examples {
		if ex.ID == "python-a" || ex.ID == "python-b" {
			t.Fatalf("stale example %s was served", ex.ID)
		}
	}
	if len(res.Examples) != 3 {
		t.Fatalf("expected 3 usable, got %d", len(res.Examples))
	}
}

func TestSelectIsRandomised(t *testing.T) {
	m := bankWith(t, map[string]int{"python": 40})
	first, err := Select(m, Request{Categories: []string{"python"}, Size: 5})
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for attempt := 0; attempt < 8; attempt++ {
		next, err := Select(m, Request{Categories: []string{"python"}, Size: 5})
		if err != nil {
			t.Fatal(err)
		}
		if next.Examples[0].ID != first.Examples[0].ID {
			same = false
			break
		}
	}
	if same {
		t.Fatal("draw order looks deterministic")
	}
}

func nowUTC() time.Time { return time.Now().UTC() }
