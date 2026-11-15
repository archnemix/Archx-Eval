// Package limits implements the two clocks that protect the bank.
//
//  1. The draw window: one granted draw per identity per window (default 1h).
//     Matching ANY single marker is enough to recognise the caller, so a user
//     cannot reset their window by changing one attribute.
//
//  2. The spam ladder: sustained request volume past the refusal point bans
//     the identity outright instead of politely asking it to wait.
//
// Both are pure functions over a record snapshot plus an event history, which
// keeps the policy testable without touching storage.
package limits

import (
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
	"github.com/archnemix/archx-eval/relay/internal/store"
)

// Policy is the configured set of thresholds.
type Policy struct {
	Window time.Duration

	SpamWindow time.Duration
	SpamCount  int
	SpamBan    time.Duration

	BurstWindow time.Duration
	BurstCount  int
	BurstBan    time.Duration
}

// Decision records the outcome of an evaluation.
type Decision struct {
	Allow      bool
	Code       string
	Reason     string
	RetryAfter time.Duration
	Ban        time.Duration
}

// EvaluateWindow decides whether this identity may be granted a draw now.
// matched is true when the identity was recognised from a stored marker.
func EvaluateWindow(p Policy, rec *store.Record, now time.Time) Decision {
	if rec == nil {
		return Decision{Allow: true, Reason: "new_identity"}
	}
	if rec.Banned(now) {
		return Decision{
			Code:       "IDENTITY_BANNED",
			Reason:     rec.BanReason,
			RetryAfter: rec.BannedUntil.Sub(now),
		}
	}
	if rec.LastDraw.IsZero() {
		return Decision{Allow: true, Reason: "no_prior_draw"}
	}
	elapsed := now.Sub(rec.LastDraw)
	if elapsed >= p.Window {
		return Decision{Allow: true, Reason: "window_elapsed"}
	}
	return Decision{
		Code:       "WINDOW_ACTIVE",
		Reason:     "a draw was already served inside the current window",
		RetryAfter: p.Window - elapsed,
	}
}

// EvaluateVolume inspects recent request timestamps and returns a ban when the
// caller has crossed a spam threshold. times must be newest-last.
func EvaluateVolume(p Policy, times []time.Time, now time.Time) Decision {
	inBurst := countWithin(times, now, p.BurstWindow)
	if p.BurstCount > 0 && inBurst >= p.BurstCount {
		return Decision{
			Code:       "SPAM_BURST",
			Reason:     "request rate far above the permitted rate",
			RetryAfter: p.BurstBan,
			Ban:        p.BurstBan,
		}
	}
	inSpam := countWithin(times, now, p.SpamWindow)
	if p.SpamCount > 0 && inSpam >= p.SpamCount {
		return Decision{
			Code:       "SPAM_SUSTAINED",
			Reason:     "too many requests in the spam window",
			RetryAfter: p.SpamBan,
			Ban:        p.SpamBan,
		}
	}
	return Decision{Allow: true}
}

func countWithin(times []time.Time, now time.Time, window time.Duration) int {
	if window <= 0 {
		return 0
	}
	cutoff := now.Add(-window)
	n := 0
	for _, t := range times {
		if !t.Before(cutoff) {
			n++
		}
	}
	return n
}

// ApplyBan stamps a ban on a record, escalating repeat offenders.
func ApplyBan(rec *store.Record, d Decision, now time.Time) {
	if d.Ban <= 0 {
		return
	}
	rec.BanCount++
	mult := time.Duration(1)
	if rec.BanCount > 1 {
		// Second strike doubles, third triples, capped at two weeks.
		mult = time.Duration(minInt(rec.BanCount, 7))
	}
	dur := d.Ban * mult
	if dur > 14*24*time.Hour {
		dur = 14 * 24 * time.Hour
	}
	rec.BannedUntil = now.Add(dur)
	rec.BanReason = d.Code
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Refuse builds the API error for a decision.
func Refuse(d Decision) *apierr.Error {
	return apierr.Limited(d.Code, d.Reason+" — retry in "+humanise(d.RetryAfter), d.RetryAfter)
}

// humanise renders a duration the way a user-facing message should.
func humanise(d time.Duration) string {
	if d < time.Minute {
		return plural(int(d.Round(time.Second)/time.Second), "second")
	}
	if d < time.Hour {
		return plural(int(d.Round(time.Minute)/time.Minute), "minute")
	}
	if d < 48*time.Hour {
		return plural(int(d.Round(time.Hour)/time.Hour), "hour")
	}
	return plural(int(d.Round(24*time.Hour)/(24*time.Hour)), "day")
}

func plural(n int, unit string) string {
	if n <= 0 {
		n = 1
	}
	s := "s"
	if n == 1 {
		s = ""
	}
	return itoa(n) + " " + unit + s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
