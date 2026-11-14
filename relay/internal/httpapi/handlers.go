package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
	"github.com/archnemix/archx-eval/relay/internal/bank"
	"github.com/archnemix/archx-eval/relay/internal/core"
	"github.com/archnemix/archx-eval/relay/internal/identity"
	"github.com/archnemix/archx-eval/relay/internal/store"
)

// drawRequest is the public request body. Device facts are grouped so the
// runner can send them verbatim from its probe.
type drawRequest struct {
	RequestID  string             `json:"request_id"`
	Categories []string           `json:"categories"`
	Mix        map[string]float64 `json:"mix,omitempty"`
	Size       int                `json:"size"`
	Device     devicePayload      `json:"device"`
}

type devicePayload struct {
	MachineID   string  `json:"machine_id"`
	DiskUUID    string  `json:"disk_uuid"`
	OSFamily    string  `json:"os_family"`
	Arch        string  `json:"arch"`
	RunnerVer   string  `json:"runner_version"`
	PythonVer   string  `json:"python_version"`
	DeviceClass string  `json:"device_class"`
	DMIProduct  string  `json:"dmi_product"`
	Hypervisor  bool    `json:"hypervisor"`
	ClockSkewS  float64 `json:"clock_skew_s"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, apierr.NotFound("no such endpoint"))
		return
	}
	writeJSON(w, 200, map[string]any{
		"service": "archx-eval relay",
		"version": core.Version,
		"endpoints": []string{
			"GET  /health",
			"GET  /v1/categories",
			"POST /v1/draw",
			"POST /v1/feedback",
		},
		"limits": map[string]any{
			"max_draw":    s.cfg.DrawMax,
			"window":      s.cfg.Window.String(),
			"stale_after": s.cfg.StaleAfterUses,
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h := s.svc.Health()
	status := 200
	if !h.OK {
		status = 503
	}
	writeJSON(w, status, h)
}

func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"categories": s.svc.Categories()})
}

func (s *Server) handleDraw(w http.ResponseWriter, r *http.Request) {
	// Closed loop: every draw must be attributable to a registered runner.
	if err := s.svc.Authenticate(bearerToken(r)); err != nil {
		writeError(w, err)
		return
	}
	var req drawRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	for _, c := range req.Categories {
		if hasControlChars(c) {
			writeError(w, apierr.BadRequest("category names may not contain control characters"))
			return
		}
	}
	reqID := r.Header.Get("X-Request-Id")
	if req.RequestID != "" {
		reqID = req.RequestID
	}

	remote := remoteAddrOf(r)
	in := core.DrawInput{
		RequestID:  reqID,
		Categories: req.Categories,
		Mix:        req.Mix,
		Size:       req.Size,
		Device: identity.Device{
			MachineID:   req.Device.MachineID,
			DiskUUID:    req.Device.DiskUUID,
			OSFamily:    strings.ToLower(strings.TrimSpace(req.Device.OSFamily)),
			Arch:        strings.ToLower(strings.TrimSpace(req.Device.Arch)),
			RunnerVer:   req.Device.RunnerVer,
			PythonVer:   req.Device.PythonVer,
			DeviceClass: req.Device.DeviceClass,
			DMIProduct:  req.Device.DMIProduct,
			Hypervisor:  req.Device.Hypervisor,
			ClockSkewS:  req.Device.ClockSkewS,
		},
		RemoteAddr: remote,
		Headers:    collectHeaders(r),
	}

	grant, err := s.svc.RequestDraw(r.Context(), in)
	if err != nil {
		s.logDeny(reqID, err)
		writeError(w, err)
		return
	}

	payload := make([]map[string]any, 0, len(grant.Examples))
	for _, ex := range grant.Examples {
		payload = append(payload, renderExample(ex))
	}
	writeJSON(w, 200, map[string]any{
		"request_id":   grant.RequestID,
		"identity_id":  grant.IdentityID,
		"issued_at":    grant.IssuedAt,
		"next_draw_at": grant.NextDrawAt,
		"count":        len(grant.Examples),
		"by_category":  grant.ByCat,
		"shortfall":    grant.Shortfall,
		"notes":        grant.Notes,
		"examples":     payload,
	})
}

// renderExample strips everything the caller must not see: reference solutions,
// test bodies, expected answers and provenance notes.
func renderExample(ex *bank.Example) map[string]any {
	out := map[string]any{
		"id":         ex.ID,
		"category":   ex.Category,
		"difficulty": ex.Difficulty,
		"kind":       ex.Kind,
		"prompt":     ex.Prompt,
	}
	if ex.Context != "" {
		out["context"] = ex.Context
	}
	if ex.Language != "" {
		out["language"] = ex.Language
	}
	if ex.Entrypoint != "" {
		out["entrypoint"] = ex.Entrypoint
	}
	if ex.Starter != "" {
		out["starter_code"] = ex.Starter
	}
	if len(ex.Tags) > 0 {
		out["tags"] = ex.Tags
	}
	return out
}

func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequestID  string         `json:"request_id"`
		IdentityID string         `json:"identity_id"`
		ModelID    string         `json:"model_id"`
		RunnerVer  string         `json:"runner_version"`
		OS         string         `json:"os"`
		Arch       string         `json:"arch"`
		DurationS  float64        `json:"duration_s"`
		Items      []feedbackItem `json:"items"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if strings.TrimSpace(body.ModelID) == "" {
		writeError(w, apierr.BadRequest("model_id is required"))
		return
	}
	if len(body.Items) == 0 {
		writeError(w, apierr.BadRequest("items may not be empty"))
		return
	}
	if len(body.Items) > 500 {
		writeError(w, apierr.BadRequest("at most 500 items may be reported at once"))
		return
	}
	if err := validateFeedbackText(body); err != nil {
		writeError(w, err)
		return
	}

	res := store.Result{
		TS:         time.Now().UTC(),
		RequestID:  body.RequestID,
		IdentityID: body.IdentityID,
		ModelID:    body.ModelID,
		RunnerVer:  body.RunnerVer,
		OS:         body.OS,
		Arch:       body.Arch,
		Summary: store.ResultSummary{
			Total:     len(body.Items),
			ByCat:     map[string]float64{},
			DurationS: body.DurationS,
		},
	}
	var scoreSum float64
	catSum := map[string]float64{}
	catN := map[string]int{}
	for _, it := range body.Items {
		if strings.TrimSpace(it.ExampleID) == "" {
			writeError(w, apierr.BadRequest("every item needs an example_id"))
			return
		}
		res.Items = append(res.Items, store.ResultItem{
			ExampleID:  it.ExampleID,
			Category:   it.Category,
			Difficulty: it.Difficulty,
			Answer:     it.Answer,
			Code:       it.Code,
			Passed:     it.Passed,
			Score:      clamp01(it.Score),
			LatencyMS:  it.LatencyMS,
			Error:      it.Error,
		})
		if it.Passed {
			res.Summary.Passed++
		}
		scoreSum += clamp01(it.Score)
		catSum[it.Category] += clamp01(it.Score)
		catN[it.Category]++
	}
	res.Summary.Score = scoreSum / float64(len(res.Items))
	for cat, sum := range catSum {
		if catN[cat] > 0 {
			res.Summary.ByCat[cat] = sum / float64(catN[cat])
		}
	}
	if err := s.svc.SubmitResult(res); err != nil {
		writeError(w, apierr.Internal("could not record results"))
		return
	}
	writeJSON(w, 200, map[string]any{
		"accepted": true,
		"summary":  res.Summary,
	})
}

