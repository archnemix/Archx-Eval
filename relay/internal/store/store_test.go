package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s, dir
}

func TestShardIsStableAndShort(t *testing.T) {
	a := shard("abc")
	if a != shard("abc") {
		t.Fatal("shard must be deterministic")
	}
	if len(a) != 2 || !ValidShard(a) {
		t.Fatalf("bad shard %q", a)
	}
	if shard("abc") == shard("abd") && shard("abc") == shard("abe") {
		t.Fatal("collisions on adjacent keys are suspicious")
	}
}

func TestPutAndLookupByAnyMarker(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now().UTC()
	rec := &Record{
		ID: "id-1", Markers: []string{"m-ip", "m-mach", "m-disk"},
		FirstSeen: now, LastSeen: now, Requests: 1,
	}
	if err := s.Put(rec); err != nil {
		t.Fatalf("put: %v", err)
	}

	for _, marker := range []string{"m-ip", "m-mach", "m-disk"} {
		got, err := s.Lookup([]string{marker})
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if len(got) != 1 || got[0].ID != "id-1" {
			t.Fatalf("marker %s should match id-1, got %v", marker, got)
		}
	}

	got, _ := s.Lookup([]string{"m-ip", "m-unknown"})
	if len(got) != 1 {
		t.Fatalf("expected 1 deduplicated match, got %d", len(got))
	}
	if n, _ := s.Stats(); n != 1 {
		t.Fatalf("expected 1 record, got %d", n)
	}
}

func TestPutIsIdempotentOnMarkers(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now().UTC()
	rec := &Record{ID: "id-1", Markers: []string{"m-ip"}, FirstSeen: now, LastSeen: now}
	for i := 0; i < 3; i++ {
		if err := s.Put(rec); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.markers["m-ip"]) != 1 {
		t.Fatalf("marker index duplicated: %v", s.markers["m-ip"])
	}
}

func TestPersistenceSurvivesReopen(t *testing.T) {
	s, dir := newStore(t)
	now := time.Now().UTC()
	if err := s.Put(&Record{ID: "id-9", Markers: []string{"m-z"}, FirstSeen: now, LastSeen: now}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "identities", shard("id-9"), "id-9.json")); err != nil {
		t.Fatalf("identity file missing: %v", err)
	}

	// A fresh store re-reads the marker index from disk.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.Get("id-9"); !ok {
		t.Fatal("record should be recoverable after reopen")
	}
}

func TestAppendEventWritesJSONL(t *testing.T) {
	s, dir := newStore(t)
	ts := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	if err := s.AppendEvent(Event{TS: ts, IdentityID: "id-1", Decision: "deny", Reason: "window_active"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := s.AppendEvent(Event{TS: ts, IdentityID: "id-2", Decision: "allow", Reason: "granted"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	path := filepath.Join(dir, "events", "2026-11-13.jsonl")
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if n := countLines(string(buf)); n != 2 {
		t.Fatalf("expected 2 event lines, got %d", n)
	}
}

// There is no personal data to expire: retention is satisfied by never storing
// raw values in the first place.
func TestPruneOnlyClearsExpiredBans(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now().UTC()
	expired := &Record{
		ID: "id-1", Markers: []string{"h1"},
		FirstSeen: now.Add(-48 * time.Hour), LastSeen: now,
		BannedUntil: now.Add(-time.Hour), BanReason: "SPAM_BURST",
	}
	active := &Record{
		ID: "id-2", Markers: []string{"h2"},
		FirstSeen: now, LastSeen: now,
		BannedUntil: now.Add(time.Hour), BanReason: "SPAM_BURST",
	}
	for _, rec := range []*Record{expired, active} {
		if err := s.Put(rec); err != nil {
			t.Fatalf("put: %v", err)
		}
	}

	cleared, err := s.Prune(now)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("expected 1 expired ban cleared, got %d", cleared)
	}
	if got, _ := s.Get("id-1"); !got.BannedUntil.IsZero() {
		t.Fatal("expired ban should be cleared")
	}
	if got, _ := s.Get("id-2"); got.BannedUntil.IsZero() {
		t.Fatal("active ban must be preserved")
	}
	// Hashed records always survive: they are the memory of the system.
	if _, ok := s.Get("id-1"); !ok {
		t.Fatal("record must survive")
	}
}

func countLines(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}
