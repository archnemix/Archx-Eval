package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// Result is one benchmark run reported back by a front-end runner. We keep it
// because the dataset promise is two-way: the user gets a verified bank, and
// we get the measurements their run produced.
type Result struct {
	TS         time.Time     `json:"ts"`
	RequestID  string        `json:"request_id"`
	IdentityID string        `json:"identity_id,omitempty"`
	ModelID    string        `json:"model_id"`
	RunnerVer  string        `json:"runner_version,omitempty"`
	OS         string        `json:"os,omitempty"`
	Arch       string        `json:"arch,omitempty"`
	Items      []ResultItem  `json:"items"`
	Summary    ResultSummary `json:"summary"`
}

// ResultItem is the outcome for a single example.
type ResultItem struct {
	ExampleID  string  `json:"example_id"`
	Category   string  `json:"category"`
	Difficulty string  `json:"difficulty"`
	Answer     string  `json:"answer,omitempty"`
	Code       string  `json:"code,omitempty"`
	Passed     bool    `json:"passed"`
	Score      float64 `json:"score"`
	LatencyMS  int     `json:"latency_ms,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// ResultSummary is the aggregate the dashboard reads.
type ResultSummary struct {
	Total     int                `json:"total"`
	Passed    int                `json:"passed"`
	Score     float64            `json:"score"`
	ByCat     map[string]float64 `json:"by_category,omitempty"`
	DurationS float64            `json:"duration_s,omitempty"`
}

// ErrResultTooLarge is returned when a report exceeds the size cap.
var ErrResultTooLarge = errors.New("store: result payload too large")

// AppendResult appends a benchmark report to results/<date>.jsonl.
func (s *Store) AppendResult(r Result) error {
	if len(r.Items) > 5000 {
		return ErrResultTooLarge
	}
	if r.TS.IsZero() {
		r.TS = time.Now().UTC()
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, "results", r.TS.UTC().Format("2006-01-02")+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}
