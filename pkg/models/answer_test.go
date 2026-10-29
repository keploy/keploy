package models

import (
	"context"
	"errors"
	"testing"
)

// An answered call counts against no test, whatever its protocol: only a
// miss is evidence.
func TestAnAnswerCountsAgainstNoTest(t *testing.T) {
	for _, how := range []string{AnswerProvisional, AnswerByKind} {
		c := UnmatchedCall{Protocol: "MongoDB", ActualSummary: "insert keploy.events", Answer: how}
		if c.CountsAgainstTest() {
			t.Errorf("an answer (%s) counts against its test", how)
		}
		r := &MockMismatchReport{Protocol: "MongoDB", Answer: how}
		if r.CountsAgainstTest() {
			t.Errorf("an answer's report (%s) counts against its test", how)
		}
	}
	if !(UnmatchedCall{Protocol: "MongoDB"}).CountsAgainstTest() {
		t.Error("a miss no longer counts against its test")
	}
}

// ReportAnswer hands the reporter a copy of the report, as ErrAnswered, and
// says whether a reporter took it; an empty Answer panics.
func TestReportAnswerTravelsAsErrAnswered(t *testing.T) {
	r := &MockMismatchReport{Protocol: "MongoDB", ActualSummary: "insert keploy.events", Answer: AnswerByKind,
		WriteKind: "Mongo insert keploy.events", Reused: true, Differs: []string{"body._id"},
		FieldDiffs: []MockFieldDiff{{Path: "body._id"}}, VariablePlaces: []string{"url.path[1]"},
		FieldReasons: []FieldReason{{Field: "body.since", Why: "body.since was learned as a read of the application's clock"}},
		Source:       &Origin{Comm: "sqs", Exe: "./sqs"}}
	if ReportAnswer(context.Background(), r) {
		t.Fatal("ReportAnswer says a reporter took the answer with none installed")
	}
	var got []error
	ctx := WithMockMismatchReporter(context.Background(), func(err error) { got = append(got, err) })
	if !ReportAnswer(ctx, r) {
		t.Fatal("ReportAnswer says no reporter took the answer")
	}
	if len(got) != 1 || !errors.Is(got[0], ErrAnswered) {
		t.Fatalf("the reporter got %v; want one ErrAnswered", got)
	}
	var carrier interface{ MismatchReport() *MockMismatchReport }
	if !errors.As(got[0], &carrier) {
		t.Fatal("the answer carries no report")
	}
	sent := carrier.MismatchReport()
	if sent == r || sent.Answer != AnswerByKind || sent.WriteKind != r.WriteKind || !sent.Reused {
		t.Fatalf("the reporter got %+v; want a copy of %+v", sent, r)
	}
	// The parser reuses its report.
	r.Differs[0], r.FieldDiffs[0].Path, r.VariablePlaces[0], r.Source.Comm = "body.other", "body.other", "url.path[2]", "other"
	r.FieldReasons[0].Field = "body.other"
	if sent.Differs[0] != "body._id" || sent.FieldDiffs[0].Path != "body._id" || sent.VariablePlaces[0] != "url.path[1]" || sent.Source.Comm != "sqs" || sent.FieldReasons[0].Field != "body.since" {
		t.Fatalf("the reported answer changed with the parser's report: %+v, %+v, %+v, %+v, %+v", sent.Differs, sent.FieldDiffs, sent.VariablePlaces, sent.Source, sent.FieldReasons)
	}
	if errors.Is(got[0], ErrNoMockMatched) {
		t.Fatal("an answer travels as a miss")
	}
	mustPanic(t, "without an Answer", func() { ReportAnswer(ctx, &MockMismatchReport{Protocol: "MongoDB"}) })
	mustPanic(t, "without an Answer", func() { ReportAnswer(ctx, nil) })
}

// A call made outside every test's window counts against no test: it is the
// application's background work (UnmatchedCall.Background).
func TestABackgroundMissCountsAgainstNoTest(t *testing.T) {
	m := UnmatchedCall{Protocol: "MongoDB", ActualSummary: "findAndModify ptw_endpoints"}
	if !m.CountsAgainstTest() {
		t.Fatal("a MongoDB miss counts against its test")
	}
	m.Background = true
	if m.CountsAgainstTest() {
		t.Fatal("a background miss counts against no test")
	}
}
