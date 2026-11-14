// Package store is the relay's durable state: who we have seen, what they were
// given, and when. It is deliberately simple — small JSON files in a sharded
// directory tree — because the whole point is that the data stays readable and
// searchable by prefix instead of collapsing into one enormous file.
//
// Layout under DATA_DIR:
//
//	index.json                     manifest: counts, format version, created_at
//	identities/<ab>/<id>.json      one record per known caller (hashes only)
//	markers/<ab>/<hash>.json       marker -> identity ids reverse index
//	events/<YYYY-MM-DD>.jsonl      append-only decision log
//	raw/<ab>/<id>.json             raw ip/machine_id/disk_uuid (private, TTL'd)
//
// <ab> is the first two hex characters of a sha256, so no directory ever grows
// past ~1/256 of the corpus and a lookup is a single file open.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FormatVersion is bumped whenever the on-disk shape changes.
const FormatVersion = 1

// Record is everything we persist about one caller, using hashes only.
type Record struct {
	ID        string    `json:"id"`
	Markers   []string  `json:"markers"` // marker hashes, order preserved
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Requests  int       `json:"requests"`
	Draws     int       `json:"draws"`
	LastDraw  time.Time `json:"last_draw,omitempty"`

	BannedUntil time.Time `json:"banned_until,omitempty"`
	BanReason   string    `json:"ban_reason,omitempty"`
	BanCount    int       `json:"ban_count,omitempty"`
}

// Banned reports whether the record is currently barred.
func (r *Record) Banned(now time.Time) bool { return now.Before(r.BannedUntil) }

// MarkerIndex is the reverse index for one marker hash.
type MarkerIndex struct {
	Hash       string       `json:"hash"`
	Identities []IndexEntry `json:"identities"`
}

// IndexEntry links a marker to an identity with the times we care about.
type IndexEntry struct {
	ID          string    `json:"id"`
	FirstSeen   time.Time `json:"first_seen"`
	LastSeen    time.Time `json:"last_seen"`
	LastDraw    time.Time `json:"last_draw"`
	BannedUntil time.Time `json:"banned_until,omitempty"`
}

// RawIdentity is the sensitive half and lives only in the private dataset.
type RawIdentity struct {
	ID        string    `json:"id"`
	IP        string    `json:"ip"`
	IPVersion string    `json:"ip_version"`
	Network   string    `json:"network"`
	MachineID string    `json:"machine_id"`
	DiskUUID  string    `json:"disk_uuid"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// Event is one append-only decision record.
type Event struct {
	TS         time.Time `json:"ts"`
	RequestID  string    `json:"request_id"`
	IdentityID string    `json:"identity_id"`
	Decision   string    `json:"decision"`
	Reason     string    `json:"reason"`
	IPHash     string    `json:"ip_hash"`
	NetHash    string    `json:"network_hash"`
	Country    string    `json:"country,omitempty"`
	OS         string    `json:"os,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	Count      int       `json:"count,omitempty"`
	RetryAfter int       `json:"retry_after_s,omitempty"`
}

// Index is the top-level manifest.
type Index struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Records   int       `json:"records"`
	Events    int       `json:"events"`
}

// Store is a thread-safe handle on the data directory.
type Store struct {
	mu  sync.RWMutex
	dir string

	records map[string]*Record  // id -> record
	markers map[string][]string // marker hash -> ids
	raw     map[string]*RawIdentity

	events int
}

// Open loads (or initialises) a Store rooted at dir.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("store: empty data dir")
	}
	for _, sub := range []string{"identities", "markers", "events", "raw"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", sub, err)
		}
	}
	s := &Store{
		dir:     dir,
		records: map[string]*Record{},
		markers: map[string][]string{},
		raw:     map[string]*RawIdentity{},
	}
	if err := s.loadIndex(); err != nil {
		return nil, err
	}
	if err := s.loadRecords(); err != nil {
		return nil, err
	}
	if err := s.loadRaw(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadRecords rebuilds records and the marker index from the shard tree.
// This is what makes a restart (or a Hugging Face Space waking from sleep)
// behave exactly like the process that wrote the data.
func (s *Store) loadRecords() error {
	root := filepath.Join(s.dir, "identities")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read identities: %w", err)
	}
	for _, shardDir := range entries {
		if !shardDir.IsDir() || !ValidShard(shardDir.Name()) {
			continue
		}
		inner := filepath.Join(root, shardDir.Name())
		files, err := os.ReadDir(inner)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", inner, err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			buf, err := os.ReadFile(filepath.Join(inner, f.Name()))
			if err != nil {
				return fmt.Errorf("store: read %s: %w", f.Name(), err)
			}
			var rec Record
			if err := json.Unmarshal(buf, &rec); err != nil {
				return fmt.Errorf("store: parse %s: %w", f.Name(), err)
			}
			if rec.ID == "" {
				continue
			}
			clone := rec
			s.records[clone.ID] = &clone
			for _, h := range clone.Markers {
				s.markers[h] = append(s.markers[h], clone.ID)
			}
		}
	}
	return nil
}

