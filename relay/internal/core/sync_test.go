package core

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/hf"
	"github.com/archnemix/archx-eval/relay/internal/syncctl"
)

// recordingRemote captures published paths and content for assertions.
type recordingRemote struct {
	mu      sync.Mutex
	written map[string][]byte
}

func newRecorder() *recordingRemote {
	return &recordingRemote{written: map[string][]byte{}}
}

func (r *recordingRemote) List(context.Context, string) ([]hf.RemoteFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]hf.RemoteFile, 0, len(r.written))
	for p, c := range r.written {
		out = append(out, hf.RemoteFile{Path: p, Version: hf.BlobSHA(c)})
	}
	return out, nil
}

func (r *recordingRemote) Publish(_ context.Context, _ string, files []hf.File) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, f := range files {
		r.written[f.Path] = f.Content
	}
	return nil
}

func (r *recordingRemote) Download(_ context.Context, p string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written[p], nil
}

func (r *recordingRemote) snapshot() map[string][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]byte, len(r.written))
	for k, v := range r.written {
		out[k] = v
	}
	return out
}

func attachRecorder(t *testing.T, h *harness) *recordingRemote {
	t.Helper()
	remote := newRecorder()
	sc := syncctl.New(syncctl.Config{
		Interval:  time.Hour, // never fires on its own in tests
		MaxFiles:  100,
		MaxQueued: 10000,
	}, remote, nil)
	h.svc.WithSync(sc)
	t.Cleanup(func() {
		_ = sc.FlushNow(context.Background())
	})
	return remote
}

// The dataset must receive hashes, never raw identifiers.
func TestDatasetReceivesNoRawIdentity(t *testing.T) {
	h := newHarness(t, 50)
	remote := attachRecorder(t, h)

	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatalf("draw: %v", err)
	}
	if err := h.svc.SyncFlush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	published := remote.snapshot()
	if len(published) == 0 {
		t.Fatal("nothing was published")
	}
	for path, content := range published {
		if strings.HasPrefix(path, "raw/") {
			t.Fatalf("raw identity row published to the dataset at %s", path)
		}
		for _, secret := range []string{"203.0.113.10", machA, diskA} {
			if strings.Contains(string(content), secret) {
				t.Fatalf("%s leaked %q to the dataset", path, secret)
			}
		}
	}
}

func TestDatasetReceivesHashedRecord(t *testing.T) {
	h := newHarness(t, 50)
	remote := attachRecorder(t, h)

	grant, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SyncFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	published := remote.snapshot()
	var found []byte
	for path, content := range published {
		if strings.HasPrefix(path, "identities/") && strings.Contains(path, grant.IdentityID) {
			found = content
		}
	}
	if found == nil {
		t.Fatalf("expected the hashed record for %s, published: %v", grant.IdentityID, keys(published))
	}
	var rec map[string]any
	if err := json.Unmarshal(found, &rec); err != nil {
		t.Fatalf("published record is not valid JSON: %v", err)
	}
	if rec["id"] != grant.IdentityID {
		t.Fatalf("unexpected record id: %v", rec["id"])
	}
}

func TestDatasetReceivesEventLog(t *testing.T) {
	h := newHarness(t, 50)
	remote := attachRecorder(t, h)

	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.SyncFlush(context.Background()); err != nil {
		t.Fatal(err)
	}

	var events []byte
	for path, content := range remote.snapshot() {
		if strings.HasPrefix(path, "events/") {
			events = append(events, content...)
		}
	}
	if len(events) == 0 {
		t.Fatal("expected an event log to be published")
	}
	if !strings.Contains(string(events), `"decision":"allow"`) {
		t.Fatalf("event log missing the allow decision: %s", events)
	}
	// The event log must carry hashes only.
	if strings.Contains(string(events), "203.0.113.10") {
		t.Fatal("event log leaked a raw IP")
	}
}

func TestSyncEnqueueNeverBlocksDraw(t *testing.T) {
	h := newHarness(t, 200)
	// A controller whose remote always fails: enqueue must still be instant.
	sc := syncctl.New(syncctl.Config{
		Interval: time.Hour, MaxFiles: 1, MaxQueued: 2,
	}, &blockingRemote{}, nil)
	h.svc.WithSync(sc)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			in := input("203.0.113.10", machA, diskA)
			in.RequestID = "r"
			in.Size = 1
			h.advance(2 * time.Second)
			_, _ = h.svc.RequestDraw(context.Background(), in)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("draws blocked on a failing dataset controller")
	}
}

type blockingRemote struct{}

func (blockingRemote) List(context.Context, string) ([]hf.RemoteFile, error) { return nil, nil }
func (blockingRemote) Publish(context.Context, string, []hf.File) error {
	return context.DeadlineExceeded
}
func (blockingRemote) Download(context.Context, string) ([]byte, error) {
	return nil, context.DeadlineExceeded
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Regression: the one-shot -sync export used its own subtree list and shipped
// raw/ to the dataset, while the live controller correctly excluded it. Both
// paths now read PublishableSubtrees, and this test pins the guarantee.
func TestExportSnapshotExcludesRaw(t *testing.T) {
	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.10", machA, diskA)); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if _, err := h.svc.ExportSnapshot(dir); err != nil {
		t.Fatalf("export: %v", err)
	}

	for _, sub := range PublishableSubtrees {
		if sub == "raw" {
			t.Fatal("raw must never be listed as publishable")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "raw" {
			t.Fatalf("export produced a raw/ subtree at %s", dir)
		}
	}

	// And nothing exported may contain a raw value.
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		buf, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, secret := range []string{"203.0.113.10", machA, diskA} {
			if strings.Contains(string(buf), secret) {
				t.Fatalf("%s leaked %q into the dataset export", p, secret)
			}
		}
		return nil
	})
}

// The bank is the answer key. It must never be a publishable subtree, because
// the dataset is written by this service and the Space repository is public.
func TestBankIsNeverPublishable(t *testing.T) {
	for _, sub := range PublishableSubtrees {
		if sub == "bank" {
			t.Fatal("bank must never appear in PublishableSubtrees: it is the answer key")
		}
	}

	h := newHarness(t, 50)
	if _, err := h.svc.RequestDraw(context.Background(), input("203.0.113.11", machB, diskB)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := h.svc.ExportSnapshot(dir); err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bank")); !os.IsNotExist(err) {
		t.Fatalf("export produced a bank/ subtree (err=%v)", err)
	}

	// Even with examples loaded, no reference solution may reach the snapshot.
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		buf, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(buf, []byte("reference_solution")) {
			t.Fatalf("%s contains a reference solution", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
