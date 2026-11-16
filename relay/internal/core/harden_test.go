package core

import (
	"context"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/identity"
)

// A subnet that trips its ceiling must stay banned after the request that
// tripped it. Before this, the ceiling lived only in the expiring rate log, so
// an attacker rotating addresses inside the same /24 got a clean slate mid
// attack: the one control that defends bank stock was the weakest one.
func TestSubnetBanPersists(t *testing.T) {
	h := newHarness(t, 200)
	h.cfg.NetMaxDraws = 1
	h.svc.cfg.NetMaxDraws = 1
	h.cfg.NetWindow = time.Hour
	h.svc.cfg.NetWindow = time.Hour

	// Two draws from the same /24: the second trips the ceiling.
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.40", machA, diskA)); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.41", machB, diskB)); err == nil {
		t.Fatal("expected the network ceiling to trip")
	}

	// A completely fresh identity in the same /24, with markers never seen
	// before, must now be refused on the stored ban alone.
	h.advance(2 * time.Minute)
	in := input("203.0.113.99", "ffffffff-1111-2222-3333-444455556666", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, err := h.svc.RequestDraw(context.Background(), in)
	if err == nil {
		t.Fatal("a stored subnet ban must refuse a brand-new identity in the same prefix")
	}
	if codeOf(err) != "NETWORK_BANNED" {
		t.Fatalf("expected NETWORK_BANNED, got %v (%s)", err, codeOf(err))
	}
}

// The subnet ban must survive a restart, or a Space sleep resets it.
func TestSubnetBanSurvivesRestart(t *testing.T) {
	h := newHarness(t, 200)
	h.cfg.NetMaxDraws, h.svc.cfg.NetMaxDraws = 1, 1
	h.cfg.NetWindow, h.svc.cfg.NetWindow = time.Hour, time.Hour

	h.svc.RequestDraw(context.Background(), input("198.51.100.10", machA, diskA))
	h.svc.RequestDraw(context.Background(), input("198.51.100.11", machB, diskB))

	h2 := h.reopen()
	in := input("198.51.100.77", "ffffffff-2222-3333-4444-555566667777", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if _, err := h2.svc.RequestDraw(context.Background(), in); codeOf(err) != "NETWORK_BANNED" {
		t.Fatalf("subnet ban did not survive restart: %v", err)
	}
}

// The volume ladder used to key on markers[0] only, which is the IP hash, so
// rotating the IP while keeping the same machine escaped a ban already earned.
// Any single marker must now be enough.
func TestBanFollowsAnyMarker(t *testing.T) {
	h := newHarness(t, 200)
	h.cfg.RateFloor, h.svc.cfg.RateFloor = 3, 3

	// Burn the ladder on machine A from one address.
	for i := 0; i < 12; i++ {
		in := input("203.0.113.50", machA, "")
		h.svc.RequestDraw(context.Background(), in)
	}

	// Same machine, new address: the ban must follow the machine marker.
	in := input("198.51.100.200", machA, "")
	_, err := h.svc.RequestDraw(context.Background(), in)
	if err == nil {
		t.Fatal("rotating the IP must not escape a ban earned on the same machine")
	}
}

// Malformed requests must be counted. A bot sending garbage bodies used to
// return before any counter was touched, so flooding cost the attacker nothing.
func TestGarbageRequestsAreCounted(t *testing.T) {
	h := newHarness(t, 200)
	h.cfg.RateFloor, h.svc.cfg.RateFloor = 3, 3

	// Size 0 is rejected by validation, i.e. after identity and counting.
	for i := 0; i < 12; i++ {
		in := input("203.0.113.60", machA, diskA)
		in.Size = 0
		h.svc.RequestDraw(context.Background(), in)
	}

	// Having been counted, the same caller is now refused by the ladder even
	// though every earlier request was invalid.
	in := input("203.0.113.60", machA, diskA)
	in.Size = 5
	if _, err := h.svc.RequestDraw(context.Background(), in); err == nil {
		t.Fatal("invalid requests were not counted towards the ladder")
	}
}

// A repeat offender must get a longer ban than a first offender for the same
// offence, which is the whole point of keeping ban history.
func TestRepeatBanIsLonger(t *testing.T) {
	h := newHarness(t, 200)
	h.cfg.RateFloor, h.svc.cfg.RateFloor = 3, 3

	var first time.Duration
	for round := 0; round < 2; round++ {
		for i := 0; i < 12; i++ {
			h.svc.RequestDraw(context.Background(), input("203.0.113.70", machA, diskA))
		}
		recs, err := h.store.Lookup([]string{h.svc.Hash(identity.MarkerMachineID, machA)})
		if err != nil || len(recs) == 0 {
			t.Fatalf("no record after round %d: %v", round, err)
		}
		ban := recs[0].BannedUntil.Sub(h.now)
		if round == 0 {
			first = ban
		} else if ban <= first {
			t.Fatalf("second ban (%v) was not longer than the first (%v)", ban, first)
		}
		// Wait out the ban, then come back and do it again.
		h.advance(ban + time.Second)
	}
}

// The ban history that drives escalation must not grow without bound: an
// attacker who keeps tripping the ladder would otherwise grow the record for
// ever, and every write of that record with it.
func TestBanHistoryStaysBounded(t *testing.T) {
	h := newHarness(t, 200)
	h.cfg.RateFloor, h.svc.cfg.RateFloor = 3, 3

	for round := 0; round < 80; round++ {
		for i := 0; i < 12; i++ {
			h.svc.RequestDraw(context.Background(), input("203.0.113.80", machA, diskA))
		}
		recs, err := h.store.Lookup([]string{h.svc.Hash(identity.MarkerMachineID, machA)})
		if err != nil || len(recs) == 0 {
			t.Fatalf("round %d: no record: %v", round, err)
		}
		if len(recs[0].BanHistory) > 64 {
			t.Fatalf("ban history grew to %d entries", len(recs[0].BanHistory))
		}
		h.advance(recs[0].BannedUntil.Sub(h.now) + time.Second)
	}
}
