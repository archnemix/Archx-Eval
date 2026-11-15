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
			Window:      cfg.Window,
			SpamWindow:  cfg.SpamWindow,
			SpamCount:   cfg.SpamCount,
			SpamBan:     cfg.SpamBan,
			BurstWindow: cfg.BurstWindow,
			BurstCount:  cfg.BurstCount,
			BurstBan:    cfg.BurstBan,
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

	id, err := identity.FromRequest(in.RemoteAddr, in.Headers, in.Device, s.Hash, s.cfg.AllowLoopback)
	if err != nil {
		return nil, err
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

	markerHashes := make([]string, 0, len(id.Markers))
	for _, m := range id.Markers {
		markerHashes = append(markerHashes, m.Hash)
	}

	// Spam ladder is checked before storage work so a flood costs us nothing,
	// and it reads a durable log so a restart cannot reset the counters.
	rateKey := s.rateKey(markerHashes)
	if d := s.evaluateVolume(rateKey, now); !d.Allow {
		// A ban needs a record to hang off; find or create one first.
		rec, err := s.upsert(ctx, id, markerHashes, now)
		if err != nil {
			return nil, err
		}
		limits.ApplyBan(rec, d, now)
		if err := s.store.Put(rec); err != nil {
			return nil, apierr.Internal("could not record the ban")
		}
		s.record(ctx, in.RequestID, rec.ID, "ban", d.Code, id, int(d.RetryAfter.Seconds()))
		return nil, limits.Refuse(d)
	}
	if err := s.rates.Record(rateKey, now); err != nil {
		fmt.Fprintf(os.Stderr, "warn: rate event not persisted: %v\n", err)
	}
	if err := s.rates.Record(s.netKey(id.Network), now); err != nil {
		fmt.Fprintf(os.Stderr, "warn: network rate event not persisted: %v\n", err)
	}
	if err := s.rates.Record(globalKey, now); err != nil {
		fmt.Fprintf(os.Stderr, "warn: global rate event not persisted: %v\n", err)
	}

	// Network and global ceilings: a single subnet, or the service as a whole,
	// may not convert traffic into bank inventory faster than this. Checked
	// before the draw so a refused request consumes nothing.
	if d := s.evaluateCeilings(id.Network, now); !d.Allow {
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
	}
}

// rateKey is the durable log key for an identity. Identity markers are already
// salted hashes, so the key leaks nothing.
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
func (s *Service) evaluateVolume(key string, now time.Time) limits.Decision {
	if key == "" {
		return limits.Decision{Allow: true}
	}
	burst := s.rates.Count(key, s.cfg.BurstWindow, now)
	sustained := s.rates.Count(key, s.cfg.SpamWindow, now)

	if s.cfg.BurstCount > 0 && burst >= s.cfg.BurstCount {
		return limits.Decision{
			Code:       "SPAM_BURST",
			Reason:     "request rate far above the permitted rate",
			RetryAfter: s.cfg.BurstBan,
			Ban:        s.cfg.BurstBan,
		}
	}
	if s.cfg.SpamCount > 0 && sustained >= s.cfg.SpamCount {
		return limits.Decision{
			Code:       "SPAM_SUSTAINED",
			Reason:     "too many requests in the spam window",
			RetryAfter: s.cfg.SpamBan,
			Ban:        s.cfg.SpamBan,
		}
	}
	return limits.Decision{Allow: true}
}

// evaluateCeilings applies subnet and service-wide draw ceilings. These are
// separate from the per-identity ladder: they protect the inventory rather than
// punishing one caller.
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
