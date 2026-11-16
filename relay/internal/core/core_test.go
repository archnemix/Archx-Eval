package core

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/bank"
	"github.com/archnemix/archx-eval/relay/internal/config"
	"github.com/archnemix/archx-eval/relay/internal/identity"
	"github.com/archnemix/archx-eval/relay/internal/ratelog"
	"github.com/archnemix/archx-eval/relay/internal/reputation"
	"github.com/archnemix/archx-eval/relay/internal/store"
)

type harness struct {
	svc   *Service
	bankM *bank.Manager
	cfg   config.Config
	now   time.Time
	store *store.Store
	rates *ratelog.Log
}

func newHarness(t *testing.T, examplesPerCat int) *harness {
	t.Helper()
	dir := t.TempDir()
	bankDir := filepath.Join(dir, "bank")
	if err := os.MkdirAll(filepath.Join(bankDir, "python"), 0o750); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < examplesPerCat; i++ {
		fmt.Fprintf(&b, `{"id":"py-%03d","category":"python","difficulty":"easy","kind":"qa","prompt":"q%d","expected_answer":"a"}`+"\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(bankDir, "python", "easy.jsonl"), []byte(b.String()), 0o640); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		BankDir:        bankDir,
		DataDir:        filepath.Join(dir, "data"),
		RateDir:        filepath.Join(dir, "rate"),
		DrawMax:        75,
		StaleAfterUses: 3,
		Window:         time.Hour,
		RateAllowance:  0.5,
		RateWindow:     time.Minute,
		RateSample:     time.Second,
		// A floor keeps two rapid test requests from looking like a flood; the
		// production default is 20.
		RateFloor:      10,
		BanWindow:      24 * time.Hour,
		MaxBan:         14 * 24 * time.Hour,
		FeedbackWindow: 24 * time.Hour,
		LedgerKeepDays: 7,
		SaltSecret:     "test-salt",
		AllowLoopback:  true,
		AccessToken:    "test-access-token",
		NetWindow:      time.Hour,
		NetMaxDraws:    1000,
		GlobalWindow:   time.Hour,
		GlobalMax:      100000,
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	bm, err := bank.Load(bankDir, cfg.StaleAfterUses)
	if err != nil {
		t.Fatalf("bank: %v", err)
	}
	rep, err := reputation.Load("")
	if err != nil {
		t.Fatalf("reputation: %v", err)
	}
	rates, err := ratelog.Open(cfg.RateDir, 96*time.Hour)
	if err != nil {
		t.Fatalf("ratelog: %v", err)
	}
	t.Cleanup(func() { _ = rates.Close() })
	svc := New(cfg, st, bm, rep, rates)
	h := &harness{svc: svc, bankM: bm, cfg: cfg, now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC), store: st, rates: rates}
	svc.SetClock(func() time.Time { return h.now })
	rates.SetClock(func() time.Time { return h.now })
	return h
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

// reopen simulates a Space restart: fresh store, bank and service over the same
// directories, which is what the relay does on every wake.
func (h *harness) reopen() *harness {
	st, err := store.Open(h.cfg.DataDir)
	if err != nil {
		panic("reopen store: " + err.Error())
	}
	bm, err := bank.Load(h.cfg.BankDir, h.cfg.StaleAfterUses)
	if err != nil {
		panic("reopen bank: " + err.Error())
	}
	rep, _ := reputation.Load("")
	rates, err := ratelog.Open(h.cfg.RateDir, 96*time.Hour)
	if err != nil {
		panic("reopen ratelog: " + err.Error())
	}
	svc := New(h.cfg, st, bm, rep, rates)
	h2 := &harness{svc: svc, bankM: bm, cfg: h.cfg, now: h.now, store: st, rates: rates}
	svc.SetClock(func() time.Time { return h2.now })
	rates.SetClock(func() time.Time { return h2.now })
	return h2
}

func input(ip, machine, disk string) DrawInput {
	return DrawInput{
		RequestID:  "req-1",
		Categories: []string{"python"},
		Size:       10,
		Device: identity.Device{
			MachineID: machine,
			DiskUUID:  disk,
			OSFamily:  "linux",
			Arch:      "x86_64",
		},
		RemoteAddr: netip.MustParseAddr(ip),
		Headers:    map[string][]string{},
	}
}

const (
	machA = "4c4c4544-0044-3510-8054-b4c04f4e4e32"
	machB = "5d5d4544-0055-3510-8054-b4c04f4e4e33"
	diskA = "9f1b2c3d4e5f60718293a4b5c6d7e8f9"
	diskB = "aa2b3c4d5e6f708192a3b4c5d6e7f809"
)

func TestNewIdentityGetsDraw(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if len(g.Examples) != 10 {
		t.Fatalf("expected 10 examples, got %d", len(g.Examples))
	}
	if g.IdentityID == "" {
		t.Fatal("expected an identity id")
	}
	if !g.NextDrawAt.After(g.IssuedAt) {
		t.Fatal("next draw must be in the future")
	}
}

func TestSameIdentityBlockedForOneHour(t *testing.T) {
	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	h.advance(20 * time.Minute)
	_, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err == nil {
		t.Fatal("expected the second draw to be refused")
	}
	if !strings.Contains(err.Error(), "WINDOW_ACTIVE") {
		t.Fatalf("expected WINDOW_ACTIVE, got %v", err)
	}
}

func TestChangingOneMarkerStillMatches(t *testing.T) {
	// The whole point of marker-set matching: rotating the IP alone is not a
	// new person.
	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	h.advance(5 * time.Minute)
	_, err := h.svc.RequestDraw(context.Background(), input("198.51.100.99", machA, diskA))
	if err == nil {
		t.Fatal("same machine_id must still be the same person")
	}

	// Same story for the disk, on a fresh IP and machine id.
	h2 := newHarness(t, 50)
	if _, err := h2.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	h2.advance(5 * time.Minute)
	if _, err := h2.svc.RequestDraw(context.Background(), input("198.51.100.99", machB, diskA)); err == nil {
		t.Fatal("shared disk uuid must still match")
	}
}

func TestAllMarkersNewIsNewPerson(t *testing.T) {
	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	h.advance(5 * time.Minute)
	// Every marker changed: this counts as a genuinely new caller.
	if _, err := h.svc.RequestDraw(context.Background(), input("198.51.100.99", machB, diskB)); err != nil {
		t.Fatalf("fully rotated identity should be allowed: %v", err)
	}
}

func TestWindowExpiryAllowsSecondDraw(t *testing.T) {
	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("first: %v", err)
	}
	h.advance(61 * time.Minute)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("draw after window should succeed: %v", err)
	}
}

