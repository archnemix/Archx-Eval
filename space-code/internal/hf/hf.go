// Package hf talks to the private Hugging Face dataset that backs this system.
//
// The dataset is a *git repository*, which shapes every design decision here:
//
//   - Writes are sequential. Two commits racing each other produce a rejected
//     push, so all writes must funnel through one controller (see syncctl).
//   - Reads are parallel and cheap, so restoring state at boot downloads files
//     concurrently.
//   - Content is addressed by blob hash. We therefore identify a remote file by
//     its git blob SHA-1, which lets us skip unchanged files with certainty
//     instead of guessing from timestamps.
//   - History is permanent. Anything committed stays in the repository's past
//     for ever, even after it is deleted from the current tree. That is why
//     raw personal values are never published here: only salted hashes are.
//
// Two implementations exist: CommitSink targets the real API, DirSink targets a
// local directory so the controller can be tested without a network.
package hf

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// File is one artefact destined for the dataset.
type File struct {
	Path    string // repo-relative, e.g. "identities/ab/abcd.json"
	Content []byte
}

// Version is the content identity of a file: the git blob SHA-1 of its bytes.
// Hugging Face reports the same value as "oid" when listing a repo tree, so
// comparing these two numbers tells us exactly whether a file changed.
func (f File) Version() string { return BlobSHA(f.Content) }

// BlobSHA computes the git blob SHA-1 of content, matching the oid the
// Hugging Face API returns for a stored file.
func BlobSHA(content []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// RemoteFile describes a file already in the dataset.
type RemoteFile struct {
	Path    string
	Version string // git blob oid, or "" when unknown
	Size    int64
}

// Remote is the read/write view of the dataset the controller needs.
type Remote interface {
	// List returns every file under prefix. An empty prefix means the whole
	// repository.
	List(ctx context.Context, prefix string) ([]RemoteFile, error)
	// Publish creates a single commit containing all files (upserts).
	Publish(ctx context.Context, summary string, files []File) error
	// Download fetches one file's bytes.
	Download(ctx context.Context, path string) ([]byte, error)
}

// ---------------------------------------------------------------------------
// DirSink: a filesystem stand-in for the dataset.
// ---------------------------------------------------------------------------

// DirSink mirrors the dataset into a local directory. It is used by tests and
// by CI, which commits the mirror into the repository for auditability.
type DirSink struct{ Root string }

// List walks the directory and reports every file's blob hash.
func (d *DirSink) List(_ context.Context, prefix string) ([]RemoteFile, error) {
	if d.Root == "" {
		return nil, fmt.Errorf("hf: DirSink has no root")
	}
	base := filepath.Join(d.Root, filepath.FromSlash(prefix))
	var out []RemoteFile
	err := filepath.WalkDir(base, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(d.Root, p)
		if err != nil {
			return err
		}
		buf, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, RemoteFile{
			Path:    filepath.ToSlash(rel),
			Version: BlobSHA(buf),
			Size:    int64(len(buf)),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

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

// Download reads one file from the mirror.
func (d *DirSink) Download(_ context.Context, path string) ([]byte, error) {
	if err := safeJoinCheck(path); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(d.Root, filepath.FromSlash(path)))
}

// ---------------------------------------------------------------------------
// CommitSink: the real dataset API.
// ---------------------------------------------------------------------------

// CommitSink publishes through the Hugging Face dataset API.
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
		Client:  &http.Client{Timeout: 60 * time.Second},
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

// Publish sends all files in one commit.
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
		return &APIError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Message:    fmt.Sprintf("commit failed: %s: %s", resp.Status, strings.TrimSpace(string(snippet))),
		}
	}
	return nil
}

