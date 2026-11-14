// Package hf publishes relay state to the private Hugging Face dataset.
//
// Two sinks exist because the system has to work in three places:
//
//   - CommitSink talks to the Hugging Face commit API. Used in production.
//   - DirSink writes the same tree locally. Used by tests and by CI, which
//     then commits the mirror into the repository under ci/outputs/.
//
// Nothing here ever stores a token in a file or a log line.
package hf

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// File is one artefact destined for the dataset.
type File struct {
	Path    string // repo-relative, e.g. "raw/ab/abc.json"
	Content []byte
}

// Sink publishes files atomically as a single commit where possible.
type Sink interface {
	Publish(ctx context.Context, summary string, files []File) error
}

// DirSink writes files under a local root. Used in CI and tests.
type DirSink struct{ Root string }

// Publish writes every file, creating directories as needed.
func (d *DirSink) Publish(_ context.Context, _ string, files []File) error {
	if d.Root == "" {
		return fmt.Errorf("hf: DirSink has no root")
	}
	for _, f := range files {
		if err := safeJoinCheck(f.Path); err != nil {
			return err
		}
		full := filepath.Join(d.Root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(full, f.Content, 0o640); err != nil {
			return err
		}
	}
	return nil
}

// CommitSink publishes through the Hugging Face dataset commit API.
type CommitSink struct {
	Repo    string
	Token   string
	BaseURL string
	Branch  string
	Client  *http.Client
}

// NewCommitSink builds a sink, defaulting to the public endpoint.
func NewCommitSink(repo, token, baseURL, branch string) *CommitSink {
	if baseURL == "" {
		baseURL = "https://huggingface.co"
	}
	if branch == "" {
		branch = "main"
	}
	return &CommitSink{
		Repo:    repo,
		Token:   token,
		BaseURL: strings.TrimRight(baseURL, "/"),
		Branch:  branch,
		Client:  &http.Client{Timeout: 30 * time.Second},
	}
}

type commitFile struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type commitRequest struct {
	Summary string       `json:"summary"`
	Files   []commitFile `json:"files"`
}

// Publish sends all files in one commit. The API caps payload size, so callers
// should batch: the sync command sends at most MaxBatch files per call.
func (c *CommitSink) Publish(ctx context.Context, summary string, files []File) error {
	if c.Repo == "" || c.Token == "" {
		return fmt.Errorf("hf: dataset repo and token are both required")
	}
	if len(files) == 0 {
		return nil
	}
	body := commitRequest{Summary: summary, Files: make([]commitFile, 0, len(files))}
	for _, f := range files {
		if err := safeJoinCheck(f.Path); err != nil {
			return err
		}
		body.Files = append(body.Files, commitFile{
			Path:     f.Path,
			Content:  base64.StdEncoding.EncodeToString(f.Content),
			Encoding: "base64",
		})
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/api/datasets/%s/commit/%s", c.BaseURL, c.Repo, c.Branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("hf: commit request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hf: commit failed: %s: %s", resp.Status, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// Collect walks a local directory and returns its files as artefacts.
func Collect(root string, limit int) ([]File, error) {
	var out []File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if limit > 0 && len(out) >= limit {
			return fs.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = append(out, File{Path: filepath.ToSlash(rel), Content: buf})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

// safeJoinCheck rejects paths that could escape the dataset root.
func safeJoinCheck(p string) error {
	if p == "" {
		return fmt.Errorf("hf: empty path")
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || strings.Contains(clean, "/../") {
		return fmt.Errorf("hf: unsafe path %q", p)
	}
	return nil
}

// Disabled is a sink that silently drops everything.
type Disabled struct{}

// Publish does nothing.
func (Disabled) Publish(context.Context, string, []File) error { return nil }
