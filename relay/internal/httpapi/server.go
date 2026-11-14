// Package httpapi is the transport: JSON in, JSON out, with all policy living
// in core. Every error the API can produce is renderable from apierr.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
	"github.com/archnemix/archx-eval/relay/internal/config"
	"github.com/archnemix/archx-eval/relay/internal/core"
)

// Server wraps the core service with HTTP concerns.
type Server struct {
	cfg   config.Config
	svc   *core.Service
	log   *slog.Logger
	mux   *http.ServeMux
	admin string
}

// New builds a Server. The returned value implements http.Handler.
func New(cfg config.Config, svc *core.Service, log *slog.Logger, adminToken string) *Server {
	s := &Server{cfg: cfg, svc: svc, log: log, admin: adminToken}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/categories", s.handleCategories)
	mux.HandleFunc("POST /v1/draw", s.handleDraw)
	mux.HandleFunc("POST /v1/feedback", s.handleFeedback)
	mux.HandleFunc("GET /v1/stale", s.handleStale)
	mux.HandleFunc("POST /v1/erase", s.handleErase)
	s.mux = mux
	return s
}

// ServeHTTP applies the cross-cutting concerns in a fixed order: panic safety,
// request id, size limit, access log.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = newRequestID()
	}
	w.Header().Set("X-Request-Id", reqID)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("panic recovered",
				"request_id", reqID,
				"panic", p,
				"stack", string(debug.Stack()),
			)
			if !rec.wrote {
				writeError(rec, apierr.Internal("unexpected server error"))
			}
		}
		s.log.Info("request",
			"request_id", reqID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip_hash_absent", true,
		)
	}()

	s.mux.ServeHTTP(rec, r)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.wrote = true
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// writeJSON emits a success envelope.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// Headers are already sent; nothing useful remains to do.
		_ = err
	}
}

// writeError renders an apierr.Error, or a generic 500 for anything else.
func writeError(w http.ResponseWriter, err error) {
	e, ok := apierr.As(err)
	if !ok {
		e = apierr.Internal("unexpected server error")
	}
	if e.RetryAfter > 0 {
		secs := int(e.RetryAfter.Round(time.Second) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeJSON(w, e.Status, map[string]any{
		"error": map[string]any{
			"code":    e.Code,
			"message": e.Message,
		},
	})
}

// decodeJSON reads a strict JSON body: unknown fields and trailing data are
// rejected, which stops clients from silently sending something we ignore.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return apierr.BadRequest("request body is too large")
		}
		return apierr.BadRequest("malformed JSON body: " + err.Error())
	}
	if dec.More() {
		return apierr.BadRequest("body must contain exactly one JSON object")
	}
	return nil
}

// requireAdmin gates operational endpoints behind a shared token.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.admin == "" {
		writeError(w, apierr.Forbidden("ADMIN_DISABLED", "operational endpoints are disabled"))
		return false
	}
	got := r.Header.Get("Authorization")
	got = strings.TrimPrefix(got, "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Admin-Token")
	}
	if !constantTimeEqual(got, s.admin) {
		writeError(w, apierr.Forbidden("ADMIN_DENIED", "administrative token required"))
		return false
	}
	return true
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// Shutdown is a no-op placeholder kept so callers have one lifecycle hook.
func (s *Server) Shutdown(context.Context) error { return nil }
