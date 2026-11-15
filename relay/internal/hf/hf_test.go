package hf

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDirSinkWritesTree(t *testing.T) {
	root := t.TempDir()
	s := &DirSink{Root: root}
	files := []File{
		{Path: "raw/ab/abc.json", Content: []byte(`{"id":"abc"}`)},
		{Path: "events/2026-11-13.jsonl", Content: []byte("{}\n")},
	}
	if err := s.Publish(context.Background(), "test", files); err != nil {
		t.Fatalf("publish: %v", err)
	}
	buf, err := os.ReadFile(filepath.Join(root, "raw", "ab", "abc.json"))
	if err != nil {
		t.Fatalf("expected file written: %v", err)
	}
	if string(buf) != `{"id":"abc"}` {
		t.Fatalf("unexpected content: %s", buf)
	}
}

func TestDirSinkRejectsTraversal(t *testing.T) {
	s := &DirSink{Root: t.TempDir()}
	if err := s.Publish(context.Background(), "x", []File{{Path: "../escape.json", Content: []byte("x")}}); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
}

func TestCommitSinkSendsBase64Payload(t *testing.T) {
	var got commitRequest
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewCommitSink("Archnemix/Archx-Eval", "hf_secret_token", srv.URL, "main")
	files := []File{{Path: "raw/aa/x.json", Content: []byte("hello")}}
	if err := s.Publish(context.Background(), "sync", files); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if auth != "Bearer hf_secret_token" {
		t.Fatalf("bad auth header: %q", auth)
	}
	if path != "/api/datasets/Archnemix/Archx-Eval/commit/main" {
		t.Fatalf("bad path: %s", path)
	}
	if len(got.Files) != 1 || got.Files[0].Encoding != "base64" {
		t.Fatalf("unexpected files: %+v", got.Files)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.Files[0].Content)
	if err != nil || string(decoded) != "hello" {
		t.Fatalf("bad content: %v %q", err, decoded)
	}
}

func TestCommitSinkSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	s := NewCommitSink("o/r", "bad", srv.URL, "main")
	err := s.Publish(context.Background(), "sync", []File{{Path: "a.json", Content: []byte("x")}})
	if err == nil {
		t.Fatal("expected an error for 401")
	}
}

func TestCommitSinkRequiresCredentials(t *testing.T) {
	s := NewCommitSink("", "", "", "")
	if err := s.Publish(context.Background(), "x", []File{{Path: "a", Content: []byte("b")}}); err == nil {
		t.Fatal("expected missing credentials to fail")
	}
}

func TestCollect(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "raw", "ab"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "raw", "ab", "x.json"), []byte("{}"), 0o640); err != nil {
		t.Fatal(err)
	}
	files, err := Collect(root, 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(files) != 1 || files[0].Path != "raw/ab/x.json" {
		t.Fatalf("unexpected files: %+v", files)
	}
	limited, err := Collect(root, 0)
	if err != nil || len(limited) != 1 {
		t.Fatalf("limit 0 should mean unlimited: %v %d", err, len(limited))
	}
}

// Regression: restore("bank", BANK_DIR) must strip the prefix, otherwise the
// bank lands in BANK_DIR/bank/... and the loader finds nothing. A Space booted
// with that bug serves 503 forever while logging a successful restore.
func TestDownloadAllStripsPrefix(t *testing.T) {
	root := t.TempDir()
	remote := &stubRemote{files: map[string][]byte{
		"bank/python/easy.jsonl": []byte(`{"id":"a"}`),
		"bank/logic/hard.jsonl":  []byte(`{"id":"b"}`),
		"bank/usage.json":        []byte(`{}`),
		"identities/ab/x.json":   []byte(`{}`),
	}}
	d := Downloader{Remote: remote, Prefix: "bank", Concurrency: 2}
	n, err := d.DownloadAll(context.Background(), root, []string{
		"bank/python/easy.jsonl",
		"bank/logic/hard.jsonl",
		"bank/usage.json",
		"identities/ab/x.json", // outside the prefix: must be skipped
	})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if n != 3 {
		t.Fatalf("wrote %d files, want 3 (the sibling path must be skipped)", n)
	}
	for _, rel := range []string{"python/easy.jsonl", "logic/hard.jsonl", "usage.json"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "bank")); !os.IsNotExist(err) {
		t.Errorf("prefix directory was not stripped: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "identities")); !os.IsNotExist(err) {
		t.Errorf("path outside the prefix was written: %v", err)
	}
}

func TestTrimPrefixPath(t *testing.T) {
	cases := []struct {
		path, prefix, want string
		ok                 bool
	}{
		{"bank/python/e.md", "bank", "python/e.md", true},
		{"bank/python/e.md", "bank/", "python/e.md", true},
		{"bank", "bank", "", false},
		{"bank/", "bank", "", false},
		{"bankx/e.md", "bank", "", false},
		{"identities/a.json", "bank", "", false},
		{"anything", "", "anything", true},
	}
	for _, c := range cases {
		got, ok := trimPrefixPath(c.path, c.prefix)
		if ok != c.ok || got != c.want {
			t.Errorf("trimPrefixPath(%q,%q) = (%q,%v), want (%q,%v)",
				c.path, c.prefix, got, ok, c.want, c.ok)
		}
	}
}

// stubRemote serves an in-memory file map for Downloader tests.
type stubRemote struct {
	files map[string][]byte
}

func (s *stubRemote) List(ctx context.Context, prefix string) ([]RemoteFile, error) {
	var out []RemoteFile
	for p := range s.files {
		out = append(out, RemoteFile{Path: p})
	}
	return out, nil
}

func (s *stubRemote) Publish(ctx context.Context, msg string, files []File) error { return nil }

func (s *stubRemote) Download(ctx context.Context, path string) ([]byte, error) {
	b, ok := s.files[path]
	if !ok {
		return nil, &APIError{Status: http.StatusNotFound}
	}
	return b, nil
}
