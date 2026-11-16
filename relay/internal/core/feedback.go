package core

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
	"github.com/archnemix/archx-eval/relay/internal/identity"
	"github.com/archnemix/archx-eval/relay/internal/store"
)

// FeedbackInput is a benchmark report from a runner.
//
// It is verified rather than trusted. The runner is our code, so the contract is
// closed: an unknown shape is either a version mismatch or not our client, and
// both are refused instead of being coerced into something storable.
type FeedbackInput struct {
	RequestID string
	// IdentityID is what the client claims. It must match the ledger entry, so
	// a runner cannot report results under someone else's identity.
	IdentityID string
	ModelID    string
	RunnerVer  string
	OS         string
	Arch       string
	DurationS  float64
	// ExampleIDs is every question the runner is answering.
	ExampleIDs []string
	// Partial says the runner could not complete every issued question. Without
	// this flag a submission must cover everything that was issued; otherwise a
	// runner could silently drop the questions it did badly on.
	Partial bool

	RemoteAddr netip.Addr
	Headers    map[string][]string
	Device     identity.Device
}

// VerifyFeedback checks a report against the ledger and returns the result
// ready to store.
//
// The checks, cheapest first:
//
//  1. the request id is one we issued            -> invented ids refused
//  2. the identity matches the one it was issued to -> misattribution refused
//  3. it has not already been reported            -> replay refused
//  4. it is inside the feedback window            -> stale reports refused
//  5. every example was actually issued to them   -> fabricated answers refused
//  6. the submission is complete (or says it is not) -> cherry-picking refused
func (s *Service) VerifyFeedback(in FeedbackInput, now time.Time) (store.Result, error) {
	if strings.TrimSpace(in.RequestID) == "" {
		return store.Result{}, apierr.BadRequest("request_id is required: draw first, then report")
	}
	if strings.TrimSpace(in.ModelID) == "" {
		return store.Result{}, apierr.BadRequest("model_id is required")
	}
	if len(in.ExampleIDs) == 0 {
		return store.Result{}, apierr.BadRequest("items may not be empty")
	}

	entry, known := s.store.LeaseLedger(in.RequestID)
	if !known {
		return store.Result{}, apierr.Forbidden("UNKNOWN_REQUEST",
			"this request_id was not issued by this relay; call /v1/draw first")
	}

	// The identity must be the one the draw was issued to. This is what stops a
	// runner reporting results against somebody else's questions.
	if entry.IdentityID != "" && in.IdentityID != "" && entry.IdentityID != in.IdentityID {
		return store.Result{}, apierr.Forbidden("IDENTITY_MISMATCH",
			"this request_id was issued to a different identity")
	}

	if !entry.ReportedAt.IsZero() {
		return store.Result{}, apierr.Conflict("ALREADY_REPORTED",
			fmt.Sprintf("results for this request were already accepted at %s",
				entry.ReportedAt.UTC().Format(time.RFC3339)))
	}

	if win := s.cfg.FeedbackWindow; win > 0 && now.Sub(entry.IssuedAt) > win {
		return store.Result{}, apierr.Forbidden("REPORT_TOO_LATE",
			fmt.Sprintf("this request is older than the %s reporting window", win))
	}

	issued := make(map[string]bool, len(entry.ExampleIDs))
	for _, id := range entry.ExampleIDs {
		issued[id] = true
	}
	seen := make(map[string]bool, len(in.ExampleIDs))
	for _, id := range in.ExampleIDs {
		if seen[id] {
			return store.Result{}, apierr.BadRequest(fmt.Sprintf("duplicate answer for %q", id))
		}
		seen[id] = true
		if !issued[id] {
			return store.Result{}, apierr.Forbidden("QUESTION_NOT_ISSUED",
				fmt.Sprintf("%q was not issued to this identity under this request", id))
		}
	}
	if !in.Partial && len(seen) != len(issued) {
		missing := len(issued) - len(seen)
		return store.Result{}, apierr.BadRequest(fmt.Sprintf(
			"%d issued item(s) are missing from the report; set partial=true to submit an incomplete run",
			missing))
	}

	// Only now is the report accepted, and the ledger entry is closed so a
	// second submission for the same request id is a replay.
	if err := s.store.MarkLedgerReported(in.RequestID, now); err != nil {
		return store.Result{}, apierr.Forbidden("UNKNOWN_REQUEST", "this request_id is no longer valid")
	}

	return store.Result{
		TS:         now,
		RequestID:  in.RequestID,
		IdentityID: entry.IdentityID,
		ModelID:    in.ModelID,
		RunnerVer:  in.RunnerVer,
		OS:         in.OS,
		Arch:       in.Arch,
		Summary: store.ResultSummary{
			Total:     len(seen),
			ByCat:     map[string]float64{},
			DurationS: in.DurationS,
		},
		Partial: in.Partial,
		Issued:  len(issued),
	}, nil
}
