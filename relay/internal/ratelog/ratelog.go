// Package ratelog is a durable, sharded sliding-window event log.
//
// It exists because the spam ladder must survive a process restart. On Hugging
// Face Spaces the container is stopped when idle and restarted on the next
// request, so an in-memory counter is not a rate limit at all: an attacker
// simply hammers through a sleep/wake cycle.
//
// Durability model, chosen deliberately:
//
//   - Every event is written with a single append(2) syscall before the request
//     is answered. That data lives in the page cache, so it survives the
//     process being killed or restarted (which is the threat here).
//   - fsync happens on a timer and on shutdown, so a machine-level crash can
//     lose at most FlushInterval of events. Paying fsync per request would cost
//     more than the protection is worth.
//
// Layout mirrors the rest of the store so a shard can be inspected by hand:
//
//	RATE_DIR/<ab>/<key>.jsonl     one {"t":<unix-millis>} per line
//
// <ab> is the first two hex characters of sha256(key), so no directory grows
// past ~1/256 of the corpus.
package ratelog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxEntriesPerKey bounds memory even if a caller floods one key.
const maxEntriesPerKey = 4096

// entry is the on-disk record. A compact integer keeps the log small.
type entry struct {
	T int64 `json:"t"` // unix milliseconds
}

// Log is a thread-safe durable sliding-window log.
type Log struct {
	dir       string
	retention time.Duration

	mu      sync.Mutex
	entries map[string][]int64 // key -> ascending unix millis
	files   map[string]*os.File

	clock func() time.Time
}

