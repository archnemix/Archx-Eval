package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/bank"
	"github.com/archnemix/archx-eval/relay/internal/ratelog"
	"github.com/archnemix/archx-eval/relay/internal/reputation"
	"github.com/archnemix/archx-eval/relay/internal/store"
)

// reboot simulates a Hugging Face Space restart: brand new Service, Store and
// rate log objects built from the same on-disk state. Nothing persists in
// memory, exactly as when the container is stopped and started again.
func (h *harness) reboot(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(h.cfg.DataDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	bm, err := bank.Load(h.cfg.BankDir, h.cfg.StaleAfterUses)
	if err != nil {
		t.Fatalf("reload bank: %v", err)
	}
	rep, _ := reputation.Load("")
	// Opened on the harness clock: load applies the retention cutoff, and the
	// harness date is frozen, so the wall clock would discard everything.
	now := h.now
	rates, err := ratelog.OpenAt(h.cfg.RateDir, 96*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatalf("reopen ratelog: %v", err)
	}
	t.Cleanup(func() { _ = rates.Close() })

	svc := New(h.cfg, st, bm, rep, rates)
	next := &harness{svc: svc, bankM: bm, cfg: h.cfg, now: h.now, store: st, rates: rates}
	svc.SetClock(func() time.Time { return next.now })
	rates.SetClock(func() time.Time { return next.now })
	return next
}

// The headline fix: a flusher must not be able to reset their own ban by
// forcing the Space to restart.
func TestBurstBanSurvivesRestart(t *testing.T) {
	h := newHarness(t, 200)

	var banned bool
	for i := 0; i < 40; i++ {
		in := input("203.0.113.10", machA, diskA)
		in.RequestID = fmt.Sprintf("r%d", i)
		h.advance(100 * time.Millisecond)
		if _, err := h.svc.RequestDraw(context.Background(), in); err != nil &&
			strings.Contains(err.Error(), "SPAM") {
			banned = true
			break
		}
	}
	if !banned {
		t.Fatal("expected the burst limit to trip before restart")
	}

	// Restart immediately: the ban must still be in force.
	after := h.reboot(t)
	_, err := after.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err == nil {
		t.Fatal("a restart must not clear the ban")
	}
	if !strings.Contains(err.Error(), "BANNED") && !strings.Contains(err.Error(), "SPAM") {
		t.Fatalf("expected still banned, got %v", err)
	}
}

// Even before a ban is stamped on the record, the request history itself must
// survive, so the ladder continues counting from where it left off.
func TestRateHistorySurvivesRestart(t *testing.T) {
	h := newHarness(t, 200)
	for i := 0; i < 10; i++ {
		in := input("203.0.113.10", machA, diskA)
		in.RequestID = fmt.Sprintf("r%d", i)
		h.advance(200 * time.Millisecond)
		_, _ = h.svc.RequestDraw(context.Background(), in)
	}
	before := h.rates.Count("id:"+h.svc.Hash("ip", "203.0.113.10"), time.Minute, h.now)
	if before < 10 {
		t.Fatalf("expected >=10 events recorded, got %d", before)
	}

	after := h.reboot(t)
	got := after.rates.Count("id:"+after.svc.Hash("ip", "203.0.113.10"), time.Minute, after.now)
	if got < before {
		t.Fatalf("restart lost rate history: before=%d after=%d", before, got)
	}
}

// The 1-hour draw window must also survive, since it lives in the record.
func TestDrawWindowSurvivesRestart(t *testing.T) {
	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	after := h.reboot(t)
	after.advance(5 * time.Minute)
	if _, err := after.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err == nil {
		t.Fatal("window must still be active after a restart")
	}
}

func TestUsageCountersSurviveRestart(t *testing.T) {
	h := newHarness(t, 50)
	grant, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	if len(grant.Examples) == 0 {
		t.Fatal("expected examples")
	}
	issued := grant.Examples[0].ID
	if err := h.svc.Flush(); err != nil {
		t.Fatal(err)
	}

	after := h.reboot(t)
	if got := after.bankM.Uses(issued); got != 1 {
		t.Fatalf("use counter for %s should survive, got %d", issued, got)
	}
}

func TestAccessTokenRequired(t *testing.T) {
	h := newHarness(t, 20)
	if err := h.svc.Authenticate("wrong"); err == nil {
		t.Fatal("wrong token must be rejected")
	}
	if err := h.svc.Authenticate(""); err == nil {
		t.Fatal("empty token must be rejected")
	}
	if err := h.svc.Authenticate("test-access-token"); err != nil {
		t.Fatalf("correct token rejected: %v", err)
	}
}

func TestNoTokenConfiguredMeansClosed(t *testing.T) {
	h := newHarness(t, 20)
	h.svc.cfg.AccessToken = ""
	if err := h.svc.Authenticate("anything"); err == nil {
		t.Fatal("a relay with no token configured must refuse draws, not allow them")
	}
}

func TestNetworkCeiling(t *testing.T) {
	h := newHarness(t, 200)
	h.svc.cfg.NetWindow = time.Hour
	h.svc.cfg.NetMaxDraws = 3

	allowed := 0
	for i := 0; i < 12; i++ {
		// Distinct identities on the same /24: the per-identity ladder never
		// trips, so only the subnet ceiling can stop this.
		ip := fmt.Sprintf("203.0.113.%d", i+10)
		in := input(ip, fmt.Sprintf("machine-%016d", i), fmt.Sprintf("disk-%016d", i))
		in.Size = 1
		h.advance(30 * time.Second)
		if _, err := h.svc.RequestDraw(context.Background(), in); err != nil {
			if strings.Contains(err.Error(), "NETWORK_CEILING") {
				break
			}
			t.Fatalf("unexpected error: %v", err)
		}
		allowed++
	}
	if allowed > 5 {
		t.Fatalf("subnet ceiling should have stopped this, allowed=%d", allowed)
	}
}

func TestGlobalCeiling(t *testing.T) {
	h := newHarness(t, 200)
	h.svc.cfg.GlobalWindow = time.Hour
	h.svc.cfg.GlobalMax = 2
	h.svc.cfg.NetMaxDraws = 100000

	stopped := false
	for i := 0; i < 12; i++ {
		in := input(fmt.Sprintf("198.51.%d.%d", i, i+1),
			fmt.Sprintf("machine-%016d", i), fmt.Sprintf("disk-%016d", i))
		in.Size = 1
		h.advance(30 * time.Second)
		if _, err := h.svc.RequestDraw(context.Background(), in); err != nil {
			if strings.Contains(err.Error(), "GLOBAL_CEILING") {
				stopped = true
				break
			}
		}
	}
	if !stopped {
		t.Fatal("global ceiling never triggered")
	}
}

func TestRateLogDirectoryIsSeparate(t *testing.T) {
	h := newHarness(t, 20)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatal(err)
	}
	// The mirrored dataset tree must not accidentally include rate state.
	if filepath.Dir(h.cfg.RateDir) != filepath.Dir(h.cfg.DataDir) {
		t.Fatalf("rate dir should sit beside the data dir, got %s", h.cfg.RateDir)
	}
	if strings.HasPrefix(h.cfg.RateDir, h.cfg.DataDir) {
		t.Fatal("rate dir must not be inside the exported data tree")
	}
}
