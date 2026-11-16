package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/bank"
	"github.com/archnemix/archx-eval/relay/internal/config"
	"github.com/archnemix/archx-eval/relay/internal/core"
	"github.com/archnemix/archx-eval/relay/internal/ratelog"
	"github.com/archnemix/archx-eval/relay/internal/reputation"
	"github.com/archnemix/archx-eval/relay/internal/store"
)

func newServer(t *testing.T, examples int) (*Server, *core.Service) {
	t.Helper()
	return newServerWith(t, examples, true)
}

// newServerWith builds a server whose loopback/private-address relaxation can
// be turned off, so the production posture is testable.
func newServerWith(t *testing.T, examples int, allowLoopback bool) (*Server, *core.Service) {
	t.Helper()
	dir := t.TempDir()
	bankDir := filepath.Join(dir, "bank")
	if err := os.MkdirAll(filepath.Join(bankDir, "python"), 0o750); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < examples; i++ {
		fmt.Fprintf(&b, `{"id":"py-%03d","category":"python","difficulty":"easy","kind":"qa","prompt":"q%d","expected_answer":"secret-answer-%d","explanation":"hidden"}`+"\n", i, i, i)
	}
	if err := os.WriteFile(filepath.Join(bankDir, "python", "easy.jsonl"), []byte(b.String()), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		BankDir: bankDir, DataDir: filepath.Join(dir, "data"),
		RateDir: filepath.Join(dir, "rate"),
		DrawMax: 75, StaleAfterUses: 3, Window: time.Hour,
		RateAllowance: 0.5, RateWindow: time.Minute, RateSample: time.Second, RateFloor: 10,
		BanWindow: 24 * time.Hour, MaxBan: 14 * 24 * time.Hour,
		FeedbackWindow: 24 * time.Hour, LedgerKeepDays: 7,
		SaltSecret: "test-salt", MaxBodyBytes: 1 << 20, AllowLoopback: allowLoopback,
		AccessToken: "test-access-token", MetricsToken: "metrics-token",
		NetWindow: time.Hour, NetMaxDraws: 1000,
		GlobalWindow: time.Hour, GlobalMax: 100000,
		RequestTimeout: 10 * time.Second,
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	bm, err := bank.Load(bankDir, 3)
	if err != nil {
		t.Fatal(err)
	}
	rep, _ := reputation.Load("")
	rates, err := ratelog.Open(cfg.RateDir, 96*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rates.Close() })
	svc := core.New(cfg, st, bm, rep, rates)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, svc, log, "admin-token"), svc
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-access-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const drawBody = `{"categories":["python"],"size":5,"device":{"machine_id":"4c4c4544-0044-3510-8054-b4c04f4e4e32","disk_uuid":"9f1b2c3d4e5f60718293a4b5c6d7e8f9","os_family":"linux","arch":"x86_64"}}`

// drawnGrant is the subset of the draw response needed to build valid feedback.
type drawnGrant struct {
	RequestID  string `json:"request_id"`
	IdentityID string `json:"identity_id"`
	Examples   []struct {
		ID         string `json:"id"`
		Category   string `json:"category"`
		Difficulty string `json:"difficulty"`
	} `json:"examples"`
}

