package httpapi

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
)

// handleMetrics emits a small Prometheus-compatible exposition. It is gated on
// METRICS_TOKEN because inventory numbers are commercially sensitive: knowing
// exactly how much usable stock remains is useful to someone farming the bank.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.authorisedMetric(r) {
		writeError(w, apierr.Forbidden("METRICS_DENIED", "metrics are not available"))
		return
	}
	h := s.svc.Health()

	var b strings.Builder
	line := func(name, kind, help string, value any) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %v\n", name, help, name, kind, name, value)
	}

	line("archx_relay_up", "gauge", "1 when the bank has usable stock.", boolInt(h.OK))
	line("archx_relay_uptime_seconds", "counter", "Seconds since process start.", h.UptimeSeconds)
	line("archx_bank_examples_total", "gauge", "Examples loaded in the bank.", h.Inventory.Total)
	line("archx_bank_examples_usable", "gauge", "Examples still inside the use limit.", h.Inventory.Usable)
	line("archx_bank_examples_stale", "gauge", "Examples that exhausted their uses.", h.Inventory.StaleCount)
	line("archx_identities_total", "gauge", "Known identities (hashed records).", h.Records)
	line("archx_markers_total", "gauge", "Distinct identity markers indexed.", h.Markers)
	line("archx_reputation_ranges", "gauge", "CIDR ranges loaded from the reputation table.", h.Reputation)

	// Per-category stock, sorted so the output is stable for diffing.
	cats := make([]string, 0, len(h.Inventory.ByCat))
	for c := range h.Inventory.ByCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	fmt.Fprintf(&b, "# HELP archx_bank_category_total Examples per category.\n# TYPE archx_bank_category_total gauge\n")
	for _, c := range cats {
		fmt.Fprintf(&b, "archx_bank_category_total{category=%q} %d\n", c, h.Inventory.ByCat[c])
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	line("archx_process_memory_bytes", "gauge", "Heap in use.", ms.HeapInuse)
	line("archx_goroutines", "gauge", "Live goroutines.", runtime.NumGoroutine())
	line("archx_process_start_time_seconds", "gauge", "Process start, unix seconds.",
		time.Now().Unix()-h.UptimeSeconds)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) authorisedMetric(r *http.Request) bool {
	token := s.metricsToken()
	if token == "" {
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	return constantTimeEqual(got, token)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
