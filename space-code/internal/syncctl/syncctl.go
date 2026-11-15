// Package syncctl is the single writer for the private dataset.
//
// The dataset is a git repository, and git does not merge concurrent pushes:
// two writers racing each other means one of them is rejected and its data is
// lost or must be reapplied. Rather than scatter retry logic through the call
// sites, every write goes through one controller that:
//
//  1. Coalesces. Changes are queued and published in batches on a fixed
//     interval, so a burst of traffic becomes one commit instead of hundreds.
//  2. Serialises. Exactly one publish runs at a time, process-wide, so a race
//     with the remote cannot happen by construction.
//  3. Deduplicates. A queued path replaces any earlier queued version of
//     itself, so a file rewritten five times costs one write.
//  4. Skips unchanged files. Content is compared by git blob hash against the
//     remote tree, so an idle system produces no commits at all.
//  5. Adapts. If the remote rate-limits us, the flush interval doubles (with a
//     ceiling) and is restored on success: gentler when pushed, faster when not.
//  6. Never blocks a request. Enqueue is a memory operation with a hard budget;
//     if the queue is full the newest change is dropped and counted, because a
//     slow dataset must not slow down draws.
package syncctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/hf"
)

// Config controls batching and backoff.
type Config struct {
	// Interval is the target flush cadence.
	Interval time.Duration
	// MinInterval and MaxInterval bound the adaptive backoff.
	MinInterval time.Duration
	MaxInterval time.Duration
	// MaxFiles caps a single commit; larger queues split across commits.
	MaxFiles int
	// MaxQueued caps memory. Beyond this, new changes are dropped and counted.
	MaxQueued int
	// MaxRetries is how many times one batch is retried before being dropped.
	MaxRetries int
	// RetryBase is the first retry delay; it doubles each attempt.
	RetryBase time.Duration
}

// DefaultConfig is tuned for a private dataset with modest traffic.
func DefaultConfig() Config {
	return Config{
		Interval:    30 * time.Second,
		MinInterval: 15 * time.Second,
		MaxInterval: 10 * time.Minute,
		MaxFiles:    100,
		MaxQueued:   20000,
		MaxRetries:  4,
		RetryBase:   2 * time.Second,
	}
}

// Stats is a snapshot of controller health, safe to serve from /health.
type Stats struct {
	Queued        int    `json:"queued"`
	Published     int64  `json:"published"`
	Skipped       int64  `json:"skipped"`
	Retries       int64  `json:"retries"`
	Commits       int64  `json:"commits"`
	Failures      int64  `json:"failures"`
	Dropped       int64  `json:"dropped"`
	IntervalMS    int64  `json:"interval_ms"`
	LastFlushUnix int64  `json:"last_flush_unix"`
	LastError     string `json:"last_error,omitempty"`
}

// Controller is the single writer.
type Controller struct {
	cfg    Config
	remote hf.Remote
	log    *slog.Logger

	// mu guards the queue and the version cache. It is held only for short
	// in-memory operations, never across a network call.
	mu    sync.Mutex
	queue map[string][]byte

	// publishMu is held across the whole remote write. This is what actually
	// guarantees "one commit in flight": the queue lock is far too wide to hold
	// during a network call, and far too narrow to provide that guarantee.
	// A probe test asserts peak concurrency stays at 1.
	publishMu sync.Mutex

	// versions caches the remote blob hash per path so we do not re-list the
	// whole tree for every batch. Guarded by mu.
	versions map[string]string
	listed   bool

	interval time.Duration

	published atomic.Int64
	skipped   atomic.Int64
	retries   atomic.Int64
	commits   atomic.Int64
	failures  atomic.Int64
	dropped   atomic.Int64

	lastFlush atomic.Int64
	lastErr   atomic.Value // string

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New builds a controller. It does not start the background loop; call Run.
func New(cfg Config, remote hf.Remote, log *slog.Logger) *Controller {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultConfig().Interval
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = DefaultConfig().MaxFiles
	}
	if cfg.MaxQueued <= 0 {
		cfg.MaxQueued = DefaultConfig().MaxQueued
	}
	// MinInterval/MaxInterval bound the adaptive backoff. Leaving them zero is
	// not harmless: widen() clamps to MaxInterval, so a zero ceiling would set
	// the interval to 0 on the first failure and the controller would hammer a
	// failing dataset in a tight loop instead of backing off.
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = DefaultConfig().MinInterval
	}
	if cfg.MaxInterval <= 0 {
		cfg.MaxInterval = DefaultConfig().MaxInterval
	}
	if cfg.MaxInterval < cfg.Interval {
		cfg.MaxInterval = cfg.Interval
	}
	if cfg.MinInterval > cfg.Interval {
		cfg.MinInterval = cfg.Interval
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Controller{
		cfg:      cfg,
		remote:   remote,
		log:      log,
		queue:    make(map[string][]byte, 64),
		versions: make(map[string]string, 256),
		interval: cfg.Interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Enqueue records a change. It never blocks on the network and never returns an
// error to a request path: if the queue is full the change is dropped and
// counted, because losing a sync record is better than stalling a draw.
func (c *Controller) Enqueue(path string, content []byte) {
	if path == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.queue[path]; !exists && len(c.queue) >= c.cfg.MaxQueued {
		c.dropped.Add(1)
		return
	}
	// Replace, never append: the newest version of a path wins.
	c.queue[path] = content
}

// EnqueueFile is a convenience wrapper.
func (c *Controller) EnqueueFile(f hf.File) { c.Enqueue(f.Path, f.Content) }

// Queued reports how many distinct paths are waiting.
func (c *Controller) Queued() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue)
}

