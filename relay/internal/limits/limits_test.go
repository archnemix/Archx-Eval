package limits

import (
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/store"
)

func policy() Policy {
	return Policy{
		Window:        time.Hour,
		RateAllowance: 0.5, RateWindow: time.Minute, RateSample: time.Second, RateFloor: 10,
		BanWindow: 24 * time.Hour, MaxBan: 14 * 24 * time.Hour,
		BanBands: []BanBand{
			{Over: 1.0, Ban: 10 * time.Minute},
			{Over: 2.0, Ban: time.Hour},
			{Over: 3.0, Ban: 6 * time.Hour},
			{Over: 10.0, Ban: 24 * time.Hour},
			{Over: 50.0, Ban: 7 * 24 * time.Hour},
		},
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

// A tiny burst must never ban anyone: three requests in one second is a user
// clicking twice, not an attack. This is why RateFloor exists.
func TestSmallBurstIsNotPunished(t *testing.T) {
	now := time.Now()
	times := []time.Time{now, now.Add(-200 * time.Millisecond), now.Add(-400 * time.Millisecond)}
	if d := EvaluateVolume(policy(), times, now); !d.Allow {
		t.Fatalf("three requests must not ban: %+v", d)
	}
}

// The ban must be proportional: a modest overshoot loses minutes, a flood loses
// days. A single fixed ban would punish the first like an attacker.
func TestBanScalesWithExcess(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		perSec  int
		wantMin time.Duration
		wantMax time.Duration
	}{
		// 0.5/s allowance. 10 in one second = 10/s = 20x over -> 24h band.
		{"20x over", 10, 6 * time.Hour, 24 * time.Hour},
		// 100 in one second = 100/s = 200x over -> the 7 day band.
		{"200x over", 100, 7 * 24 * time.Hour, 7 * 24 * time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var times []time.Time
			for i := 0; i < c.perSec; i++ {
				times = append(times, now.Add(-time.Duration(i)*time.Millisecond))
			}
			d := EvaluateVolume(policy(), times, now)
			if d.Allow {
				t.Fatalf("%s should be banned", c.name)
			}
			if d.Ban < c.wantMin || d.Ban > c.wantMax {
				t.Fatalf("%s: ban %v outside [%v, %v] (over=%.1f)",
					c.name, d.Ban, c.wantMin, c.wantMax, d.Over)
			}
		})
	}
}

// Escalation is what stops "serve ten minutes, repeat for ever". A caller who
// has already been banned twice in the window gets a much longer ban for the
// same offence.
func TestRepeatOffenderEscalates(t *testing.T) {
	p := policy()
	now := time.Now()
	base := 10 * time.Minute

	first, prior := EvaluateHistory(p, nil, now, base)
	if first != base || prior != 0 {
		t.Fatalf("first offence: got %v after %d priors, want %v after 0", first, prior, base)
	}

	two := []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Hour)}
	third, prior := EvaluateHistory(p, two, now, base)
	if prior != 2 {
		t.Fatalf("expected 2 priors, got %d", prior)
	}
	if third != 4*base {
		t.Fatalf("third offence: got %v, want %v (2 doublings)", third, 4*base)
	}
}

// Bans that age out of the window stop counting, so a user who behaved badly
// months ago is not punished for life.
func TestOldBansStopCounting(t *testing.T) {
	p := policy()
	now := time.Now()
	old := []time.Time{now.Add(-72 * time.Hour), now.Add(-48 * time.Hour)}
	dur, prior := EvaluateHistory(p, old, now, 10*time.Minute)
	if prior != 0 || dur != 10*time.Minute {
		t.Fatalf("expired bans must not count: got %v after %d priors", dur, prior)
	}
}

// The cap must hold no matter how many prior bans pile up.
func TestHistoryCapsAtMaxBan(t *testing.T) {
	p := policy()
	now := time.Now()
	var many []time.Time
	for i := 0; i < 40; i++ {
		many = append(many, now.Add(-time.Duration(i)*time.Minute))
	}
	dur, _ := EvaluateHistory(p, many, now, 10*time.Minute)
	if dur > p.MaxBan {
		t.Fatalf("ban %v exceeded MaxBan %v", dur, p.MaxBan)
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

// ApplyBan records the ban AND the history that the escalation ladder reads
// next time. The history is the part that makes repeat offences cost more.
func TestApplyBanRecordsHistory(t *testing.T) {
	now := time.Now()
	rec := &store.Record{}
	ApplyBan(rec, Decision{Ban: 10 * time.Minute, Code: "SPAM_RATE"}, now)
	if got := rec.BannedUntil.Sub(now); got != 10*time.Minute {
		t.Fatalf("expected 10m, got %v", got)
	}
	if len(rec.BanHistory) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(rec.BanHistory))
	}
	ApplyBan(rec, Decision{Ban: 10 * time.Minute, Code: "SPAM_RATE"}, now.Add(time.Hour))
	if len(rec.BanHistory) != 2 || rec.BanCount != 2 {
		t.Fatalf("second ban not recorded: count=%d history=%d", rec.BanCount, len(rec.BanHistory))
	}
	// Both bans are inside the 24h window, so the ladder sees two priors and
	// doubles twice: this is the "serve the short ban and repeat" case that the
	// escalation exists to stop.
	dur, prior := EvaluateHistory(policy(), rec.BanHistory, now.Add(time.Hour), 10*time.Minute)
	if prior != 2 || dur != 40*time.Minute {
		t.Fatalf("after two priors: got %v after %d priors, want 40m after 2", dur, prior)
	}
}

// A ban history may not grow without bound; the slice is capped because an
// attacker who keeps tripping the ladder would otherwise grow the record for
// ever and slow every write of it.
func TestBanHistoryIsBounded(t *testing.T) {
	rec := &store.Record{}
	now := time.Now()
	for i := 0; i < 200; i++ {
		ApplyBan(rec, Decision{Ban: time.Minute, Code: "SPAM_RATE"}, now)
	}
	if len(rec.BanHistory) > 64 {
		t.Fatalf("history grew to %d entries", len(rec.BanHistory))
	}
}