func TestSizeCapEnforced(t *testing.T) {
	h := newHarness(t, 200)
	in := input("203.0.113.10", machA, diskA)
	in.Size = 76
	if _, err := h.svc.RequestDraw(context.Background(), in); err == nil {
		t.Fatal("expected size above DrawMax to fail")
	}
	in.Size = 75
	if _, err := h.svc.RequestDraw(context.Background(), in); err != nil {
		t.Fatalf("75 should be allowed: %v", err)
	}
}

func TestBurstSpamBans(t *testing.T) {
	h := newHarness(t, 200)
	var lastErr error
	for i := 0; i < 20; i++ {
		in := input("203.0.113.10", machA, diskA)
		in.RequestID = fmt.Sprintf("req-%d", i)
		h.advance(200 * time.Millisecond)
		if _, err := h.svc.RequestDraw(context.Background(), in); err != nil {
			lastErr = err
			if strings.Contains(err.Error(), "SPAM") {
				break
			}
		}
	}
	if lastErr == nil || !strings.Contains(lastErr.Error(), "SPAM") {
		t.Fatalf("expected a spam ban, got %v", lastErr)
	}
}

// A ban is stamped with a real duration and enforced until it expires, not just
// for the length of the draw window. The old version of this test asserted a
// fixed multi-hour ban; bans are now proportional to the offence, so a modest
// overshoot earns minutes and the test checks the ban is actually in force
// rather than checking a particular number.
func TestBanEnforcedUntilItExpires(t *testing.T) {
	h := newHarness(t, 200)
	for i := 0; i < 20; i++ {
		in := input("203.0.113.10", machA, diskA)
		in.RequestID = fmt.Sprintf("r%d", i)
		h.advance(200 * time.Millisecond)
		h.svc.RequestDraw(context.Background(), in)
	}

	// Partway through the ban the caller is still refused, and the refusal
	// reports roughly the time left rather than the original duration.
	h.advance(5 * time.Minute)
	_, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err == nil {
		t.Fatal("expected a refusal during the ban")
	}
	if !strings.Contains(err.Error(), "SPAM_RATE") && !strings.Contains(err.Error(), "BANNED") {
		t.Fatalf("expected a ban refusal, got %v", err)
	}
}