// loadRaw rebuilds the sensitive tree. Only used to answer lookups; the data
// never leaves the process except through ExportSnapshot.
func (s *Store) loadRaw() error {
	root := filepath.Join(s.dir, "raw")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read raw: %w", err)
	}
	for _, shardDir := range entries {
		if !shardDir.IsDir() || !ValidShard(shardDir.Name()) {
			continue
		}
		inner := filepath.Join(root, shardDir.Name())
		files, err := os.ReadDir(inner)
		if err != nil {
			return fmt.Errorf("store: read %s: %w", inner, err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			buf, err := os.ReadFile(filepath.Join(inner, f.Name()))
			if err != nil {
				return fmt.Errorf("store: read %s: %w", f.Name(), err)
			}
			var raw RawIdentity
			if err := json.Unmarshal(buf, &raw); err != nil {
				return fmt.Errorf("store: parse %s: %w", f.Name(), err)
			}
			if raw.ID == "" {
				continue
			}
			clone := raw
			s.raw[clone.ID] = &clone
		}
	}
	return nil
}

func (s *Store) loadIndex() error {
	path := filepath.Join(s.dir, "index.json")
	buf, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		idx := Index{Version: FormatVersion, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
		return writeJSON(path, &idx)
	}
	if err != nil {
		return fmt.Errorf("store: read index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(buf, &idx); err != nil {
		return fmt.Errorf("store: parse index: %w", err)
	}
	if idx.Version != FormatVersion {
		return fmt.Errorf("store: index version %d, expected %d", idx.Version, FormatVersion)
	}
	s.events = idx.Events
	return nil
}

// shard maps a key to its two-character directory name.
func shard(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:2]
}

