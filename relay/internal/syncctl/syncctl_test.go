package syncctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/hf"
)

// fakeRemote records every commit so the tests can assert on what was sent.
type fakeRemote struct {
	mu            sync.Mutex
	existing      map[string]string // path -> blob sha
	publishes     []publishCall
	failWith      error
	failTimes     int
	concurrent    int32
	maxConcurrent int32
	downloads     int32
}

type publishCall struct {
	summary string
	files   []hf.File
}

func newFake() *fakeRemote {
	return &fakeRemote{existing: map[string]string{}}
}

func (f *fakeRemote) List(context.Context, string) ([]hf.RemoteFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []hf.RemoteFile
	for p, v := range f.existing {
		out = append(out, hf.RemoteFile{Path: p, Version: v})
	}
	return out, nil
}

func (f *fakeRemote) Publish(_ context.Context, summary string, files []hf.File) error {
	now := atomic.AddInt32(&f.concurrent, 1)
	for {
		prev := atomic.LoadInt32(&f.maxConcurrent)
		if now <= prev || atomic.CompareAndSwapInt32(&f.maxConcurrent, prev, now) {
			break
		}
	}
	defer atomic.AddInt32(&f.concurrent, -1)

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTimes > 0 {
		f.failTimes--
		return f.failWith
	}
	f.publishes = append(f.publishes, publishCall{summary: summary, files: files})
	for _, file := range files {
		f.existing[file.Path] = file.Version()
	}
	return nil
}

func (f *fakeRemote) Download(_ context.Context, path string) ([]byte, error) {
	atomic.AddInt32(&f.downloads, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.existing[path]; !ok {
		return nil, &hf.APIError{Status: http.StatusNotFound, Message: "missing"}
	}
	return []byte("content of " + path), nil
}

func (f *fakeRemote) calls() []publishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]publishCall, len(f.publishes))
	copy(out, f.publishes)
	return out
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig() Config {
	c := DefaultConfig()
	c.Interval = 20 * time.Millisecond
	c.MinInterval = 10 * time.Millisecond
	c.MaxInterval = 80 * time.Millisecond
	c.MaxFiles = 3
	c.RetryBase = time.Millisecond
	c.MaxRetries = 2
	return c
}

func TestEnqueueCoalescesBatches(t *testing.T) {
	remote := newFake()
	c := New(testConfig(), remote, quietLogger())

	for i := 0; i < 7; i++ {
		c.Enqueue(fmt.Sprintf("f%d.json", i), []byte(fmt.Sprintf("v%d", i)))
	}
	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	calls := remote.calls()
	// MaxFiles is 3, so 7 files must arrive as 3 commits.
	if len(calls) != 3 {
		t.Fatalf("expected 3 batched commits, got %d", len(calls))
	}
	total := 0
	for _, call := range calls {
		total += len(call.files)
		if len(call.files) > 3 {
			t.Fatalf("batch exceeded MaxFiles: %d", len(call.files))
		}
	}
	if total != 7 {
		t.Fatalf("expected 7 files published, got %d", total)
	}
}

func TestDuplicatePathKeepsNewest(t *testing.T) {
	remote := newFake()
	c := New(testConfig(), remote, quietLogger())

	for i := 0; i < 5; i++ {
		c.Enqueue("same.json", []byte(fmt.Sprintf("version-%d", i)))
	}
	if c.Queued() != 1 {
		t.Fatalf("queue should deduplicate to 1 path, got %d", c.Queued())
	}
	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := remote.calls()
	if len(calls) != 1 || len(calls[0].files) != 1 {
		t.Fatalf("expected one commit with one file, got %+v", calls)
	}
	if string(calls[0].files[0].Content) != "version-4" {
		t.Fatalf("expected the newest version, got %q", calls[0].files[0].Content)
	}
}

