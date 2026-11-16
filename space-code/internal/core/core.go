// Package core is the relay's decision engine: it wires identity, storage,
// limits and the bank together into the two operations the API exposes.
//
// It knows nothing about HTTP. That keeps the policy testable in isolation and
// means the transport can change without touching a single rule.
package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
	"github.com/archnemix/archx-eval/relay/internal/bank"
	"github.com/archnemix/archx-eval/relay/internal/config"
	"github.com/archnemix/archx-eval/relay/internal/draw"
	"github.com/archnemix/archx-eval/relay/internal/identity"
	"github.com/archnemix/archx-eval/relay/internal/limits"
	"github.com/archnemix/archx-eval/relay/internal/ratelog"
	"github.com/archnemix/archx-eval/relay/internal/reputation"
	"github.com/archnemix/archx-eval/relay/internal/store"
	"github.com/archnemix/archx-eval/relay/internal/syncctl"
)

// Service is the relay core.
type Service struct {
	cfg   config.Config
	store *store.Store
	bank  *bank.Manager
	rep   *reputation.Table
	pol   limits.Policy
	salt  []byte

	// sync queues state changes for the dataset. It is optional: when nil the
	// relay runs in local-only mode. Even when present it never blocks a
	// request, because Enqueue is an in-memory operation.
	sync *syncctl.Controller

	// rates is the durable sliding-window log behind the spam ladder and the
	// network ceilings. It survives a process restart, which matters because
	// a Hugging Face Space stops and restarts constantly.
	rates *ratelog.Log

	now func() time.Time
}

// New builds a Service. Callers own the returned value's lifecycle.
func New(cfg config.Config, st *store.Store, bm *bank.Manager, rep *reputation.Table, rates *ratelog.Log) *Service {
	return &Service{
		cfg:   cfg,
		store: st,
		bank:  bm,
		rep:   rep,
		salt:  cfg.Salt(),
		rates: rates,
		now:   func() time.Time { return time.Now().UTC() },
		pol: limits.Policy{
			Window:        cfg.Window,
			RateAllowance: cfg.RateAllowance,
			RateWindow:    cfg.RateWindow,
			RateSample:    cfg.RateSample,
			RateFloor:     cfg.RateFloor,
			BanWindow:     cfg.BanWindow,
			MaxBan:        cfg.MaxBan,
			// Bands are ordered by severity; the first that fits wins. They are
			// deliberately generous at the bottom so a polling loop that trips
			// 1.5x loses minutes, not days.
			BanBands: []limits.BanBand{
				{Over: 1.0, Ban: 10 * time.Minute},
				{Over: 2.0, Ban: 1 * time.Hour},
				{Over: 3.0, Ban: 6 * time.Hour},
				{Over: 10.0, Ban: 24 * time.Hour},
				{Over: 50.0, Ban: 7 * 24 * time.Hour},
			},
		},
	}
}

// WithSync attaches the dataset write controller.
func (s *Service) WithSync(c *syncctl.Controller) *Service {
	s.sync = c
	return s
}

// SyncStats reports controller health, or nil when sync is disabled.
func (s *Service) SyncStats() *syncctl.Stats {
	if s.sync == nil {
		return nil
	}
	st := s.sync.Stats()
	return &st
}

// SetClock overrides the clock (tests only).
func (s *Service) SetClock(f func() time.Time) { s.now = f }

// DrawInput is the caller-supplied half of a draw request.
type DrawInput struct {
	// Admitted is set by Admit. When false, RequestDraw records the IP marker
	// itself, so a caller that skips the HTTP layer still gets accurate
	// counting instead of silently losing every event.
	Admitted   bool
	RequestID  string
	Categories []string
	Mix        map[string]float64
	Size       int
	Device     identity.Device
	RemoteAddr netip.Addr
	Headers    map[string][]string
}

// Grant is a successful draw.
type Grant struct {
	RequestID  string
	IdentityID string
	Examples   []*bank.Example
	ByCat      map[string]int
	IssuedAt   time.Time
	NextDrawAt time.Time
	Shortfall  int
	Notes      []string
}