// Lookup returns every record sharing ANY of the supplied marker hashes.
// This is the "is this person already known" query and it is O(len(markers))
// file opens, never a scan.
func (s *Store) Lookup(markerHashes []string) ([]*Record, error) {
	seen := map[string]bool{}
	var out []*Record
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range markerHashes {
		ids, ok := s.markers[h]
		if !ok {
			continue
		}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			rec, ok := s.records[id]
			if !ok {
				continue
			}
			seen[id] = true
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out, nil
}

// Get returns a record by ID.
func (s *Store) Get(id string) (*Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.records[id]
	return rec, ok
}

// Put persists a record and its marker links atomically enough for our needs:
// we write the shard files first, then update the in-memory maps under a lock.
func (s *Store) Put(rec *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	clone := *rec
	s.records[clone.ID] = &clone

	for _, h := range clone.Markers {
		ids := s.markers[h]
		found := false
		for _, id := range ids {
			if id == clone.ID {
				found = true
				break
			}
		}
		if !found {
			s.markers[h] = append(ids, clone.ID)
		}
	}
	return s.persistLocked(&clone)
}

func (s *Store) persistLocked(rec *Record) error {
	if err := writeJSON(filepath.Join(s.dir, "identities", shard(rec.ID), rec.ID+".json"), rec); err != nil {
		return err
	}
	for _, h := range rec.Markers {
		idxs := make([]IndexEntry, 0, len(s.markers[h]))
		for _, id := range s.markers[h] {
			r, ok := s.records[id]
			if !ok {
				continue
			}
			idxs = append(idxs, IndexEntry{
				ID:          r.ID,
				FirstSeen:   r.FirstSeen,
				LastSeen:    r.LastSeen,
				LastDraw:    r.LastDraw,
				BannedUntil: r.BannedUntil,
			})
		}
		if err := writeJSON(filepath.Join(s.dir, "markers", shard(h), h+".json"), MarkerIndex{Hash: h, Identities: idxs}); err != nil {
			return err
		}
	}
	return nil
}

// PutRaw stores the sensitive fields. Only the private dataset mirrors this
// tree, and Prune removes it on schedule.
func (s *Store) PutRaw(raw *RawIdentity) error {
	s.mu.Lock()
	s.raw[raw.ID] = raw
	s.mu.Unlock()
	return writeJSON(filepath.Join(s.dir, "raw", shard(raw.ID), raw.ID+".json"), raw)
}

// Raw returns a copy of the sensitive record.
func (s *Store) Raw(id string) (*RawIdentity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.raw[id]
	if !ok {
		return nil, false
	}
	clone := *r
	return &clone, true
}

// AppendEvent adds one line to today's decision log.
func (s *Store) AppendEvent(ev Event) error {
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "events", ev.TS.UTC().Format("2006-01-02")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	s.mu.Lock()
	s.events++
	s.mu.Unlock()
	return nil
}

// Prune removes raw identity rows older than ttl and clears expired bans.
// It returns how many raw rows went away so the caller can log it.
func (s *Store) Prune(now time.Time, ttl time.Duration) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, raw := range s.raw {
		if now.Sub(raw.LastSeen) > ttl {
			if err := os.Remove(filepath.Join(s.dir, "raw", shard(id), id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			delete(s.raw, id)
			removed++
		}
	}
	for _, rec := range s.records {
		if !rec.BannedUntil.IsZero() && !now.Before(rec.BannedUntil) {
			rec.BannedUntil = time.Time{}
			rec.BanReason = ""
			if err := s.persistLocked(rec); err != nil {
				return removed, err
			}
		}
	}
	return removed, nil
}

// Stats reports headline counters for the health endpoint.
func (s *Store) Stats() (records, markers, raw int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.records), len(s.markers), len(s.raw)
}

// FlushIndex writes the manifest. Best effort; called on shutdown and by the
// background flusher.
func (s *Store) FlushIndex() error {
	s.mu.RLock()
	idx := Index{
		Version:   FormatVersion,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		Records:   len(s.records),
		Events:    s.events,
	}
	s.mu.RUnlock()
	if prev, err := os.ReadFile(filepath.Join(s.dir, "index.json")); err == nil {
		var old Index
		if json.Unmarshal(prev, &old) == nil && !old.CreatedAt.IsZero() {
			idx.CreatedAt = old.CreatedAt
		}
	}
	return writeJSON(filepath.Join(s.dir, "index.json"), &idx)
}

// writeJSON writes atomically: temp file in the same directory, then rename.
func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// ValidShard reports whether a directory name looks like one we create.
func ValidShard(name string) bool {
	if len(name) != 2 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil && !strings.ContainsAny(name, "./")
}

// ShardOf exposes the shard directory name for a key, so callers (and tests)
// never have to reimplement the layout.
func ShardOf(key string) string { return shard(key) }

// --- dataset mirror helpers -------------------------------------------------
//
// These expose the canonical on-disk path and serialised form for each artefact
// so the sync controller publishes byte-identical content to what the store
// writes locally. Two code paths producing two formats would be a silent
// source of dataset drift.

// RecordPath is the dataset path for a hashed identity record.
func RecordPath(id string) string { return path.Join("identities", shard(id), id+".json") }

// RawPath is the dataset path for a sensitive identity row.
func RawPath(id string) string { return path.Join("raw", shard(id), id+".json") }

// EventPath is the dataset path for one day of the event log.
func EventPath(at time.Time) string {
	return path.Join("events", at.UTC().Format("2006-01-02")+".jsonl")
}

// ResultPath is the dataset path for one day of benchmark results.
func ResultPath(at time.Time) string {
	return path.Join("results", at.UTC().Format("2006-01-02")+".jsonl")
}

// MarshalRecord serialises a record exactly as it is stored.
func MarshalRecord(rec *Record) ([]byte, error) {
	buf, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(buf, '\n'), nil
}

// MarshalRaw serialises a raw identity row exactly as it is stored.
func MarshalRaw(raw *RawIdentity) ([]byte, error) {
	buf, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(buf, '\n'), nil
}

// ReadEventDay returns the whole event log for a day, for publication as one
// file. A day is a natural batching unit: it is append-mostly and small.
func (s *Store) ReadEventDay(at time.Time) ([]byte, error) {
	p := filepath.Join(s.dir, "events", at.UTC().Format("2006-01-02")+".jsonl")
	buf, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return []byte{}, nil
	}
	return buf, err
}

// ReadResultDay returns the whole result log for a day.
func (s *Store) ReadResultDay(at time.Time) ([]byte, error) {
	p := filepath.Join(s.dir, "results", at.UTC().Format("2006-01-02")+".jsonl")
	buf, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return []byte{}, nil
	}
	return buf, err
}