func TestUnchangedFilesAreSkipped(t *testing.T) {
	remote := newFake()
	c := New(testConfig(), remote, quietLogger())

	c.Enqueue("a.json", []byte("hello"))
	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Same content again: no second commit should be made.
	c.Enqueue("a.json", []byte("hello"))
	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(remote.calls()); got != 1 {
		t.Fatalf("unchanged content must not be republished, got %d commits", got)
	}
	if c.Stats().Skipped == 0 {
		t.Fatal("expected the skip counter to move")
	}

	// Changed content must publish.
	c.Enqueue("a.json", []byte("hello2"))
	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(remote.calls()); got != 2 {
		t.Fatalf("changed content must publish, got %d commits", got)
	}
}

// slowRemote widens the race window so the serialisation guarantee is actually
// exercised rather than merely plausible. An earlier version of the controller
// passed a fast fake while still allowing six concurrent commits.
type slowRemote struct {
	*fakeRemote
	inFlight int32
	peak     int32
	calls    int32
}

func (s *slowRemote) Publish(ctx context.Context, summary string, files []hf.File) error {
	now := atomic.AddInt32(&s.inFlight, 1)
	defer atomic.AddInt32(&s.inFlight, -1)
	for {
		prev := atomic.LoadInt32(&s.peak)
		if now <= prev || atomic.CompareAndSwapInt32(&s.peak, prev, now) {
			break
		}
	}
	atomic.AddInt32(&s.calls, 1)
	time.Sleep(15 * time.Millisecond)
	return nil
}

func TestPublishesAreSerialised(t *testing.T) {
	remote := &slowRemote{fakeRemote: newFake()}
	cfg := testConfig()
	cfg.MaxFiles = 1
	c := New(cfg, remote, quietLogger())

	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				c.Enqueue(fmt.Sprintf("w%d-%d.json", worker, i), []byte(fmt.Sprint(i)))
				_ = c.FlushNow(context.Background())
			}
		}(w)
	}
	wg.Wait()

	if peak := atomic.LoadInt32(&remote.peak); peak > 1 {
		t.Fatalf("commits overlapped; peak concurrency = %d (serialisation is broken)", peak)
	}
	if atomic.LoadInt32(&remote.calls) == 0 {
		t.Fatal("no commit was attempted, so the test proved nothing")
	}
}

func TestRetryThenSuccess(t *testing.T) {
	remote := newFake()
	remote.failTimes = 2
	remote.failWith = &hf.APIError{Status: http.StatusTooManyRequests, Message: "rate limited"}

	c := New(testConfig(), remote, quietLogger())
	c.Enqueue("retry.json", []byte("x"))
	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if got := len(remote.calls()); got != 1 {
		t.Fatalf("expected one successful commit, got %d", got)
	}
	if c.Stats().Retries == 0 {
		t.Fatal("a retried publish should be counted as a retry")
	}
	if c.Stats().Commits != 1 {
		t.Fatalf("expected exactly one succeeded commit, got %d", c.Stats().Commits)
	}
}

func TestPermanentFailureRequeues(t *testing.T) {
	remote := newFake()
	remote.failTimes = 100
	remote.failWith = &hf.APIError{Status: http.StatusUnauthorized, Message: "bad token"}

	c := New(testConfig(), remote, quietLogger())
	c.Enqueue("perm.json", []byte("x"))
	if err := c.FlushNow(context.Background()); err == nil {
		t.Fatal("expected an error for an unauthorised publish")
	}
	if c.Queued() != 1 {
		t.Fatalf("failed batch must be requeued, queue=%d", c.Queued())
	}
	if opts := c.Stats().LastError; opts == "" {
		t.Fatal("last error should be recorded")
	}
}

func TestBackoffWidensThenNarrows(t *testing.T) {
	remote := newFake()
	// A non-retryable error means the batch fails outright, which must widen
	// the interval so a broken token does not hammer the API every tick.
	remote.failTimes = 1
	remote.failWith = &hf.APIError{Status: http.StatusForbidden, Message: "forbidden"}

	c := New(testConfig(), remote, quietLogger())
	base := c.currentInterval()

	c.Enqueue("b.json", []byte("x"))
	if err := c.FlushNow(context.Background()); err == nil {
		t.Fatal("expected the flush to fail")
	}
	widened := c.currentInterval()
	if widened <= base {
		t.Fatalf("interval should widen on failure: base=%v now=%v", base, widened)
	}

	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.currentInterval() >= widened {
		t.Fatal("interval should narrow after a success")
	}
}