// After the ban expires the caller is served again, which is what makes a short
// proportional ban acceptable: it is a delay, not a life sentence.
func TestShortBanExpiresAndAllows(t *testing.T) {
	h := newHarness(t, 200)
	for i := 0; i < 20; i++ {
		in := input("203.0.113.11", machA, diskA)
		in.RequestID = fmt.Sprintf("r%d", i)
		h.advance(200 * time.Millisecond)
		h.svc.RequestDraw(context.Background(), in)
	}
	// Past both the ban and the draw window.
	h.advance(2 * time.Hour)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.11", machA, diskA)); err != nil {
		t.Fatalf("an expired ban must stop blocking: %v", err)
	}
}

func TestStockShortfallReported(t *testing.T) {
	h := newHarness(t, 4)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	if len(g.Examples) != 4 {
		t.Fatalf("expected 4 examples, got %d", len(g.Examples))
	}
	if g.Shortfall != 6 {
		t.Fatalf("expected shortfall 6, got %d", g.Shortfall)
	}
}

func TestUnknownCategoryRejected(t *testing.T) {
	h := newHarness(t, 10)
	in := input("203.0.113.10", machA, diskA)
	in.Categories = []string{"cobol"}
	if _, err := h.svc.RequestDraw(context.Background(), in); err == nil {
		t.Fatal("expected unknown category to fail")
	}
}

func TestUsageCountedAndStaleAfterThree(t *testing.T) {
	h := newHarness(t, 1)
	identities := []struct{ ip, machine, disk string }{
		{"203.0.113.10", machA, diskA},
	}
	if _, err := h.svc.RequestDraw(context.Background(), input(identities[0].ip, identities[0].machine, identities[0].disk)); err != nil {
		t.Fatalf("first: %v", err)
	}
	if got := h.bankM.Uses("py-000"); got != 1 {
		t.Fatalf("expected 1 use, got %d", got)
	}
	// Two more distinct callers consume the remaining allowed uses.
	others := [][3]string{
		{"203.0.114.11", machB, diskB},
		{"203.0.115.12", "6e6e4544-0066-3510-8054-b4c04f4e4e34", "bb3b4c5d6e7f8091a2b3c4d5e6f70819"},
	}
	for _, o := range others {
		h.advance(2 * time.Hour)
		if _, err := h.svc.RequestDraw(context.Background(), input(o[0], o[1], o[2])); err != nil {
			t.Fatalf("draw for %s: %v", o[0], err)
		}
	}
	if got := h.bankM.Uses("py-000"); got != 3 {
		t.Fatalf("expected 3 uses, got %d", got)
	}
	if stale := h.svc.StaleExamples(); len(stale) != 1 || stale[0].ID != "py-000" {
		t.Fatalf("py-000 should be stale: %+v", stale)
	}
	// A further caller gets nothing: the only example is out of circulation.
	h.advance(2 * time.Hour)
	_, err := h.svc.RequestDraw(context.Background(), input("203.0.116.13", "7f7f4544-0077-3510-8054-b4c04f4e4e35", "cc4c5d6e7f8091a2b3c4d5e6f70819aa"))
	if err == nil {
		t.Fatal("expected no stock once every example is stale")
	}
}

func TestRespectsProxyHeadersForDifferentiation(t *testing.T) {
	h := newHarness(t, 50)
	in := input("10.0.0.1", machA, diskA)
	in.Headers = map[string][]string{identity.HeaderForwardedFor: {"203.0.113.50"}}
	in.RemoteAddr = netip.MustParseAddr("198.51.100.7")
	g, err := h.svc.RequestDraw(context.Background(), in)
	if err != nil {
		t.Fatalf("draw: %v", err)
	}
	// The record must be keyed on the socket address, not the forwarded header
	// (which the client controls), and it must contain no raw value at all.
	rec, ok := h.store.Get(g.IdentityID)
	if !ok {
		t.Fatal("expected a record")
	}
	wantIP := h.svc.Hash(identity.MarkerIP, "198.51.100.7")
	found := false
	for _, m := range rec.Markers {
		if m == wantIP {
			found = true
		}
	}
	if !found {
		t.Fatalf("record is not keyed on the socket address; markers=%v", rec.Markers)
	}
}