type feedbackItem struct {
	ExampleID  string  `json:"example_id"`
	Category   string  `json:"category"`
	Difficulty string  `json:"difficulty"`
	Answer     string  `json:"answer"`
	Code       string  `json:"code"`
	Passed     bool    `json:"passed"`
	Score      float64 `json:"score"`
	LatencyMS  int     `json:"latency_ms"`
	Error      string  `json:"error"`
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func (s *Server) handleStale(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	stale := s.svc.StaleExamples()
	writeJSON(w, 200, map[string]any{
		"count":    len(stale),
		"examples": stale,
	})
}

func (s *Server) handleErase(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var body struct {
		IdentityID string `json:"identity_id"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.IdentityID == "" {
		writeError(w, apierr.BadRequest("identity_id is required"))
		return
	}
	removed, err := s.svc.Erase(body.IdentityID)
	if err != nil {
		writeError(w, apierr.Internal("erase failed"))
		return
	}
	writeJSON(w, 200, map[string]any{"erased": removed})
}

func (s *Server) logDeny(reqID string, err error) {
	e, ok := apierr.As(err)
	if !ok {
		return
	}
	if e.Status == 429 || e.Status == 403 {
		s.log.Warn("refused",
			"request_id", reqID,
			"code", e.Code,
			"retry_after_s", int(e.RetryAfter.Seconds()),
		)
	}
}

func remoteAddrOf(r *http.Request) netip.Addr {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if a, err := netip.ParseAddr(host); err == nil {
		return a
	}
	return netip.Addr{}
}

func collectHeaders(r *http.Request) map[string][]string {
	out := make(map[string][]string, len(r.Header)+2)
	for k, v := range r.Header {
		kk := canonicalHeader(k)
		out[kk] = append(out[kk], v...)
	}
	return out
}

func canonicalHeader(k string) string {
	parts := strings.Split(strings.ToLower(k), "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "-")
}

// bearerToken extracts the access token from the standard header, falling back
// to a custom header for clients that cannot set Authorization.
func bearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("X-Access-Token"))
}

// Field caps. These exist so the feedback endpoint cannot be used as free
// object storage: every accepted byte becomes our bandwidth, disk and bill.
const (
	maxAnswerBytes = 8 << 10
	maxCodeBytes   = 64 << 10
	maxErrorBytes  = 512
	maxModelIDLen  = 128
)

func validateFeedbackText(body struct {
	RequestID  string         `json:"request_id"`
	IdentityID string         `json:"identity_id"`
	ModelID    string         `json:"model_id"`
	RunnerVer  string         `json:"runner_version"`
	OS         string         `json:"os"`
	Arch       string         `json:"arch"`
	DurationS  float64        `json:"duration_s"`
	Items      []feedbackItem `json:"items"`
}) error {
	if len(body.ModelID) > maxModelIDLen {
		return apierr.BadRequest("model_id is too long")
	}
	if len(body.RunnerVer) > 64 || len(body.OS) > 32 || len(body.Arch) > 32 {
		return apierr.BadRequest("runner metadata is too long")
	}
	for _, it := range body.Items {
		if len(it.ExampleID) > 128 {
			return apierr.BadRequest("example_id is too long")
		}
		if len(it.Answer) > maxAnswerBytes {
			return apierr.BadRequest("answer is too long")
		}
		if len(it.Code) > maxCodeBytes {
			return apierr.BadRequest("code is too long")
		}
		if len(it.Error) > maxErrorBytes {
			return apierr.BadRequest("error is too long")
		}
		if len(it.Category) > 64 || len(it.Difficulty) > 32 {
			return apierr.BadRequest("category or difficulty is too long")
		}
		// Control characters would let a caller forge lines in the append-only
		// JSONL result log. Reject rather than sanitise: silent rewriting of a
		// submitted answer would corrupt the benchmark data.
		for _, field := range []string{it.ExampleID, it.Category, it.Difficulty} {
			if hasControlChars(field) {
				return apierr.BadRequest("identifiers may not contain control characters")
			}
		}
	}
	return nil
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}
