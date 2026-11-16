// Package limits implements the clocks that protect the bank.
//
//  1. The draw window: one granted draw per identity per window (default 1h).
//     Matching ANY single marker is enough to recognise the caller, so a user
//     cannot reset their window by changing one attribute.
//
//  2. The volume ladder: request volume is measured as a RATE (peak requests
//     per second), and the ban that follows is proportional to how far past the
//     allowance the caller went. A polling loop that trips 1.5x loses ten
//     minutes; a flood at 100x loses days. A fixed ladder would punish the
//     first like an attacker and let the second off lightly.
//
//  3. The history ladder: the ban length also depends on how often this
//     identity has already been banned recently. Someone who takes the ten
//     minute ban and immediately repeats it is not the same as someone who
//     tripped a loop once, so repeat offences are discounted much faster than
//     the offence itself scales. The window matters: bans that age out of
//     BanWindow stop counting, so a well-behaved user is not punished for life.
//
// All of it is pure functions over a record snapshot plus an event history,
// which keeps the policy testable without touching storage.
package limits

import (
	"strings"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
	"github.com/archnemix/archx-eval/relay/internal/store"
)

// Policy is the configured set of thresholds. Every value is settable from the
// environment, because the first version of any rate limiter is wrong for real
// traffic and re-tuning must not require a redeploy.
type Policy struct {
	Window time.Duration

	// Volume ladder. Allowance is expressed per second so a burst and a steady
	// drip can be told apart: 180 requests spread over a minute is a polling
	// loop, 180 in two seconds is an attack. Same average, different intent.
	RateAllowance float64       // requests/second a caller may sustain
	RateWindow    time.Duration // span over which the peak is measured
	RateSample    time.Duration // bucket size for the peak
	// RateFloor is the minimum number of requests in RateWindow before the
	// ladder may fire at all. Without it, three requests arriving in the same
	// second look like 3/s against a 2/s allowance and a user who double-clicked
	// gets banned. Small bursts are normal; only sustained volume is an attack.
	RateFloor int

	// Ban length bands, indexed by how many times over the allowance the
	// caller went. The first band that fits wins.
	BanBands []BanBand

	// History ladder.
	BanWindow time.Duration // how far back bans are counted
	MaxBan    time.Duration // hard ceiling on any single ban
}

// BanBand maps an excess multiplier onto a ban duration.
type BanBand struct {
	Over float64
	Ban  time.Duration
}

// Decision records the outcome of an evaluation.
type Decision struct {
	Allow      bool
	Code       string
	Reason     string
	RetryAfter time.Duration
	Ban        time.Duration
	// Over is the measured excess multiplier, for logging and tests.
	Over float64
}

// EvaluateWindow decides whether this identity may be granted a draw now.
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

// EvaluateVolume measures the peak request rate over the recent window and
// returns a proportional ban once the caller sustains more than the allowance.
// times must be newest-last.
func EvaluateVolume(p Policy, times []time.Time, now time.Time) Decision {
	if p.RateAllowance <= 0 || len(times) == 0 {
		return Decision{Allow: true}
	}
	// Never punish a burst that is too small to be automation.
	if p.RateFloor > 0 && len(times) < p.RateFloor {
		return Decision{Allow: true}
	}
	peak := peakRate(times, now, p.RateWindow, p.RateSample)
	if peak <= 0 {
		return Decision{Allow: true}
	}
	over := peak / p.RateAllowance
	if over < 1 {
		return Decision{Allow: true, Over: over}
	}
	ban := banForOver(p, over)
	if ban <= 0 {
		return Decision{Allow: true, Over: over}
	}
	code := "SPAM_RATE"
	reason := "request rate is above the permitted rate"
	return Decision{
		Code:       code,
		Reason:     reason,
		RetryAfter: ban,
		Ban:        ban,
		Over:       over,
	}
}

// banForOver picks the band matching this excess. Bands are checked in
// descending order of Over so the most specific fit wins even if the caller
// supplied them unsorted.
func banForOver(p Policy, over float64) time.Duration {
	var best time.Duration
	bestOver := -1.0
	for _, b := range p.BanBands {
		if over >= b.Over && b.Over > bestOver {
			bestOver = b.Over
			best = b.Ban
		}
	}
	if bestOver < 0 {
		return 0
	}
	return best
}

// peakRate returns the highest requests-per-second rate seen in any sample
// bucket inside the window. Measuring the peak rather than the average is the
// point: it is what separates a burst from a drip.
func peakRate(times []time.Time, now time.Time, window, sample time.Duration) float64 {
	if window <= 0 || sample <= 0 {
		return 0
	}
	cutoff := now.Add(-window)
	buckets := make(map[int64]int, 16)
	span := int64(sample)
	for _, t := range times {
		if t.Before(cutoff) || t.After(now) {
			continue
		}
		b := now.Sub(t).Nanoseconds() / span
		buckets[b]++
	}
	best := 0
	for _, n := range buckets {
		if n > best {
			best = n
		}
	}
	if best == 0 {
		return 0
	}
	// Requests per second inside the hottest bucket.
	return float64(best) / (float64(sample) / float64(time.Second))
}

// EvaluateHistory scales a ban by how often this identity has been banned in
// the recent past. This is the rule that stops "take the ten minute ban, wait
// it out, do it again for ever": each repeat multiplies the ban, and the
// escalation resets only when the older bans age out of BanWindow.
func EvaluateHistory(p Policy, bands []time.Time, now time.Time, base time.Duration) (time.Duration, int) {
	if base <= 0 {
		return 0, 0
	}
	window := p.BanWindow
	if window <= 0 {
		window = 24 * time.Hour
	}
	cutoff := now.Add(-window)
	recent := 0
	for _, t := range bands {
		if !t.Before(cutoff) && !t.After(now) {
			recent++
		}
	}
	dur := base
	// First ban is served at face value; each prior ban in the window doubles
	// the next one. Doubling is deliberate: the ladder has to outrun the ban
	// duration itself, or a patient offender can simply keep coming back.
	for i := 0; i < recent; i++ {
		dur *= 2
		if dur > maxBan(p) {
			break
		}
	}
	if dur > maxBan(p) {
		dur = maxBan(p)
	}
	return dur, recent
}

func maxBan(p Policy) time.Duration {
	if p.MaxBan > 0 {
		return p.MaxBan
	}
	return 14 * 24 * time.Hour
}

// ApplyBan stamps a ban on a record. now is the decision time; the record keeps
// the ban history that EvaluateHistory reads next time.
func ApplyBan(rec *store.Record, d Decision, now time.Time) {
	if d.Ban <= 0 {
		return
	}
	rec.BanCount++
	rec.BannedUntil = now.Add(d.Ban)
	rec.BanReason = d.Code
	rec.BanHistory = append(rec.BanHistory, now)
	// Keep the history bounded; older entries can never be inside a BanWindow
	// once there are this many, and an unbounded slice would grow for ever.
	if len(rec.BanHistory) > 64 {
		rec.BanHistory = rec.BanHistory[len(rec.BanHistory)-64:]
	}
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

// BanForCode returns the base ban for a decision code, so a ban can be
// re-escalated from its original length rather than from whatever remains of
// the current one.
func BanForCode(p Policy, code string) time.Duration {
	switch {
	case code == "SPAM_RATE":
		// The minimum band is the right base: escalation multiplies from there.
		if len(p.BanBands) > 0 {
			return p.BanBands[0].Ban
		}
	case strings.HasPrefix(code, "NET"):
		return p.RateWindow
	}
	if len(p.BanBands) > 0 {
		return p.BanBands[0].Ban
	}
	return 10 * time.Minute
}