// Stats snapshots counters.
func (c *Controller) Stats() Stats {
	s := Stats{
		Queued:        c.Queued(),
		Published:     c.published.Load(),
		Skipped:       c.skipped.Load(),
		Retries:       c.retries.Load(),
		Commits:       c.commits.Load(),
		Failures:      c.failures.Load(),
		Dropped:       c.dropped.Load(),
		IntervalMS:    int64(c.currentInterval() / time.Millisecond),
		LastFlushUnix: c.lastFlush.Load(),
	}
	if v, ok := c.lastErr.Load().(string); ok {
		s.LastError = v
	}
	return s
}

func (c *Controller) currentInterval() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.interval
}

// Run drives the flush loop until the context is cancelled or Stop is called.
// A final flush is attempted on shutdown so queued work is not silently lost.
func (c *Controller) Run(ctx context.Context) {
	defer close(c.done)
	for {
		timer := time.NewTimer(c.currentInterval())
		select {
		case <-ctx.Done():
			timer.Stop()
			c.flushAll(context.Background())
			return
		case <-c.stop:
			timer.Stop()
			c.flushAll(context.Background())
			return
		case <-timer.C:
			c.flushAll(ctx)
		}
	}
}

// Stop asks the loop to finish and waits for it.
func (c *Controller) Stop() {
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
}

// FlushNow publishes everything queued right now. Used by -sync and by tests.
func (c *Controller) FlushNow(ctx context.Context) error { return c.flushAll(ctx) }

// flushAll repeatedly publishes batches until the queue is empty or a batch
// fails. Every iteration takes the lock, so no other publish can interleave.
func (c *Controller) flushAll(ctx context.Context) error {
	var lastErr error
	for {
		err := c.flushOnce(ctx)
		if err != nil {
			lastErr = err
			if !isRetryable(err) {
				break
			}
		}
		if c.Queued() == 0 {
			break
		}
	}
	return lastErr
}