// Open loads any existing log under dir, discarding entries older than
// retention. retention should exceed the largest window the caller uses.
func Open(dir string, retention time.Duration) (*Log, error) {
	if dir == "" {
		return nil, errors.New("ratelog: empty directory")
	}
	if retention <= 0 {
		retention = 48 * time.Hour
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("ratelog: create %s: %w", dir, err)
	}
	l := &Log{
		dir:       dir,
		retention: retention,
		entries:   map[string][]int64{},
		files:     map[string]*os.File{},
		clock:     func() time.Time { return time.Now().UTC() },
	}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

// SetClock overrides the clock. Tests only.
func (l *Log) SetClock(f func() time.Time) { l.clock = f }

// load replays the shard tree into memory.
func (l *Log) load() error {
	shards, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ratelog: read dir: %w", err)
	}
	cutoff := l.clock().Add(-l.retention).UnixMilli()

	for _, shardDir := range shards {
		if !shardDir.IsDir() || len(shardDir.Name()) != 2 {
			continue
		}
		inner := filepath.Join(l.dir, shardDir.Name())
		files, err := os.ReadDir(inner)
		if err != nil {
			return fmt.Errorf("ratelog: read %s: %w", inner, err)
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".jsonl" {
				continue
			}
			key := strings.TrimSuffix(f.Name(), ".jsonl")
			if err := l.loadFile(filepath.Join(inner, f.Name()), key, cutoff); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *Log) loadFile(path, key string, cutoff int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("ratelog: open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	out := l.entries[key]
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(line, &e); err != nil {
			// A torn final line from a crash is expected; skip it rather
			// than refusing to start.
			continue
		}
		if e.T < cutoff {
			continue
		}
		out = append(out, e.T)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("ratelog: scan %s: %w", path, err)
	}
	if len(out) > 0 {
		l.entries[key] = out
	}
	return nil
}

// Record appends one event for key. The write reaches the kernel before this
// returns, so a subsequent process restart still sees it.
func (l *Log) Record(key string, at time.Time) error {
	if key == "" {
		return errors.New("ratelog: empty key")
	}
	ms := at.UnixMilli()

	l.mu.Lock()
	defer l.mu.Unlock()

	list := l.entries[key]
	list = append(list, ms)
	if len(list) > maxEntriesPerKey {
		list = list[len(list)-maxEntriesPerKey:]
	}
	l.entries[key] = list

	f, err := l.fileForLocked(key)
	if err != nil {
		return err
	}
	line, err := json.Marshal(entry{T: ms})
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// Count returns how many events for key fall inside window, ending at now.
func (l *Log) Count(key string, window time.Duration, now time.Time) int {
	if window <= 0 {
		return 0
	}
	cutoff := now.Add(-window).UnixMilli()

	l.mu.Lock()
	defer l.mu.Unlock()

	list := l.entries[key]

	// Events are appended in arrival order, which is only approximately
	// monotonic (a retry can arrive with an earlier timestamp), so this scan
	// must not assume sorted input. The retained slice is bounded by
	// maxEntriesPerKey, so a full pass is cheap and always correct.
	retainCutoff := now.Add(-l.retention).UnixMilli()
	n := 0
	for _, t := range list {
		if t >= cutoff {
			n++
		}
	}

	// Opportunistically drop events that can never count again, but only when
	// there is enough to be worth the allocation.
	if len(list) > 64 && list[0] < retainCutoff {
		kept := make([]int64, 0, len(list))
		for _, t := range list {
			if t >= retainCutoff {
				kept = append(kept, t)
			}
		}
		l.entries[key] = kept
	}
	return n
}

// Times returns the event timestamps inside the window, newest last. The peak
// rate calculation needs the distribution, not just the count: 180 requests
// spread over a minute and 180 in two seconds have the same Count but a very
// different peak.
func (l *Log) Times(key string, window time.Duration, now time.Time) []time.Time {
	if window <= 0 {
		return nil
	}
	cutoff := now.Add(-window).UnixMilli()

	l.mu.Lock()
	list := l.entries[key]
	out := make([]time.Time, 0, len(list))
	for _, t := range list {
		if t >= cutoff {
			out = append(out, time.UnixMilli(t).UTC())
		}
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// CountPrefix sums Count over every key sharing a prefix, used for subnet and
// network-wide ceilings.
func (l *Log) CountPrefix(prefix string, window time.Duration, now time.Time) int {
	if window <= 0 {
		return 0
	}
	l.mu.Lock()
	keys := make([]string, 0, 8)
	for k := range l.entries {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	l.mu.Unlock()

	total := 0
	for _, k := range keys {
		total += l.Count(k, window, now)
	}
	return total
}

// Flush fsyncs every open file.
func (l *Log) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	for _, f := range l.files {
		if err := f.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Compact rewrites every file keeping only entries inside retention, and
// forgets keys with nothing left. Returns how many files were rewritten.
func (l *Log) Compact(now time.Time) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-l.retention).UnixMilli()
	rewritten := 0

	for key, list := range l.entries {
		kept := make([]int64, 0, len(list))
		for _, t := range list {
			if t >= cutoff {
				kept = append(kept, t)
			}
		}
		if len(kept) == 0 {
			delete(l.entries, key)
			if f, ok := l.files[key]; ok {
				_ = f.Close()
				delete(l.files, key)
			}
			if err := os.Remove(l.pathFor(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return rewritten, err
			}
			continue
		}
		l.entries[key] = kept
		if len(kept) == len(list) {
			continue
		}
		if err := l.rewriteLocked(key, kept); err != nil {
			return rewritten, err
		}
		rewritten++
	}
	return rewritten, nil
}

func (l *Log) rewriteLocked(key string, list []int64) error {
	if f, ok := l.files[key]; ok {
		_ = f.Close()
		delete(l.files, key)
	}
	path := l.pathFor(key)
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	w := bufio.NewWriter(tmp)
	for _, t := range list {
		line, err := json.Marshal(entry{T: t})
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
			return err
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
			return err
		}
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}

func (l *Log) fileForLocked(key string) (*os.File, error) {
	if f, ok := l.files[key]; ok {
		return f, nil
	}
	path := l.pathFor(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	l.files[key] = f
	return f, nil
}

func (l *Log) pathFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	shard := hex.EncodeToString(sum[:])[:2]
	return filepath.Join(l.dir, shard, key+".jsonl")
}

// PathFor exposes the on-disk path for a key. Used by tests and operators.
func (l *Log) PathFor(key string) string { return l.pathFor(key) }

// Keys reports how many keys are currently tracked.
func (l *Log) Keys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Close flushes and closes every handle.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	for key, f := range l.files {
		if err := f.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(l.files, key)
	}
	return firstErr
}
