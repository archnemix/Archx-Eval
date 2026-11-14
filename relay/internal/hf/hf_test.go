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
