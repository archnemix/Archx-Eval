package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/archnemix/archx-eval/relay/internal/identity"
)

// --- authentication -------------------------------------------------------

func TestDrawRequiresToken(t *testing.T) {
	srv, _ := newServer(t, 20)
	req := httptest.NewRequest(http.MethodPost, "/v1/draw", bytes.NewBufferString(drawBody))
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without a token, got %d", rec.Code)
	}
}

func TestDrawRejectsWrongToken(t *testing.T) {
	srv, _ := newServer(t, 20)
	req := httptest.NewRequest(http.MethodPost, "/v1/draw", bytes.NewBufferString(drawBody))
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestDrawAcceptsAlternateTokenHeader(t *testing.T) {
	srv, _ := newServer(t, 20)
	req := httptest.NewRequest(http.MethodPost, "/v1/draw", bytes.NewBufferString(drawBody))
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Access-Token", "test-access-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body)
	}
}

// --- transport hardening --------------------------------------------------

func TestSecurityHeadersPresent(t *testing.T) {
	srv, _ := newServer(t, 10)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Strict-Transport-Security":    "max-age=31536000; includeSubDomains",
	}
	for k, v := range want {
		if got := rec.Header().Get(k); got != v {
			t.Fatalf("header %s = %q, want %q", k, got, v)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Fatalf("weak CSP: %q", csp)
	}
}

func TestNonJSONContentTypeRejected(t *testing.T) {
	srv, _ := newServer(t, 10)
	req := httptest.NewRequest(http.MethodPost, "/v1/draw", bytes.NewBufferString(drawBody))
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("Authorization", "Bearer test-access-token")
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for wrong content type, got %d", rec.Code)
	}
}

func TestOversizedBodyRejectedBeforeParsing(t *testing.T) {
	srv, _ := newServer(t, 10)
	huge := strings.Repeat("x", 3<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/draw", strings.NewReader(`{"categories":["`+huge+`"]}`))
	req.RemoteAddr = "203.0.113.10:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-access-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected body rejection, got %d", rec.Code)
	}
}

func TestClientRequestIDCannotBeUnbounded(t *testing.T) {
	srv, _ := newServer(t, 10)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("X-Request-Id", strings.Repeat("a", 500))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	got := rec.Header().Get("X-Request-Id")
	if len(got) > 64 {
		t.Fatalf("client-supplied request id echoed unbounded: %d chars", len(got))
	}
}

// --- proxy / rate-limit bypass --------------------------------------------

func TestSpoofedForwardedForDoesNotBypassWindow(t *testing.T) {
	srv, _ := newServer(t, 50)
	if rec := post(t, srv, "/v1/draw", drawBody); rec.Code != 200 {
		t.Fatalf("first draw: %d", rec.Code)
	}
	// A classic bypass attempt: keep the same device but claim a fresh IP each
	// time. The marker set must still recognise the caller.
	for i := 0; i < 5; i++ {
		ip := "198.51." + string(rune('1'+i)) + ".77"
		req := httptest.NewRequest(http.MethodPost, "/v1/draw", bytes.NewBufferString(drawBody))
		req.RemoteAddr = "203.0.113.10:5555"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-access-token")
		req.Header.Set(identity.HeaderForwardedFor, ip+", 10.0.0.1")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code == 200 {
			t.Fatalf("spoofed X-Forwarded-For granted a second draw")
		}
	}
}

func TestPrivateClientAddressRefused(t *testing.T) {
	// Production posture: loopback and RFC1918 clients are refused, because a
	// private address on a public request cannot be attributed or limited.
	srv, _ := newServerWith(t, 50, false)
	req := httptest.NewRequest(http.MethodPost, "/v1/draw", bytes.NewBufferString(drawBody))
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-access-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected private address to be refused, got %d: %s", rec.Code, rec.Body)
	}
}

// --- input abuse ----------------------------------------------------------

func TestCategoryFloodRejected(t *testing.T) {
	srv, _ := newServer(t, 20)
	cats := make([]string, 40)
	for i := range cats {
		cats[i] = "python"
	}
	payload, _ := json.Marshal(map[string]any{
		"categories": cats,
		"size":       1,
		"device": map[string]any{
			"machine_id": "4c4c4544-0044-3510-8054-b4c04f4e4e32",
			"disk_uuid":  "9f1b2c3d4e5f60718293a4b5c6d7e8f9",
		},
	})
	rec := post(t, srv, "/v1/draw", string(payload))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected category flood to be rejected, got %d", rec.Code)
	}
}

func TestFeedbackCannotBeUsedAsFreeStorage(t *testing.T) {
	srv, _ := newServer(t, 10)
	huge := strings.Repeat("A", 100<<10)
	body, _ := json.Marshal(map[string]any{
		"model_id": "m",
		"items": []map[string]any{{
			"example_id": "py-000", "category": "python", "difficulty": "easy",
			"passed": true, "score": 1, "code": huge,
		}},
	})
	rec := post(t, srv, "/v1/feedback", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected oversize code to be rejected, got %d", rec.Code)
	}
}

func TestErrorResponsesDoNotLeakInternals(t *testing.T) {
	srv, _ := newServer(t, 10)
	// Unknown category produces an error; it must not mention files or types.
	rec := post(t, srv, "/v1/draw", `{"categories":["nope"],"size":1,"device":{"machine_id":"4c4c4544-0044-3510-8054-b4c04f4e4e32","disk_uuid":"9f1b2c3d4e5f60718293a4b5c6d7e8f9"}}`)
	body := strings.ToLower(rec.Body.String())
	for _, bad := range []string{"/home/", "goroutine", "panic", ".go:", "sql", "stack"} {
		if strings.Contains(body, bad) {
			t.Fatalf("error response leaked %q: %s", bad, rec.Body)
		}
	}
}

// --- metrics --------------------------------------------------------------

func TestMetricsRequiresToken(t *testing.T) {
	srv, _ := newServer(t, 10)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without metrics token, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer metrics-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with metrics token, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "archx_bank_examples_total") {
		t.Fatalf("metrics body missing expected series: %s", rec.Body)
	}
}

func TestMetricsDisabledWhenUnset(t *testing.T) {
	srv, _ := newServer(t, 10)
	srv.metrics = ""
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected metrics to be disabled, got %d", rec.Code)
	}
}
