package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/archnemix/archx-eval/relay/internal/apierr"
)

// feedbackFor builds a valid report for a grant, so each test can then break
// exactly one thing and assert that this one thing is what gets refused.
func feedbackFor(g *Grant) FeedbackInput {
	ids := make([]string, 0, len(g.Examples))
	for _, ex := range g.Examples {
		ids = append(ids, ex.ID)
	}
	return FeedbackInput{
		RequestID:  g.RequestID,
		IdentityID: g.IdentityID,
		ModelID:    "gpt-x",
		ExampleIDs: ids,
	}
}

func codeOf(err error) string {
	if e, ok := err.(*apierr.Error); ok {
		return e.Code
	}
	return ""
}

func TestVerifiedFeedbackIsAccepted(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.30", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	res, err := h.svc.VerifyFeedback(feedbackFor(g), h.now)
	if err != nil {
		t.Fatalf("valid feedback refused: %v", err)
	}
	if res.RequestID != g.RequestID || res.Summary.Total != len(g.Examples) {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.IdentityID != g.IdentityID {
		t.Fatalf("identity not carried from the ledger: %q", res.IdentityID)
	}
}

// The core guarantee: a made-up request id buys nothing. Without this, anyone
// could POST a fabricated score into the dataset.
func TestFabricatedRequestIDRefused(t *testing.T) {
	h := newHarness(t, 50)
	in := FeedbackInput{RequestID: "invented-1234", ModelID: "gpt-x", ExampleIDs: []string{"py-easy-001"}}
	_, err := h.svc.VerifyFeedback(in, h.now)
	if codeOf(err) != "UNKNOWN_REQUEST" {
		t.Fatalf("expected UNKNOWN_REQUEST, got %v", err)
	}
}

// Results may not be reported under someone else's identity.
func TestMisattributedIdentityRefused(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.31", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	in := feedbackFor(g)
	in.IdentityID = "some-other-identity"
	if _, err := h.svc.VerifyFeedback(in, h.now); codeOf(err) != "IDENTITY_MISMATCH" {
		t.Fatalf("expected IDENTITY_MISMATCH, got %v", err)
	}
}

// Answers for questions that were never issued are refused. This is the check
// that makes "we verify it was our question" real rather than assumed.
func TestAnswerForUnissuedQuestionRefused(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.32", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	in := feedbackFor(g)
	in.ExampleIDs = append(in.ExampleIDs, "py-hard-999")
	if _, err := h.svc.VerifyFeedback(in, h.now); codeOf(err) != "QUESTION_NOT_ISSUED" {
		t.Fatalf("expected QUESTION_NOT_ISSUED, got %v", err)
	}
}

// Replaying a valid report must fail: one draw, one result.
func TestReplayRefused(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.33", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	in := feedbackFor(g)
	if _, err := h.svc.VerifyFeedback(in, h.now); err != nil {
		t.Fatalf("first report should be accepted: %v", err)
	}
	if _, err := h.svc.VerifyFeedback(in, h.now); codeOf(err) != "ALREADY_REPORTED" {
		t.Fatalf("expected ALREADY_REPORTED, got %v", err)
	}
}

// Dropping the questions a model did badly on would silently inflate its score,
// so an incomplete report is refused unless the runner admits it is partial.
func TestIncompleteReportRefusedUnlessPartial(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.34", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Examples) < 2 {
		t.Skip("need at least two examples")
	}
	in := feedbackFor(g)
	in.ExampleIDs = in.ExampleIDs[:1]

	if _, err := h.svc.VerifyFeedback(in, h.now); codeOf(err) != "BAD_REQUEST" {
		t.Fatalf("expected refusal for a silent partial run, got %v", err)
	}

	in.Partial = true
	res, err := h.svc.VerifyFeedback(in, h.now)
	if err != nil {
		t.Fatalf("declared partial run should be accepted: %v", err)
	}
	if !res.Partial || res.Issued != len(g.Examples) {
		t.Fatalf("partial run not recorded honestly: %+v", res)
	}
}

// Duplicate answers for one question would let a runner submit extra samples of
// the same easy question and skew the aggregate.
func TestDuplicateAnswersRefused(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.35", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	in := feedbackFor(g)
	in.ExampleIDs = append(in.ExampleIDs, in.ExampleIDs[0])
	if _, err := h.svc.VerifyFeedback(in, h.now); codeOf(err) != "BAD_REQUEST" {
		t.Fatalf("expected duplicate refusal, got %v", err)
	}
}

// Stale reports are refused: the ledger is pruned, so accepting a report for a
// request we can no longer check would reintroduce the hole.
func TestStaleReportRefused(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.36", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	later := h.now.Add(48 * time.Hour)
	if _, err := h.svc.VerifyFeedback(feedbackFor(g), later); codeOf(err) != "REPORT_TOO_LATE" {
		t.Fatalf("expected REPORT_TOO_LATE, got %v", err)
	}
}

// The ledger must survive a restart, or every in-flight run becomes
// unverifiable the first time the Space sleeps.
func TestLedgerSurvivesRestart(t *testing.T) {
	h := newHarness(t, 50)
	g, err := h.svc.RequestDraw(context.Background(), input("203.0.113.37", machA, diskA))
	if err != nil {
		t.Fatal(err)
	}
	h2 := h.reopen()
	if _, err := h2.svc.VerifyFeedback(feedbackFor(g), h2.now); err != nil {
		t.Fatalf("ledger did not survive restart: %v", err)
	}
}

// A draw always carries a request id, even when the caller sent none, because
// feedback depends on it.
func TestDrawAlwaysHasRequestID(t *testing.T) {
	h := newHarness(t, 20)
	in := input("203.0.113.38", machA, diskA)
	in.RequestID = ""
	g, err := h.svc.RequestDraw(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(g.RequestID) == "" {
		t.Fatal("draw returned an empty request id, so its feedback could never be verified")
	}
	if _, ok := h.store.LeaseLedger(g.RequestID); !ok {
		t.Fatal("no ledger entry for the minted request id")
	}
}
