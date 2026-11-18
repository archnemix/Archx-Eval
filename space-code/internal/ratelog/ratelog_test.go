package ratelog

import (
	"os"
	"testing"
	"time"
)

func openLog(t *testing.T, dir string) *Log {
	t.Helper()
	l, err := Open(dir, 48*time.Hour)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return l
}

func TestRecordAndCount(t *testing.T) {
	l := openLog(t, t.TempDir())
	defer l.Close()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := l.Record("k1", now.Add(-time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if got := l.Count("k1", time.Minute, now); got != 5 {
		t.Fatalf("expected 5, got %d", got)
	}
	if got := l.Count("k1", 2*time.Second, now); got != 3 {
		t.Fatalf("expected 3 inside 2s, got %d", got)
	}
	if got := l.Count("unknown", time.Minute, now); got != 0 {
		t.Fatalf("expected 0 for unknown key, got %d", got)
	}
}

// The entire point of this package: a restart must not forgive a flood.
func TestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	l := openLog(t, dir)
	for i := 0; i < 12; i++ {
		if err := l.Record("identity-abc", now.Add(-time.Duration(i)*100*time.Millisecond)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	_ = l.Flush()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Simulate a Hugging Face Space waking up with a brand new process.
	again := openLog(t, dir)
	defer again.Close()
	if got := again.Count("identity-abc", time.Minute, now); got != 12 {
		t.Fatalf("expected 12 events to survive restart, got %d", got)
	}
}

func TestTornLineDoesNotPreventStartup(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir)
	if err := l.Record("k", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = l.Flush()
	_ = l.Close()

	// Append a half-written line, exactly as a kill -9 mid-write leaves.
	f, err := os.OpenFile(l.PathFor("k"), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"t":123`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	again, err := Open(dir, 48*time.Hour)
	if err != nil {
		t.Fatalf("torn line must not block startup: %v", err)
	}
	defer again.Close()
	if got := again.Count("k", time.Hour, time.Now().UTC()); got != 1 {
		t.Fatalf("expected the intact event to load, got %d", got)
	}
}

func TestRetentionDropsOldEntriesOnLoad(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	l := openLog(t, dir)
	if err := l.Record("old", now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := l.Record("new", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = l.Flush()
	_ = l.Close()

	again := openLog(t, dir) // retention 48h
	defer again.Close()
	if got := again.Count("old", 96*time.Hour, now); got != 0 {
		t.Fatalf("expired entry should be dropped, got %d", got)
	}
	if got := again.Count("new", 96*time.Hour, now); got != 1 {
		t.Fatalf("recent entry should survive, got %d", got)
	}
}

func TestCompactShrinksFiles(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir)
	defer l.Close()
	now := time.Now().UTC()

	for i := 0; i < 10; i++ {
		if err := l.Record("k", now.Add(-50*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Record("k", now); err != nil {
		t.Fatal(err)
	}
	n, err := l.Compact(now)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 file rewritten, got %d", n)
	}
	if got := l.Count("k", 96*time.Hour, now); got != 1 {
		t.Fatalf("expected only the fresh entry, got %d", got)
	}
}

func TestCompactRemovesEmptyKey(t *testing.T) {
	dir := t.TempDir()
	l := openLog(t, dir)
	defer l.Close()
	now := time.Now().UTC()
	if err := l.Record("gone", now.Add(-49*time.Hour)); err != nil {
		t.Fatal(err)
	}
	path := l.PathFor("gone")
	if _, err := l.Compact(now); err != nil {
		t.Fatal(err)
	}
	if l.Keys() != 0 {
		t.Fatalf("expected no keys left, got %d", l.Keys())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file should be removed")
	}
}

func TestMaxEntriesBound(t *testing.T) {
	l := openLog(t, t.TempDir())
	defer l.Close()
	now := time.Now().UTC()
	for i := 0; i < maxEntriesPerKey+200; i++ {
		if err := l.Record("flood", now); err != nil {
			t.Fatal(err)
		}
	}
	l.mu.Lock()
	n := len(l.entries["flood"])
	l.mu.Unlock()
	if n > maxEntriesPerKey {
		t.Fatalf("memory must stay bounded, got %d entries", n)
	}
}

func TestCountPrefix(t *testing.T) {
	l := openLog(t, t.TempDir())
	defer l.Close()
	now := time.Now().UTC()
	for _, k := range []string{"net:a", "net:b", "other"} {
		for i := 0; i < 3; i++ {
			if err := l.Record(k, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := l.CountPrefix("net:", time.Minute, now); got != 6 {
		t.Fatalf("expected 6, got %d", got)
	}
}

func TestConcurrentRecordAndCount(t *testing.T) {
	l := openLog(t, t.TempDir())
	defer l.Close()
	now := time.Now().UTC()
	done := make(chan struct{})
	for w := 0; w < 16; w++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < 40; i++ {
				_ = l.Record("shared", now)
				_ = l.Count("shared", time.Minute, now)
			}
		}()
	}
	for w := 0; w < 16; w++ {
		<-done
	}
	l.mu.Lock()
	n := len(l.entries["shared"])
	l.mu.Unlock()
	if n == 0 {
		t.Fatal("concurrent writes lost every event")
	}
}

func TestRejectsEmptyKey(t *testing.T) {
	l := openLog(t, t.TempDir())
	defer l.Close()
	if err := l.Record("", time.Now()); err == nil {
		t.Fatal("expected empty key to be rejected")
	}
}

// The retention cutoff is applied while loading, so the clock used at Open
// decides whether history survives. Reopening on the wall clock while history
// was written on another clock silently drops everything -- which is exactly
// what happened once the calendar moved past a frozen test date.
func TestOpenAtUsesTheSuppliedClockForRetention(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()

	l, err := OpenAt(dir, 48*time.Hour, func() time.Time { return base })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := l.Record("id:abc", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	// Same clock, an hour later: history is well inside retention and must load.
	again, err := OpenAt(dir, 48*time.Hour, func() time.Time { return base.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	// The window must span the events: they were written at base, and the
	// clock has moved on an hour.
	if got := again.Count("id:abc", 2*time.Hour, base.Add(time.Hour)); got != 5 {
		t.Fatalf("expected all 5 events to reload, got %d", got)
	}
	_ = again.Close()

	// Far ahead of the entries: they are older than retention and are dropped.
	// This is the failure mode, asserted so it can never come back by accident.
	stale, err := OpenAt(dir, 48*time.Hour, func() time.Time { return base.Add(100 * time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	if got := stale.Count("id:abc", 200*time.Hour, base.Add(100*time.Hour)); got != 0 {
		t.Fatalf("expected expired events to be dropped, got %d", got)
	}
	_ = stale.Close()
}