// Identity is "IP OR machine_id OR disk_uuid": any single marker must be enough
// to serve and to recognise the caller. Requiring both device fields locked out
// any runner that could only read one of them.
func TestAnySingleMarkerIsEnough(t *testing.T) {
	cases := []struct {
		name          string
		machine, disk string
		wantMarkers   int // 1 for IP alone, 3 for IP + both
	}{
		{"ip only", "", "", 1},
		{"machine only", machA, "", 2},
		{"disk only", "", diskA, 2},
		{"both", machA, diskA, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, 10)
			g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.20", c.machine, c.disk))
			if err != nil {
				t.Fatalf("draw with %s: %v", c.name, err)
			}
			rec, ok := h.store.Get(g.IdentityID)
			if !ok {
				t.Fatal("expected a record")
			}
			if len(rec.Markers) != c.wantMarkers {
				t.Fatalf("markers = %d, want %d: %v", len(rec.Markers), c.wantMarkers, rec.Markers)
			}
		})
	}
}

// An absent device field must contribute no marker at all. If it were hashed,
// every caller without that field would share one hash and collapse into a
// single identity, which would be both wrong and a trivial denial of service.
func TestAbsentFieldProducesNoMarker(t *testing.T) {
	h := newHarness(t, 10)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.21", "", ""))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := h.store.Get(g.IdentityID)
	empty := h.svc.Hash(identity.MarkerMachineID, "")
	for _, m := range rec.Markers {
		if m == empty {
			t.Fatal("an absent machine_id produced a marker; every such caller would share it")
		}
	}
}

func TestInvalidDeviceRejected(t *testing.T) {
	h := newHarness(t, 10)
	// Present but fabricated: refused rather than hashed into a marker that
	// would never match again.
	for _, bad := range []string{"unknown", "none", "00000000-0000-0000-0000-000000000000"} {
		if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", bad, diskA)); err == nil {
			t.Fatalf("expected placeholder machine id %q to be rejected", bad)
		}
	}
	// Too short to be a real identifier.
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", "abc", "")); err == nil {
		t.Fatal("expected an implausibly short machine id to be rejected")
	}
}

func TestReputationBlocksTor(t *testing.T) {
	h := newHarness(t, 10)
	dir := t.TempDir()
	path := filepath.Join(dir, "rep.json")
	body := `{"entries":[{"cidr":"203.0.113.0/24","tor":true}]}`
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	rep, err := reputation.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h.svc.rep = rep
	_, err = h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err == nil || !strings.Contains(err.Error(), "PRIVACY_BLOCKED") {
		t.Fatalf("expected tor exit to be blocked, got %v", err)
	}
}

func TestEventsRecorded(t *testing.T) {
	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(h.cfg.DataDir, "events"))
	if err != nil {
		t.Fatalf("events dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected an event log file")
	}
	buf, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "events", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf), `"decision":"allow"`) {
		t.Fatalf("expected an allow event, got %s", buf)
	}
}

// The strongest privacy guarantee the system can make: after a full request
// cycle, not one byte on disk contains the IP or the device identifiers.
func TestNoRawIdentityAnywhereOnDisk(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Flush(); err != nil {
		t.Fatal(err)
	}

	secrets := []string{"203.0.113.10", machA, diskA, machA[:8], diskA[:8]}
	err = filepath.WalkDir(h.cfg.DataDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, secret := range secrets {
			if strings.Contains(string(buf), secret) {
				t.Errorf("%s contains raw identity value %q", path, secret)
			}
		}
		// Also assert the shard tree has no raw subtree at all.
		if strings.Contains(filepath.ToSlash(path), "/raw/") {
			t.Errorf("unexpected raw subtree entry: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if _, ok := h.store.Get(g.IdentityID); !ok {
		t.Fatal("the hashed record should still exist")
	}
}

func TestHealthReportsBank(t *testing.T) {
	h := newHarness(t, 5)
	health := h.svc.Health()
	if !health.OK {
		t.Fatal("health should be OK with stock")
	}
	if health.Inventory.Total != 5 {
		t.Fatalf("expected 5 total, got %d", health.Inventory.Total)
	}
}

func TestPruneClearsBansButKeepsMemory(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := h.store.Get(g.IdentityID)
	rec.BannedUntil = h.now.Add(-time.Minute)
	rec.BanReason = "SPAM_BURST"
	if err := h.store.Put(rec); err != nil {
		t.Fatal(err)
	}

	cleared, err := h.svc.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if cleared != 1 {
		t.Fatalf("expected 1 ban cleared, got %d", cleared)
	}
	after, ok := h.store.Get(g.IdentityID)
	if !ok {
		t.Fatal("hashed record must survive pruning")
	}
	if !after.BannedUntil.IsZero() {
		t.Fatal("expired ban should be cleared")
	}
}

func shardOf(id string) string { return store.ShardOf(id) }