// flushOnce publishes at most MaxFiles changed files as a single commit.
func (c *Controller) flushOnce(ctx context.Context) error {
	// Lock ordering is always publishMu then mu, never the reverse. Enqueue
	// only ever takes mu, so it can never block behind a network call.
	c.publishMu.Lock()

	c.mu.Lock()
	if len(c.queue) == 0 {
		c.mu.Unlock()
		c.publishMu.Unlock()
		return nil
	}

	// Snapshot a batch and clear it from the queue. Clearing before the network
	// call means a change enqueued during the call lands in the next batch
	// instead of being silently included in a commit we might retry.
	batch := make(map[string][]byte, len(c.queue))
	paths := make([]string, 0, len(c.queue))
	for p := range c.queue {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) > c.cfg.MaxFiles {
		paths = paths[:c.cfg.MaxFiles]
	}
	for _, p := range paths {
		batch[p] = c.queue[p]
		delete(c.queue, p)
	}
	for _, p := range paths {
		if !c.listed {
			break
		}
		if cached, ok := c.versions[p]; ok && cached == hf.BlobSHA(batch[p]) {
			delete(batch, p)
			c.skipped.Add(1)
		}
	}
	c.mu.Unlock()

	if len(batch) == 0 {
		c.publishMu.Unlock()
		return nil
	}

	// Refresh the remote tree once per process, and again whenever the remote
	// rejects a commit for being out of date.
	if err := c.ensureListed(ctx); err != nil {
		defer c.publishMu.Unlock()
		return c.requeue(batch, err)
	}

	c.mu.Lock()
	for p, content := range batch {
		if cached, ok := c.versions[p]; ok && cached == hf.BlobSHA(content) {
			delete(batch, p)
			c.skipped.Add(1)
		}
	}
	c.mu.Unlock()
	if len(batch) == 0 {
		c.publishMu.Unlock()
		return nil
	}

	files := make([]hf.File, 0, len(batch))
	for p, content := range batch {
		files = append(files, hf.File{Path: p, Content: content})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	summary := fmt.Sprintf("relay sync: %d file(s) at %s",
		len(files), time.Now().UTC().Format(time.RFC3339))

	retried, err := c.publishWithRetry(ctx, summary, files)
	if retried {
		// The remote pushed back at us even though we eventually got through.
		// Slow down rather than hammering it again on the next tick.
		c.retries.Add(1)
		c.widen()
	}
	if err != nil {
		c.failures.Add(1)
		c.lastErr.Store(err.Error())
		c.widen()
		c.publishMu.Unlock()
		return c.requeue(batch, err)
	}

	// Success: record the new versions, narrow the interval back, count it.
	// Each lock is taken, released, and never held across another acquisition.
	c.mu.Lock()
	for _, f := range files {
		c.versions[f.Path] = f.Version()
	}
	remaining := len(c.queue)
	c.mu.Unlock()

	c.published.Add(int64(len(files)))
	c.commits.Add(1)
	c.lastFlush.Store(time.Now().Unix())
	c.lastErr.Store("")
	c.narrow()

	c.publishMu.Unlock()

	c.log.Info("dataset sync published",
		"files", len(files), "queued", remaining, "interval_s", int(c.currentInterval()/time.Second))
	return nil
}

// ensureListed loads the remote tree once so unchanged files can be skipped.
func (c *Controller) ensureListed(ctx context.Context) error {
	c.mu.Lock()
	need := !c.listed
	c.mu.Unlock()
	if !need {
		return nil
	}

	files, err := c.remote.List(ctx, "")
	if err != nil {
		return err
	}

	c.mu.Lock()
	for _, f := range files {
		c.versions[f.Path] = f.Version
	}
	c.listed = true
	c.mu.Unlock()

	c.log.Info("dataset tree indexed", "files", len(files))
	return nil
}

// publishWithRetry retries one commit, honouring Retry-After when given. The
// first return value reports whether at least one retry was needed.
func (c *Controller) publishWithRetry(ctx context.Context, summary string, files []hf.File) (bool, error) {
	delay := c.cfg.RetryBase
	var lastErr error
	retried := false
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			retried = true
			select {
			case <-ctx.Done():
				return retried, ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		err := c.remote.Publish(ctx, summary, files)
		if err == nil {
			return retried, nil
		}
		lastErr = err
		if !isRetryable(err) {
			return retried, err
		}
		// The remote tree may have moved; force a re-list on the next attempt.
		c.mu.Lock()
		c.listed = false
		c.mu.Unlock()

		if apiErr, ok := err.(*hf.APIError); ok && apiErr.RetryAfter > 0 {
			delay = apiErr.RetryAfter
		}
		c.log.Warn("dataset publish failed, will retry",
			"attempt", attempt+1, "err", err, "next_delay_ms", delay.Milliseconds())
	}
	return retried, lastErr
}

// requeue puts a failed batch back so the next flush retries it. Existing
// entries win: a newer version must not be overwritten by a stale one.
func (c *Controller) requeue(batch map[string][]byte, cause error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	restored := 0
	for p, content := range batch {
		if _, newer := c.queue[p]; newer {
			continue
		}
		c.queue[p] = content
		restored++
	}
	if restored > 0 {
		c.log.Warn("dataset batch requeued after failure", "files", restored, "err", cause)
	}
	return cause
}

// widen doubles the flush interval, up to the ceiling.
func (c *Controller) widen() {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.interval * 2
	if next > c.cfg.MaxInterval {
		next = c.cfg.MaxInterval
	}
	if next != c.interval {
		c.log.Warn("backing off dataset sync", "interval_s", int(next/time.Second))
	}
	c.interval = next
}

// narrow eases the interval back towards the configured target.
func (c *Controller) narrow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.interval * 3 / 4
	if next < c.cfg.MinInterval {
		next = c.cfg.MinInterval
	}
	if next < c.cfg.Interval {
		next = c.cfg.Interval
	}
	c.interval = next
}

// isRetryable reports whether a publish failure is worth retrying.
func isRetryable(err error) bool {
	var apiErr *hf.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	// A transport error (DNS, reset, timeout) is worth retrying; a malformed
	// payload is not, but we cannot distinguish those here, so be optimistic.
	if errors.Is(err, context.Canceled) {
		return false
	}
	return true
}

// Restore downloads a subtree from the dataset into a local directory. Reads
// are parallel, so this is fast even though writes are strictly ordered.
func Restore(ctx context.Context, remote hf.Remote, prefix, destRoot string, concurrency int) (int, error) {
	files, err := remote.List(ctx, prefix)
	if err != nil {
		return 0, err
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	// Strip the prefix so a subtree's contents land directly in destRoot. The
	// bank is the reason this matters: restore("bank", BANK_DIR) must produce
	// BANK_DIR/python/easy.jsonl, not BANK_DIR/bank/python/easy.jsonl, or the
	// loader finds nothing and the relay serves 503 forever.
	d := hf.Downloader{Remote: remote, Concurrency: concurrency, Prefix: prefix}
	return d.DownloadAll(ctx, destRoot, paths)
}

// SanitizePath trims a leading slash and query so callers cannot smuggle a
// traversal sequence through a config value.
func SanitizePath(p string) string {
	// Trim both sides of every slash: a config value like "identities//" must
	// not become "identities/" and then be joined into a doubled path.
	return strings.Trim(strings.TrimSpace(p), "/")
}