func TestRateLimitRetryStillBacksOff(t *testing.T) {
	remote := newFake()
	remote.failTimes = 1
	remote.failWith = &hf.APIError{Status: http.StatusTooManyRequests, Message: "slow down"}

	c := New(testConfig(), remote, quietLogger())
	base := c.currentInterval()
	c.Enqueue("rl.json", []byte("x"))

	if err := c.FlushNow(context.Background()); err != nil {
		t.Fatalf("retry should have rescued the batch: %v", err)
	}
	if c.currentInterval() <= base {
		t.Fatal("being rate limited must widen the interval even when the retry succeeds")
	}
	if c.Stats().Retries == 0 {
		t.Fatal("the retry should be counted")
	}
}

func TestQueueBudgetDropsInsteadOfGrowing(t *testing.T) {
	remote := newFake()
	cfg := testConfig()
	cfg.MaxQueued = 10
	c := New(cfg, remote, quietLogger())

	for i := 0; i < 100; i++ {
		c.Enqueue(fmt.Sprintf("f%d", i), []byte("x"))
	}
	if c.Queued() > 10 {
		t.Fatalf("queue must stay bounded, got %d", c.Queued())
	}
	if c.Stats().Dropped == 0 {
		t.Fatal("drops must be counted")
	}
}

func TestRunLoopFlushesOnShutdown(t *testing.T) {
	remote := newFake()
	c := New(testConfig(), remote, quietLogger())
	c.Enqueue("shutdown.json", []byte("value"))

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { c.Run(ctx); close(finished) }()

	cancel() // behaves like SIGTERM: the final flush must still happen
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit")
	}
	if got := len(remote.calls()); got != 1 {
		t.Fatalf("queued work must be flushed on shutdown, got %d commits", got)
	}
}

func TestRunLoopPublishesPeriodically(t *testing.T) {
	remote := newFake()
	c := New(testConfig(), remote, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	c.Enqueue("periodic.json", []byte("v"))
	deadline := time.After(2 * time.Second)
	for {
		if len(remote.calls()) == 1 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("periodic flush never happened")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRestoreDownloadsInParallel(t *testing.T) {
	remote := newFake()
	remote.existing = map[string]string{
		"identities/ab/one.json": hf.BlobSHA([]byte("1")),
		"identities/ab/two.json": hf.BlobSHA([]byte("2")),
		"events/day.jsonl":       hf.BlobSHA([]byte("3")),
	}
	dir := t.TempDir()
	n, err := Restore(context.Background(), remote, "", dir, 4)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 files restored, got %d", n)
	}
	if atomic.LoadInt32(&remote.downloads) != 3 {
		t.Fatalf("expected 3 downloads, got %d", remote.downloads)
	}
}

func TestBlobSHAIsStable(t *testing.T) {
	a := hf.BlobSHA([]byte("hello"))
	b := hf.BlobSHA([]byte("hello"))
	c := hf.BlobSHA([]byte("hello!"))
	if a != b {
		t.Fatal("blob hash must be deterministic")
	}
	if a == c {
		t.Fatal("different content must hash differently")
	}
	if len(a) != 40 {
		t.Fatalf("expected a 40-char git sha, got %d", len(a))
	}
}

func TestSanitizePath(t *testing.T) {
	cases := map[string]string{
		"/identities/": "identities",
		"identities//": "identities",
		"  raw  ":      "raw",
	}
	for in, want := range cases {
		if got := SanitizePath(in); got != want {
			t.Fatalf("SanitizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRetryableClassification(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	for _, status := range retryable {
		err := &hf.APIError{Status: status}
		if !isRetryable(err) {
			t.Fatalf("status %d should be retryable", status)
		}
	}
	permanent := []int{400, 401, 403, 404, 422}
	for _, status := range permanent {
		err := &hf.APIError{Status: status}
		if isRetryable(err) {
			t.Fatalf("status %d should not be retried", status)
		}
	}
	if isRetryable(context.Canceled) {
		t.Fatal("cancellation must not be retried")
	}
	if !isRetryable(errors.New("connection reset")) {
		t.Fatal("transport errors should be retried")
	}
}
