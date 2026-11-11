package models

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// A miss as evidence keeps it (Sanitized) keeps where its variables were
// bound (BoundIn): each is a name, never a value. It shares no slice with
// the miss.
func TestASanitizedMissKeepsWhereItsVariablesWereBound(t *testing.T) {
	m := UnmatchedCall{Protocol: "MongoDB", ActualSummary: "findAndModify entity_versions", MatchPhase: MatchPhaseVariable, ClosestMock: "mock-3212", BoundIn: []string{"mock-2065", "test case test-4"}}
	out := m.Sanitized()
	if !slices.Equal(out.BoundIn, []string{"mock-2065", "test case test-4"}) {
		t.Fatalf("sanitized BoundIn %v; want the miss's", out.BoundIn)
	}
	m.BoundIn[0] = "mock-1"
	if out.BoundIn[0] != "mock-2065" {
		t.Fatalf("the sanitized miss changed with the miss: %v", out.BoundIn)
	}
	if got := (UnmatchedCall{Protocol: "MongoDB"}).Sanitized().BoundIn; got != nil {
		t.Fatalf("a miss bound nowhere sanitizes to BoundIn %v; want none", got)
	}
}

// ReportAnswer's copy of a report shares no BoundIn with it.
func TestReportAnswerCopiesWhereTheVariablesWereBound(t *testing.T) {
	r := &MockMismatchReport{Protocol: "MongoDB", Answer: AnswerByKind, BoundIn: []string{"mock-2065"}}
	var got []error
	ctx := WithMockMismatchReporter(context.Background(), func(err error) { got = append(got, err) })
	if !ReportAnswer(ctx, r) || len(got) != 1 {
		t.Fatalf("the reporter got %v; want one answer", got)
	}
	var carrier interface{ MismatchReport() *MockMismatchReport }
	if !errors.As(got[0], &carrier) {
		t.Fatal("the answer carries no report")
	}
	r.BoundIn[0] = "mock-1"
	if sent := carrier.MismatchReport(); !slices.Equal(sent.BoundIn, []string{"mock-2065"}) {
		t.Fatalf("the reported answer's BoundIn %v changed with the parser's report", sent.BoundIn)
	}
}