// drawFor performs a real draw, so feedback tests can report against ids that
// were actually issued. Verification makes a fabricated request id impossible
// to use, which is the point of the change.
func drawFor(t *testing.T, srv *Server, remoteAddr string) drawnGrant {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/draw", bytes.NewBufferString(drawBody))
	req.RemoteAddr = remoteAddr + ":5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-access-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("draw failed: %d %s", rec.Code, rec.Body)
	}
	var g drawnGrant
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDrawHappyPath(t *testing.T) {
	srv, _ := newServer(t, 20)
	rec := post(t, srv, "/v1/draw", drawBody)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Count    int              `json:"count"`
		Examples []map[string]any `json:"examples"`
		ByCat    map[string]int   `json:"by_category"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Count != 5 || len(out.Examples) != 5 {
		t.Fatalf("expected 5 examples, got %d", out.Count)
	}
	if out.ByCat["python"] != 5 {
		t.Fatalf("bad by_category: %v", out.ByCat)
	}
}

func TestDrawNeverLeaksAnswers(t *testing.T) {
	srv, _ := newServer(t, 20)
	rec := post(t, srv, "/v1/draw", drawBody)
	body := rec.Body.String()
	for _, forbidden := range []string{"secret-answer", "hidden", "expected_answer", "explanation", "reference_solution"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, body)
		}
	}
}

func TestDrawSetsRequestIDHeader(t *testing.T) {
	srv, _ := newServer(t, 10)
	rec := post(t, srv, "/v1/draw", drawBody)
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("expected an X-Request-Id header")
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("expected no-store cache header, got %q", got)
	}
}

func TestSecondDrawGets429WithRetryAfter(t *testing.T) {
	srv, _ := newServer(t, 20)
	if rec := post(t, srv, "/v1/draw", drawBody); rec.Code != 200 {
		t.Fatalf("first: %d", rec.Code)
	}
	rec := post(t, srv, "/v1/draw", drawBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header")
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Error.Code != "WINDOW_ACTIVE" {
		t.Fatalf("unexpected code %q", out.Error.Code)
	}
}

func TestMalformedJSONRejected(t *testing.T) {
	srv, _ := newServer(t, 10)
	rec := post(t, srv, "/v1/draw", `{"categories":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	srv, _ := newServer(t, 10)
	rec := post(t, srv, "/v1/draw", `{"categories":["python"],"size":1,"surprise":true,"device":{"machine_id":"4c4c4544-0044-3510-8054-b4c04f4e4e32","disk_uuid":"9f1b2c3d4e5f60718293a4b5c6d7e8f9"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d", rec.Code)
	}
}

func TestOversizedBodyRejected(t *testing.T) {
	srv, _ := newServer(t, 10)
	big := strings.Repeat("a", 2<<20)
	rec := post(t, srv, "/v1/draw", `{"categories":["`+big+`"]}`)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected rejection, got %d", rec.Code)
	}
}

func TestHealthReflectsBank(t *testing.T) {
	srv, _ := newServer(t, 7)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Fatalf("expected ok true: %v", out)
	}
}

func TestHealthUnavailableWhenBankEmpty(t *testing.T) {
	srv, _ := newServer(t, 0)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestCategories(t *testing.T) {
	srv, _ := newServer(t, 3)
	req := httptest.NewRequest(http.MethodGet, "/v1/categories", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var out struct {
		Categories []string `json:"categories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Categories) != 1 || out.Categories[0] != "python" {
		t.Fatalf("unexpected categories: %v", out.Categories)
	}
}

func TestAdminStaleRequiresToken(t *testing.T) {
	srv, _ := newServer(t, 5)
	req := httptest.NewRequest(http.MethodGet, "/v1/stale", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without token, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/stale", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with token, got %d: %s", rec.Code, rec.Body)
	}
}

// Feedback is now verified against the ledger, so this test does a real draw
// and reports against the ids that draw actually issued.
func TestFeedbackAcceptedAndScored(t *testing.T) {
	srv, _ := newServer(t, 5)

	grant := drawFor(t, srv, "203.0.113.10")
	if len(grant.Examples) < 2 {
		t.Fatalf("expected at least 2 examples, got %d", len(grant.Examples))
	}
	var items []string
	for i, ex := range grant.Examples {
		passed := i == 0
		score := 0
		if passed {
			score = 1
		}
		items = append(items, fmt.Sprintf(
			`{"example_id":%q,"category":%q,"difficulty":%q,"passed":%v,"score":%d}`,
			ex.ID, ex.Category, ex.Difficulty, passed, score))
	}
	body := fmt.Sprintf(`{"request_id":%q,"identity_id":%q,"model_id":"gpt-x","items":[%s]}`,
		grant.RequestID, grant.IdentityID, strings.Join(items, ","))

	rec := post(t, srv, "/v1/feedback", body)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Accepted bool `json:"accepted"`
		Summary  struct {
			Total  int     `json:"total"`
			Passed int     `json:"passed"`
			Score  float64 `json:"score"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Accepted || out.Summary.Total != len(grant.Examples) || out.Summary.Passed != 1 {
		t.Fatalf("unexpected summary: %+v", out)
	}
}

func TestFeedbackRejectsBadInput(t *testing.T) {
	srv, _ := newServer(t, 5)
	if rec := post(t, srv, "/v1/feedback", `{"items":[{"example_id":"a"}]}`); rec.Code != 400 {
		t.Fatalf("expected 400 for missing model_id, got %d", rec.Code)
	}
	if rec := post(t, srv, "/v1/feedback", `{"model_id":"m","items":[]}`); rec.Code != 400 {
		t.Fatalf("expected 400 for empty items, got %d", rec.Code)
	}
}

func TestEraseRemovesRawOnly(t *testing.T) {
	srv, svc := newServer(t, 10)
	rec := post(t, srv, "/v1/draw", drawBody)
	var out struct {
		IdentityID string `json:"identity_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	body := `{"identity_id":"` + out.IdentityID + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/erase", bytes.NewBufferString(body))
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
	if _, err := svc.Erase(out.IdentityID); err != nil {
		t.Fatalf("erase: %v", err)
	}
}

func TestIndexAnd404(t *testing.T) {
	srv, _ := newServer(t, 3)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "archx-eval relay") {
		t.Fatalf("bad index: %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