// Hash implements identity.HashFunc with a salted HMAC-SHA256, truncated to
// 32 hex characters. The truncation keeps marker files short while leaving
// 128 bits of collision resistance, which is plenty for this corpus.
func (s *Service) Hash(kind, value string) string {
	mac := hmac.New(sha256.New, s.salt)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

// RequestDraw runs the full admission pipeline.
//
// Order is deliberate and cheap-first:
//  1. identity validation (free)
//  2. VPN/privacy refusal (free)
//  3. spam ladder (memory only)
//  4. marker lookup (a handful of file reads)
//  5. draw window (uses the record we already fetched)
//  6. stock check (must happen before we record anything)
//  7. persist usage and only then hand over examples
func (s *Service) RequestDraw(ctx context.Context, in DrawInput) (*Grant, error) {
	now := s.now()

	// The request id is the correlation handle between a draw and its results,
	// so it must never be empty. Trust the caller's if it supplied one, and
	// mint one if it did not: a draw without an id could never have its
	// feedback verified, which would quietly make the ledger useless.
	if in.RequestID == "" {
		in.RequestID = newID()
	}

	// Identity, counting and the volume ladder run FIRST, before any shape
	// check. Previously a bot could send malformed bodies all day and never be
	// counted, because every cheap rejection returned before the counters were
	// touched. A refusal must cost the caller something, or flooding is free.
	id, err := identity.FromRequest(in.RemoteAddr, in.Headers, in.Device, s.Hash, s.cfg.AllowLoopback)
	if err != nil {
		return nil, err
	}
	markerHashes := make([]string, 0, len(id.Markers))
	for _, m := range id.Markers {
		markerHashes = append(markerHashes, m.Hash)
	}

	// Network ban first: it is the cheapest check and the one that survives an
	// attacker rotating their IP.
	// The ban key must be the SAME key the rate log and the ban writer use,
	// otherwise the stored ban is invisible to the check: the subnet ban did
	// nothing at all until this was fixed.
	netHash := s.netKey(id.Network)
	if d := s.subnetBanDecision(netHash, now); !d.Allow {
		s.record(ctx, in.RequestID, "", "deny", d.Code, id, int(d.RetryAfter.Seconds()))
		return nil, limits.Refuse(d)
	}

	// The volume ladder is evaluated for EVERY marker, not just the first. The
	// ban key used to be markers[0], which is the IP hash, so rotating the IP
	// while keeping the same machine escaped a ban that was already earned.
	if d, banRec := s.evaluateVolumeForMarkers(markerHashes, now); !d.Allow {
		if banRec != nil {
			// Escalate from the minimum band, then multiply by the number of
			// bans this identity has already collected in the window. Without
			// this the ladder restarts at the floor every time a ban expires,
			// so a caller could serve a short ban and repeat for ever.
			if base := limits.BanForCode(s.pol, d.Code); base > 0 {
				if escalated, _ := limits.EvaluateHistory(s.pol, banRec.BanHistory, now, base); escalated > 0 {
					d.Ban = escalated
					d.RetryAfter = escalated
				}
			}
			limits.ApplyBan(banRec, d, now)
			if err := s.store.Put(banRec); err != nil {
				return nil, apierr.Internal("could not record the ban")
			}
			s.publishIdentity(banRec)
		}
		s.record(ctx, in.RequestID, idOf(banRec), "ban", d.Code, id, int(d.RetryAfter.Seconds()))
		return nil, limits.Refuse(d)
	}
	// The IP marker is already counted when Admit ran before body parsing; the
	// device markers are new either way. Network and global counters are
	// recorded only here, because a valid draw is what consumes bank stock.
	start := 0
	if in.Admitted {
		start = 1
	}
	for _, h := range markerHashes[start:] {
		if err := s.rates.Record("id:"+h, now); err != nil {
			fmt.Fprintf(os.Stderr, "warn: rate event not persisted: %v\n", err)
		}
	}
	if err := s.rates.Record(s.netKey(id.Network), now); err != nil {
		fmt.Fprintf(os.Stderr, "warn: network rate event not persisted: %v\n", err)
	}
	if err := s.rates.Record(globalKey, now); err != nil {
		fmt.Fprintf(os.Stderr, "warn: global rate event not persisted: %v\n", err)
	}

	// Only now are shape errors able to return, and they have already been
	// counted against the caller.
	if err := draw.ValidateMix(in.Mix); err != nil {
		return nil, apierr.BadRequest(err.Error())
	}
	size := in.Size
	if size == 0 {
		size = s.cfg.DrawMax
		return nil, apierr.BadRequest("size is required")
	}
	if size < 0 {
		return nil, apierr.BadRequest("size must be positive")
	}
	if size > s.cfg.DrawMax {
		return nil, apierr.BadRequest(fmt.Sprintf("size may not exceed %d", s.cfg.DrawMax))
	}
	// Bound the request shape itself: an unbounded category list or mix map is
	// cheap to send and expensive to process.
	if len(in.Categories) > 32 {
		return nil, apierr.BadRequest("at most 32 categories may be requested")
	}
	if len(in.Mix) > 32 {
		return nil, apierr.BadRequest("at most 32 mix weights may be supplied")
	}

	if s.rep.Loaded() {
		if entry, ok := s.rep.Lookup(id.IP); ok {
			if entry.Tor {
				s.record(ctx, in.RequestID, "", "deny", "TOR_EXIT", id, 0)
				return nil, apierr.Forbidden("PRIVACY_BLOCKED", "requests via Tor are not accepted")
			}
			id.Privacy = identity.SoftFlagsFromIP(id.Privacy, entry.Hosting || entry.VPN, entry.Abuser)
		} else if identity.LooksHosted(id.IP) {
			id.Privacy = identity.SoftFlagsFromIP(id.Privacy, true, 0)
		}
	} else if identity.LooksHosted(id.IP) {
		id.Privacy = identity.SoftFlagsFromIP(id.Privacy, true, 0)
	}
	if id.Privacy.Blocked() {
		s.record(ctx, in.RequestID, "", "deny", "PRIVACY_BLOCKED", id, 0)
		return nil, apierr.Forbidden("PRIVACY_BLOCKED",
			"this request appears to come from a masked network and is not accepted")
	}

	// Network and global ceilings: a single subnet, or the service as a whole,
	// may not convert traffic into bank inventory faster than this. Checked
	// before the draw so a refused request consumes nothing.
	if d := s.evaluateCeilings(id.Network, now); !d.Allow {
		// A tripped network ceiling becomes a stored ban. Without this the
		// subnet control was the weakest one: it lived only in the expiring
		// rate log, so an attacker rotating addresses inside the prefix got a
		// clean slate mid-attack.
		if d.Code == "NETWORK_CEILING" {
			// RetryAfter is already the ban length: escalating it is what
			// makes a subnet that keeps hammering cost more each time.
			key := s.netKey(id.Network)
			var prior []time.Time
			if b, ok := s.store.GetSubnetBan(key); ok && b != nil {
				prior = b.BanHistory
			}
			ban, _ := limits.EvaluateHistory(s.pol, prior, now, d.RetryAfter)
			if ban <= 0 {
				ban = d.RetryAfter
			}
			s.banSubnet(key, d.Reason, ban, now)
			d.Ban = ban
			d.RetryAfter = ban
		} else if d.Code == "IDENTITY_BANNED" || d.Code == "NETWORK_BANNED" {
			d.Ban = 0
		}
		s.record(ctx, in.RequestID, "", "deny", d.Code, id, int(d.RetryAfter.Seconds()))
		return nil, limits.Refuse(d)
	}

	matches, err := s.store.Lookup(markerHashes)
	if err != nil {
		return nil, apierr.Internal("identity lookup failed")
	}
	var rec *store.Record
	if len(matches) > 0 {
		rec = matches[0]
		for _, m := range matches {
			if m.LastSeen.After(rec.LastSeen) {
				rec = m
			}
		}
	}

	if d := limits.EvaluateWindow(s.pol, rec, now); !d.Allow {
		if rec != nil {
			// Refused while banned: serve the remaining ban, do NOT stamp
			// another. Another stamp here would escalate on every refused
			// request and turn one flood into a permanent ban.
			rec.Requests++
			rec.LastSeen = now
			s.mergeMarkers(rec, markerHashes)
			if err := s.store.Put(rec); err != nil {
				return nil, apierr.Internal("could not update identity")
			}
		}
		s.record(ctx, in.RequestID, idOf(rec), "deny", d.Code, id, int(d.RetryAfter.Seconds()))
		return nil, limits.Refuse(d)
	}

	// Stock check before we commit to issuing anything.
	result, err := draw.Select(s.bank, draw.Request{
		Categories: in.Categories,
		Mix:        in.Mix,
		Size:       size,
	})
	if err != nil {
		s.record(ctx, in.RequestID, idOf(rec), "deny", "INSUFFICIENT_STOCK", id, 0)
		return nil, err
	}

	issued := make([]string, 0, len(result.Examples))
	for _, ex := range result.Examples {
		issued = append(issued, ex.ID)
	}

	if rec == nil {
		rec = &store.Record{
			ID:        newID(),
			Markers:   markerHashes,
			FirstSeen: now,
		}
	} else {
		s.mergeMarkers(rec, markerHashes)
	}
	rec.LastSeen = now
	rec.Requests++
	rec.Draws++
	rec.LastDraw = now
	if err := s.store.Put(rec); err != nil {
		return nil, apierr.Internal("could not persist identity")
	}
	s.bank.MarkUsed(issued, now)
	if err := s.bank.SaveUsage(); err != nil {
		// Usage persistence is important but must not fail the user's draw:
		// the in-memory counter already prevents double-issue this process.
		fmt.Fprintf(os.Stderr, "warn: usage save failed: %v\n", err)
	}
	// Record what was issued, so feedback can later be checked against it. This
	// is the whole integrity mechanism: without it /v1/feedback accepts any
	// invented request id and the results dataset becomes whatever a script
	// last posted.
	entry := store.LedgerEntry{
		RequestID:  in.RequestID,
		IdentityID: rec.ID,
		ExampleIDs: issued,
		IssuedAt:   now,
	}
	if err := s.store.AppendLedger(entry); err != nil {
		// A draw without a ledger row cannot be verified later, so it must fail
		// rather than quietly produce unverifiable results.
		return nil, apierr.Internal("could not record the issued draw")
	}
	s.record(ctx, in.RequestID, rec.ID, "allow", "granted", id, 0)
	s.publishIdentity(rec)

	return &Grant{
		RequestID:  in.RequestID,
		IdentityID: rec.ID,
		Examples:   result.Examples,
		ByCat:      result.ByCat,
		IssuedAt:   now,
		NextDrawAt: now.Add(s.cfg.Window),
		Shortfall:  result.Shortfall,
		Notes:      result.Notes,
	}, nil
}

// Authenticate verifies the caller's access token. Draws are closed-loop: a
// token is required so that every draw can be traced to a registered runner,
// which is what makes the abuse ladder enforceable at all.
func (s *Service) Authenticate(token string) error {
	if s.cfg.AccessToken == "" {
		return apierr.Forbidden("ACCESS_DISABLED",
			"this relay has no access token configured and is not accepting draws")
	}
	if !constantTimeEqual(token, s.cfg.AccessToken) {
		return apierr.Forbidden("ACCESS_DENIED", "a valid access token is required")
	}
	return nil
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// upsert finds an existing record or builds an unsaved one.
func (s *Service) upsert(ctx context.Context, id *identity.Identity, markers []string, now time.Time) (*store.Record, error) {
	matches, err := s.store.Lookup(markers)
	if err != nil {
		return nil, apierr.Internal("identity lookup failed")
	}
	if len(matches) > 0 {
		rec := matches[0]
		s.mergeMarkers(rec, markers)
		rec.LastSeen = now
		return rec, nil
	}
	return &store.Record{ID: newID(), Markers: markers, FirstSeen: now, LastSeen: now}, nil
}

// mergeMarkers keeps a record's marker set current as a caller's device data
// drifts (a new NIC, a reimaged disk) without ever dropping a known marker.
func (s *Service) mergeMarkers(rec *store.Record, markers []string) {
	have := make(map[string]bool, len(rec.Markers))
	for _, m := range rec.Markers {
		have[m] = true
	}
	for _, m := range markers {
		if !have[m] {
			rec.Markers = append(rec.Markers, m)
			have[m] = true
		}
	}
}

// publishIdentity queues the hashed record for the dataset.
//
// The raw row is deliberately NOT queued. The dataset is a git repository, and
// git history is permanent: a committed IP address stays recoverable in the
// commit graph even after the file is deleted, so publishing raw identifiers
// would make the retention promise impossible to keep. Raw rows stay on the
// Space's persistent volume and are pruned locally after 30 days.
func (s *Service) publishIdentity(rec *store.Record) {
	if s.sync == nil || rec == nil {
		return
	}
	if buf, err := store.MarshalRecord(rec); err == nil {
		s.sync.Enqueue(store.RecordPath(rec.ID), buf)
	}
}

func (s *Service) record(ctx context.Context, reqID, id, decision, reason string, ident *identity.Identity, retry int) {
	if ident == nil {
		return
	}
	ev := store.Event{
		TS:         s.now(),
		RequestID:  reqID,
		IdentityID: id,
		Decision:   decision,
		Reason:     reason,
		IPHash:     s.Hash(identity.MarkerIP, ident.IP.String()),
		NetHash:    s.Hash("net", ident.Network.String()),
		Country:    ident.Country,
		OS:         ident.Device.OSFamily,
		Arch:       ident.Device.Arch,
		RetryAfter: retry,
	}
	if err := s.store.AppendEvent(ev); err != nil {
		fmt.Fprintf(os.Stderr, "warn: event not persisted: %v\n", err)
		return
	}
	if s.sync != nil {
		if buf, err := s.store.ReadEventDay(ev.TS); err == nil {
			s.sync.Enqueue(store.EventPath(ev.TS), buf)
		}
		if buf, err := s.store.ReadLedgerDay(ev.TS); err == nil {
			s.sync.Enqueue("ledger/"+ev.TS.UTC().Format("2006-01-02")+".jsonl", buf)
		}
	}
}

// rateKey is the durable log key for an identity. Identity markers are already
// salted hashes, so the key leaks nothing.
// banBaseFor recovers the base ban length for a record from the ladder, so an
// extension escalates from the right starting point instead of from whatever
// is left of the current ban.
func (s *Service) banBaseFor(rec *store.Record) time.Duration {
	return limits.BanForCode(s.pol, rec.BanReason)
}

func (s *Service) rateKey(markers []string) string {
	if len(markers) == 0 {
		return ""
	}
	return "id:" + markers[0]
}

func (s *Service) netKey(p netip.Prefix) string {
	if !p.IsValid() {
		return ""
	}
	return "net:" + s.Hash("net", p.String())
}

const globalKey = "global"

// evaluateVolume rebuilds the request history from the durable log and asks
// the limits package for a verdict.
// evaluateVolumeForMarkers runs the volume ladder against every marker the
// caller presented and returns the worst decision.
//
// Banning on only markers[0] (the IP hash) was a hole: rotating the IP while
// keeping the same machine escaped a ban already earned. Any single marker
// tripping the ladder is now enough.
func (s *Service) evaluateVolumeForMarkers(markerHashes []string, now time.Time) (limits.Decision, *store.Record) {
	var worst limits.Decision
	var worstRec *store.Record
	for _, h := range markerHashes {
		// If this marker is already serving a ban, do not treat every further
		// request as a fresh offence: one attack must cost one ban. The number
		// of bans is what drives escalation, so re-stamping on each refused
		// request turned a single flood into a two-week ban for the whole /24.
		if recs, err := s.store.Lookup([]string{h}); err == nil {
			alreadyBanned := false
			for _, r := range recs {
				if r.Banned(now) {
					alreadyBanned = true
					break
				}
			}
			if alreadyBanned {
				continue
			}
		}
		d := s.evaluateVolume("id:"+h, now)
		if d.Allow {
			continue
		}
		if d.Ban > worst.Ban {
			worst = d
			// A ban needs a record to hang off; find the one this marker
			// belongs to, or create it.
			if rec, err := s.upsert(context.Background(), nil, []string{h}, now); err == nil {
				worstRec = rec
			}
		}
	}
	if worst.Ban == 0 {
		return limits.Decision{Allow: true}, nil
	}
	// Escalate by recent ban history so "serve ten minutes, repeat" cannot loop
	// for ever. The history is read from the record we are about to stamp.
	if worstRec != nil {
		if dur, prior := limits.EvaluateHistory(s.pol, worstRec.BanHistory, now, worst.Ban); dur > 0 {
			worst.Ban = dur
			worst.RetryAfter = dur
			if prior > 0 {
				worst.Reason = fmt.Sprintf("%s (repeat offence: %d ban(s) in the last %s)",
					worst.Reason, prior, s.pol.BanWindow)
			}
		}
	}
	return worst, worstRec
}

// Admit performs the accounting that must happen before the request body is
// parsed.
//
// This exists because body decoding happens in the HTTP layer, so a request
// with malformed JSON never reached any counter: a bot could send rubbish all
// day for free, and the ladder never saw it. Identity is derived from the
// address alone here, the subnet ban and the volume ladder are enforced, and
// the counters are recorded. Device markers cannot be known yet; they are
// recorded later by RequestDraw.
func (s *Service) Admit(in DrawInput) error {
	_ = in.Admitted
	now := s.now()
	id, err := identity.FromRequest(in.RemoteAddr, in.Headers, identity.Device{}, s.Hash, s.cfg.AllowLoopback)
	if err != nil {
		return err
	}
	markerHashes := make([]string, 0, len(id.Markers))
	for _, m := range id.Markers {
		markerHashes = append(markerHashes, m.Hash)
	}

	netHash := s.netKey(id.Network)
	if d := s.subnetBanDecision(netHash, now); !d.Allow {
		s.record(context.Background(), in.RequestID, "", "deny", d.Code, id, int(d.RetryAfter.Seconds()))
		return limits.Refuse(d)
	}

	// A caller already serving a ban is refused here, before the body is read.
	// Without this they kept receiving 400s for malformed bodies during their
	// own ban, which looks like "the ban did not apply" both to them and to
	// anyone reading the logs.
	for _, h := range markerHashes {
		recs, err := s.store.Lookup([]string{h})
		if err != nil {
			continue
		}
		for _, r := range recs {
			if r.Banned(now) {
				d := limits.Decision{
					Code:       "IDENTITY_BANNED",
					Reason:     r.BanReason,
					RetryAfter: r.BannedUntil.Sub(now),
				}
				s.record(context.Background(), in.RequestID, r.ID, "deny", d.Code, id, int(d.RetryAfter.Seconds()))
				return limits.Refuse(d)
			}
		}
	}

	// A caller already serving a ban is refused here, before the body is read.
	// Without this they kept receiving 400s for malformed bodies during their
	// own ban, which looks like "the ban never applied" to them and to anyone
	// reading the logs.
	for _, h := range markerHashes {
		recs, lerr := s.store.Lookup([]string{h})
		if lerr != nil {
			continue
		}
		for _, r := range recs {
			if r.Banned(now) {
				d := limits.Decision{
					Code:       "IDENTITY_BANNED",
					Reason:     r.BanReason,
					RetryAfter: r.BannedUntil.Sub(now),
				}
				s.record(context.Background(), in.RequestID, r.ID, "deny", d.Code, id, int(d.RetryAfter.Seconds()))
				return limits.Refuse(d)
			}
		}
	}

	if d, banRec := s.evaluateVolumeForMarkers(markerHashes, now); !d.Allow {
		if banRec != nil {
			if base := limits.BanForCode(s.pol, d.Code); base > 0 {
				if escalated, _ := limits.EvaluateHistory(s.pol, banRec.BanHistory, now, base); escalated > 0 {
					d.Ban = escalated
					d.RetryAfter = escalated
				}
			}
			limits.ApplyBan(banRec, d, now)
			_ = s.store.Put(banRec)
			s.publishIdentity(banRec)
		}
		s.record(context.Background(), in.RequestID, idOf(banRec), "ban", d.Code, id, int(d.RetryAfter.Seconds()))
		return limits.Refuse(d)
	}

	for _, h := range markerHashes {
		if err := s.rates.Record("id:"+h, now); err != nil {
			fmt.Fprintf(os.Stderr, "warn: rate event not persisted: %v\n", err)
		}
	}
	// Network and global counters are NOT recorded here: they exist to cap how
	// fast the bank can be drained, and only a valid draw drains it. A rejected
	// garbage request should cost the caller, not consume the subnet's budget.
	return nil
}

// subnetBanDecision reports whether this network prefix is currently banned.
func (s *Service) subnetBanDecision(netHash string, now time.Time) limits.Decision {
	if netHash == "" {
		return limits.Decision{Allow: true}
	}
	b, ok := s.store.GetSubnetBan(netHash)
	if !ok || b == nil || !now.Before(b.BannedUntil) {
		return limits.Decision{Allow: true}
	}
	return limits.Decision{
		Code:       "NETWORK_BANNED",
		Reason:     b.Reason,
		RetryAfter: b.BannedUntil.Sub(now),
	}
}

// banSubnet records a network-level ban. This is what makes the network
// ceiling durable: the ceiling itself is evaluated from the rate log, which
// expires, but once tripped the punishment is stored so rotating the IP inside
// the same prefix does not reset it.
func (s *Service) banSubnet(netHash, reason string, ban time.Duration, now time.Time) {
	if netHash == "" || ban <= 0 {
		return
	}
	b, _ := s.store.GetSubnetBan(netHash)
	if b == nil {
		b = &store.SubnetBan{NetHash: netHash}
	}
	b.BanCount++
	b.Reason = reason
	b.BannedUntil = now.Add(ban)
	b.BanHistory = append(b.BanHistory, now)
	if len(b.BanHistory) > 64 {
		b.BanHistory = b.BanHistory[len(b.BanHistory)-64:]
	}
	if err := s.store.PutSubnetBan(b); err != nil {
		fmt.Fprintf(os.Stderr, "warn: subnet ban not persisted: %v\n", err)
	}
}

func (s *Service) evaluateVolume(key string, now time.Time) limits.Decision {
	if key == "" {
		return limits.Decision{Allow: true}
	}
	// The peak rate over the window is what matters, not a raw count: it tells
	// a burst apart from a steady drip at the same average.
	times := s.rates.Times(key, s.pol.RateWindow, now)
	return limits.EvaluateVolume(s.pol, times, now)
}

func (s *Service) evaluateCeilings(p netip.Prefix, now time.Time) limits.Decision {
	if s.cfg.NetMaxDraws > 0 {
		if n := s.rates.Count(s.netKey(p), s.cfg.NetWindow, now); n > s.cfg.NetMaxDraws {
			return limits.Decision{
				Code:       "NETWORK_CEILING",
				Reason:     "this network has reached its draw ceiling",
				RetryAfter: s.cfg.NetWindow,
			}
		}
	}
	if s.cfg.GlobalMax > 0 {
		if n := s.rates.Count(globalKey, s.cfg.GlobalWindow, now); n > s.cfg.GlobalMax {
			return limits.Decision{
				Code:       "GLOBAL_CEILING",
				Reason:     "the bank is at capacity for this window",
				RetryAfter: s.cfg.GlobalWindow,
			}
		}
	}
	return limits.Decision{Allow: true}
}

// Health summarises readiness.
type Health struct {
	OK            bool           `json:"ok"`
	Version       string         `json:"version"`
	UptimeSeconds int64          `json:"uptime_s"`
	Bank          map[string]int `json:"bank"`
	Inventory     bank.Counts    `json:"inventory"`
	Records       int            `json:"records"`
	Markers       int            `json:"markers"`
	Reputation    int            `json:"reputation_ranges"`
	SaltIsDefault bool           `json:"salt_is_default"`
	Sync          *syncctl.Stats `json:"sync,omitempty"`
}

var startedAt = time.Now()

// Version is stamped at build time via ldflags.
var Version = "dev"

// Health reports service state without touching the bank lists.
func (s *Service) Health() Health {
	recs, markers := s.store.Stats()
	return Health{
		OK:            s.bank.Size() > 0,
		Version:       Version,
		UptimeSeconds: int64(time.Since(startedAt).Seconds()),
		Bank:          s.bank.Stats(),
		Inventory:     s.bank.Inventory(),
		Records:       recs,
		Markers:       markers,
		Reputation:    s.rep.Size(),
		SaltIsDefault: s.cfg.SaltIsDefault(),
		Sync:          s.SyncStats(),
	}
}

// Categories lists servable categories.
func (s *Service) Categories() []string { return s.bank.Categories() }

// StaleExamples exposes the stale list for the daily curator job.
func (s *Service) StaleExamples() []bank.Example { return s.bank.Stale() }

// Prune clears expired bans. No personal data is stored, so there is nothing
// to expire; the retention obligation disappears with the raw values.
func (s *Service) Prune() (int, error) { return s.store.Prune(s.now()) }

// PruneLedger drops ledger days older than the configured retention and forgets
// subnet bans that have been expired for longer than the ban window. Both are
// called from the housekeeping ticker.
func (s *Service) PruneLedger(now time.Time) (int, int, error) {
	days := s.cfg.LedgerKeepDays
	if days <= 0 {
		days = 7
	}
	files, err := s.store.PruneLedger(now, days)
	if err != nil {
		return 0, 0, err
	}
	keep := s.cfg.BanWindow
	if keep <= 0 {
		keep = 24 * time.Hour
	}
	bans, err := s.store.PruneSubnetBans(now, keep)
	return files, bans, err
}

// Maintain compacts and fsyncs the durable rate log. Called on a timer.
func (s *Service) Maintain(now time.Time) (int, error) {
	n, err := s.rates.Compact(now)
	if err != nil {
		return n, err
	}
	return n, s.rates.Flush()
}

// Flush persists counters and the manifest.
func (s *Service) Flush() error {
	if err := s.bank.SaveUsage(); err != nil {
		return err
	}
	return s.store.FlushIndex()
}

// FlushRates fsyncs the durable rate log; called on shutdown.
func (s *Service) FlushRates() error { return s.rates.Flush() }

// PublishableSubtrees is the single canonical list of store subtrees that may
// be mirrored to the dataset. "raw" is absent on purpose: raw identity rows
// must never enter a git repository, whose history is permanent.
//
// Both the live sync controller and the one-shot -sync export read this list,
// so the two paths cannot drift apart (a real leak happened when they did).
var PublishableSubtrees = []string{"identities", "markers", "events", "results"}

// ExportSnapshot copies the publishable store tree into dir for the dataset
// mirror.
func (s *Service) ExportSnapshot(dir string) ([]string, error) {
	if err := s.Flush(); err != nil {
		return nil, err
	}
	src := s.cfg.DataDir
	var written []string
	for _, sub := range PublishableSubtrees {
		from := filepath.Join(src, sub)
		to := filepath.Join(dir, sub)
		n, err := copyTree(from, to)
		if err != nil {
			return written, err
		}
		written = append(written, n...)
	}
	return written, nil
}

func copyTree(from, to string) ([]string, error) {
	var out []string
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dest := filepath.Join(to, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(dest, buf, 0o640); err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(filepath.Join(filepath.Base(from), rel)))
		return nil
	})
	if os.IsNotExist(err) {
		return out, nil
	}
	return out, err
}

func idOf(rec *store.Record) string {
	if rec == nil {
		return ""
	}
	return rec.ID
}

// newID returns a random 128-bit hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is not recoverable in any useful way here.
		panic("core: entropy source unavailable")
	}
	return hex.EncodeToString(b[:])
}

// SubmitResult records a benchmark report from a front-end runner.
func (s *Service) SubmitResult(r store.Result) error {
	return s.store.AppendResult(r)
}

// Erase reports whether an identity is known. There is no personal data to
// delete: only hashes are stored, so the endpoint is retained for compatibility
// and answers "nothing to erase" rather than pretending to remove something.
func (s *Service) Erase(id string) (bool, error) {
	_, known := s.store.Get(id)
	return known, nil
}

// SyncFlush publishes everything queued for the dataset. Used by -sync, by
// shutdown, and by tests.
func (s *Service) SyncFlush(ctx context.Context) error {
	if s.sync == nil {
		return nil
	}
	return s.sync.FlushNow(ctx)
}