// List reads the repository tree under prefix. Reads are safe to run in
// parallel with each other, which is what the restore path relies on.
func (c *CommitSink) List(ctx context.Context, prefix string) ([]RemoteFile, error) {
	if c.Repo == "" {
		return nil, fmt.Errorf("hf: dataset repo is required")
	}
	var out []RemoteFile
	next := ""
	for page := 0; page < 200; page++ {
		q := url.Values{}
		q.Set("recursive", "true")
		q.Set("expand", "false")
		if prefix != "" {
			q.Set("path", prefix)
		}
		if next != "" {
			q.Set("cursor", next)
		}
		endpoint := fmt.Sprintf("%s/api/datasets/%s/tree/%s?%s",
			c.BaseURL, c.Repo, c.Branch, q.Encode())

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		c.auth(req)
		resp, err := c.Client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("hf: list request failed: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusNotFound {
			// An empty repository has no tree yet.
			return out, nil
		}
		if resp.StatusCode >= 300 {
			return nil, &APIError{
				Status:     resp.StatusCode,
				RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
				Message:    fmt.Sprintf("list failed: %s: %s", resp.Status, truncate(string(body), 300)),
			}
		}

		var entries []struct {
			Type string `json:"type"`
			Path string `json:"path"`
			Size int64  `json:"size"`
			OID  string `json:"oid"`
			LFS  *struct {
				OID string `json:"oid"`
			} `json:"lfs"`
		}
		if err := json.Unmarshal(body, &entries); err != nil {
			return nil, fmt.Errorf("hf: parse tree: %w", err)
		}
		for _, e := range entries {
			if e.Type != "file" {
				continue
			}
			version := e.OID
			if e.LFS != nil && e.LFS.OID != "" {
				version = e.LFS.OID // large files are stored by LFS pointer
			}
			out = append(out, RemoteFile{Path: e.Path, Version: version, Size: e.Size})
		}

		next = nextCursor(resp.Header.Get("Link"))
		if next == "" {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Download fetches a single file's raw bytes.
func (c *CommitSink) Download(ctx context.Context, path string) ([]byte, error) {
	if err := safeJoinCheck(path); err != nil {
		return nil, err
	}
	if c.Repo == "" {
		return nil, fmt.Errorf("hf: dataset repo is required")
	}
	endpoint := fmt.Sprintf("%s/datasets/%s/resolve/%s/%s",
		c.BaseURL, c.Repo, c.Branch, strings.TrimPrefix(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.auth(req)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hf: download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, &APIError{
			Status:     resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			Message:    fmt.Sprintf("download %s failed: %s: %s", path, resp.Status, truncate(string(body), 200)),
		}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// Ping verifies the token can see the dataset. It reads nothing sensitive and
// returns a message safe to log.
func (c *CommitSink) Ping(ctx context.Context) error {
	if c.Repo == "" || c.Token == "" {
		return fmt.Errorf("hf: dataset repo and token are both required")
	}
	endpoint := fmt.Sprintf("%s/api/datasets/%s", c.BaseURL, c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	c.auth(req)
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("hf: cannot reach the dataset: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("hf: token is not authorised for %s (status %d)", c.Repo, resp.StatusCode)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("hf: dataset %s not found; check the repo id and that the token can see it", c.Repo)
	default:
		return fmt.Errorf("hf: unexpected status %d from %s", resp.StatusCode, c.Repo)
	}
}

func (c *CommitSink) auth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
}

// APIError carries the HTTP status so the controller can decide whether a
// failure is worth retrying.
type APIError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
}

func (e *APIError) Error() string { return "hf: " + e.Message }

// Retryable reports whether retrying could plausibly succeed.
func (e *APIError) Retryable() bool {
	switch e.Status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusInternalServerError:
		return true
	}
	return false
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// nextCursor extracts the cursor from an RFC 5988 Link header.
func nextCursor(link string) string {
	if link == "" {
		return ""
	}
	for _, part := range strings.Split(link, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end <= start {
			continue
		}
		u, err := url.Parse(part[start+1 : end])
		if err != nil {
			continue
		}
		return u.Query().Get("cursor")
	}
	return ""
}

// Collect walks a local directory and returns its files as artefacts.
func Collect(root string, limit int) ([]File, error) {
	var out []File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
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
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// safeJoinCheck rejects paths that could escape the dataset root.
func safeJoinCheck(p string) error {
	if p == "" {
		return fmt.Errorf("hf: empty path")
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") ||
		strings.Contains(clean, "/../") || clean == ".." {
		return fmt.Errorf("hf: unsafe path %q", p)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Downloader fetches a set of files concurrently. Reads are independent, so
// parallelism is safe here even though writes are not.
type Downloader struct {
	Remote      Remote
	Concurrency int

	// Prefix, when set, is stripped from every remote path before it is joined
	// onto destRoot. It exists because callers ask for a subtree ("bank/") but
	// want its *contents* placed directly in the destination directory.
	Prefix string
}

// trimPrefixPath strips a leading subtree from a remote path. It returns false
// when the path names the prefix directory itself or a sibling of it, which the
// caller must skip rather than write outside its destination.
func trimPrefixPath(path, prefix string) (string, bool) {
	if prefix == "" {
		return path, true
	}
	want := strings.Trim(prefix, "/") + "/"
	if !strings.HasPrefix(path, want) {
		return "", false
	}
	rel := strings.TrimPrefix(path, want)
	if rel == "" {
		return "", false
	}
	return rel, true
}

// DownloadAll fetches paths into destRoot, preserving relative paths. It
// returns the number of files written. A missing file is skipped, not fatal,
// because the dataset legitimately lacks files that have never been written.
func (d Downloader) DownloadAll(ctx context.Context, destRoot string, paths []string) (int, error) {
	n := d.Concurrency
	if n <= 0 {
		n = 8
	}
	if n > 32 {
		n = 32
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan string)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		written  int
		firstErr error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range jobs {
				buf, err := d.Remote.Download(ctx, p)
				if err != nil {
					if apiErr, ok := err.(*APIError); ok && apiErr.Status == http.StatusNotFound {
						continue
					}
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					continue
				}
				rel, ok := trimPrefixPath(p, d.Prefix)
				if !ok || safeJoinCheck(rel) != nil {
					continue
				}
				dst := filepath.Join(destRoot, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					continue
				}
				if err := os.WriteFile(dst, buf, 0o640); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					continue
				}
				mu.Lock()
				written++
				mu.Unlock()
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, p := range paths {
			select {
			case <-ctx.Done():
				return
			case jobs <- p:
			}
		}
	}()

	wg.Wait()
	return written, firstErr
}
