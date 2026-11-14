package limits

import (
	"strings"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/store"
)

func policy() Policy {
	return Policy{
		Window:      time.Hour,
		SpamWindow:  time.Minute,
		SpamCount:   30,
		SpamBan:     48 * time.Hour,
		BurstWindow: 10 * time.Second,
		BurstCount:  15,
		BurstBan:    2 * time.Hour,
	}
}

func TestNewIdentityAllowed(t *testing.T) {
	d := EvaluateWindow(policy(), nil, time.Now())
	if !d.Allow {
		t.Fatal("new identity should be allowed")
	}
}

func TestWindowBlocksAndReportsRemaining(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rec := &store.Record{LastDraw: now.Add(-20 * time.Minute)}
	d := EvaluateWindow(policy(), rec, now)
	if d.Allow {
		t.Fatal("should be blocked inside the window")
	}
	if d.Code != "WINDOW_ACTIVE" {
		t.Fatalf("unexpected code %s", d.Code)
	}
	if d.RetryAfter != 40*time.Minute {
		t.Fatalf("expected 40m remaining, got %v", d.RetryAfter)
	}
}

func TestWindowElapsedAllows(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rec := &store.Record{LastDraw: now.Add(-61 * time.Minute)}
	if d := EvaluateWindow(policy(), rec, now); !d.Allow {
		t.Fatal("expired window should allow")
	}
}

func TestActiveBanBlocks(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rec := &store.Record{BannedUntil: now.Add(time.Hour), BanReason: "SPAM_BURST"}
	d := EvaluateWindow(policy(), rec, now)
	if d.Allow || d.Code != "IDENTITY_BANNED" {
		t.Fatalf("expected ban, got %+v", d)
	}
	if d.RetryAfter != time.Hour {
		t.Fatalf("expected 1h remaining, got %v", d.RetryAfter)
	}
}

func TestBurstBans(t *testing.T) {
	now := time.Now()
	var times []time.Time
	for i := 0; i < 15; i++ {
		times = append(times, now.Add(-time.Duration(i)*100*time.Millisecond))
	}
	d := EvaluateVolume(policy(), times, now)
	if d.Allow || d.Code != "SPAM_BURST" {
		t.Fatalf("expected burst ban, got %+v", d)
	}
	if d.Ban != 2*time.Hour {
		t.Fatalf("expected 2h ban, got %v", d.Ban)
	}
}

func TestSustainedSpamBansLonger(t *testing.T) {
	now := time.Now()
	var times []time.Time
	for i := 0; i < 30; i++ {
		times = append(times, now.Add(-time.Duration(i)*1500*time.Millisecond))
	}
	d := EvaluateVolume(policy(), times, now)
	if d.Allow || d.Code != "SPAM_SUSTAINED" {
		t.Fatalf("expected sustained spam ban, got %+v", d)
	}
	if d.Ban != 48*time.Hour {
		t.Fatalf("expected 48h ban, got %v", d.Ban)
	}
}

func TestModerateTrafficAllowed(t *testing.T) {
	now := time.Now()
	times := []time.Time{now.Add(-time.Minute), now.Add(-time.Hour)}
	if d := EvaluateVolume(policy(), times, now); !d.Allow {
		t.Fatalf("moderate traffic should pass: %+v", d)
	}
}

func TestOldRequestsFallOutOfWindow(t *testing.T) {
	now := time.Now()
	var times []time.Time
	for i := 0; i < 40; i++ {
		times = append(times, now.Add(-2*time.Hour))
	}
	if d := EvaluateVolume(policy(), times, now); !d.Allow {
		t.Fatal("stale requests must not count")
	}
}

func TestApplyBanEscalates(t *testing.T) {
	now := time.Now()
	rec := &store.Record{}
	d := Decision{Ban: 2 * time.Hour, Code: "SPAM_BURST"}

	ApplyBan(rec, d, now)
	if got := rec.BannedUntil.Sub(now); got != 2*time.Hour {
		t.Fatalf("first ban: expected 2h, got %v", got)
	}
	ApplyBan(rec, d, now)
	if got := rec.BannedUntil.Sub(now); got != 4*time.Hour {
		t.Fatalf("second ban: expected 4h, got %v", got)
	}
}

func TestApplyBanCapsAtTwoWeeks(t *testing.T) {
	now := time.Now()
	rec := &store.Record{BanCount: 50}
	ApplyBan(rec, Decision{Ban: 48 * time.Hour, Code: "SPAM_SUSTAINED"}, now)
	if got := rec.BannedUntil.Sub(now); got != 14*24*time.Hour {
		t.Fatalf("expected cap of 14 days, got %v", got)
	}
}

func TestRefuseMessageIsReadable(t *testing.T) {
	err := Refuse(Decision{Code: "WINDOW_ACTIVE", Reason: "wait", RetryAfter: 40 * time.Minute})
	if err.Status != 429 {
		t.Fatalf("expected 429, got %d", err.Status)
	}
	if !strings.Contains(err.Message, "40 minutes") {
		t.Fatalf("message should humanise the wait: %s", err.Message)
	}
}

func TestHumanise(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "30 seconds",
		1 * time.Minute:  "1 minute",
		5 * time.Minute:  "5 minutes",
		2 * time.Hour:    "2 hours",
		48 * time.Hour:   "2 days",
	}
	for in, want := range cases {
		if got := humanise(in); got != want {
			t.Fatalf("humanise(%v) = %q, want %q", in, got, want)
		}
	}
}
