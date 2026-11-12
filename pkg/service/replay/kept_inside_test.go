package replay

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// keptLines returns the report's lines: the header's destination count and
// opt-in, and "<protocol> <destination> <calls>" for each destination, in the
// order they were logged.
func keptLines(t *testing.T, logs *observer.ObservedLogs) (header map[string]any, dests []string) {
	t.Helper()
	for _, e := range logs.All() {
		if !strings.HasPrefix(e.Message, KeptInsidePrefix) {
			continue
		}
		fields := e.ContextMap()
		if _, ok := fields["destinations"]; ok {
			if header != nil {
				t.Fatalf("the report's header was logged twice")
			}
			header = fields
			continue
		}
		dests = append(dests, fmt.Sprintf("%s %v", strings.TrimPrefix(e.Message, KeptInsidePrefix), fields["calls"]))
	}
	return header, dests
}

// Each destination the app called that no mock answered is named once for the
// run, with its count over every read, whatever test or set read it; an
// answered call and a connection that sent nothing are not in it.
func TestTheRunNamesEachDestinationKeptInsideOnce(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Replayer{logger: zap.New(core)}

	r.keptInside.add([]models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /", Destination: "sqs.us-east-1.amazonaws.com"},
		{Protocol: "DNS", ActualSummary: "A api.stripe.com."},
		{Protocol: "HTTP", ActualSummary: "GET /health", Destination: "payments:8080", Answer: models.AnswerProvisional},
		{Protocol: "Generic", Destination: "10.0.0.7:5672", MatchPhase: models.MatchPhaseNoRequest},
	})
	r.keptInside.add([]models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "POST /", Destination: "sqs.us-east-1.amazonaws.com"},
		{Protocol: "DNS", ActualSummary: "AAAA api.stripe.com."},
		{Protocol: "unknown", ActualSummary: "no mock matched"},
		{Protocol: "PostgresV2", ActualSummary: "SELECT 1", Destination: "db:5432"},
	})
	r.sayKeptInside()

	header, dests := keptLines(t, logs)
	if header == nil {
		t.Fatalf("no report header; logged: %v", logs.All())
	}
	if got := header["destinations"]; got != int64(4) {
		t.Fatalf("destinations = %v, want 4", got)
	}
	if optIn, _ := header["opt_in"].(string); !strings.Contains(optIn, "--allow-egress dns") || !strings.Contains(optIn, "real egress opened by") {
		t.Fatalf("the header does not say how real egress is opened: %q", optIn)
	}
	want := []string{
		"DNS api.stripe.com 2",
		"PostgresV2 db:5432 1",
		"HTTP sqs.us-east-1.amazonaws.com 2",
		"unknown (the unknown parser did not name the destination) 1",
	}
	if !slices.Equal(dests, want) {
		t.Fatalf("report lines\n got %q\nwant %q", dests, want)
	}
}

// A run in which every call was answered says nothing.
func TestARunWithNoMissSaysNothingKeptInside(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Replayer{logger: zap.New(core)}
	r.keptInside.add(nil)
	r.keptInside.add([]models.UnmatchedCall{{Protocol: "HTTP", Destination: "payments:8080", Answer: models.AnswerProvisional}})
	r.sayKeptInside()
	if header, dests := keptLines(t, logs); header != nil || dests != nil {
		t.Fatalf("a run without a miss reported %v %q", header, dests)
	}
}

// Every read of the agent's misses a test set makes feeds the run's report:
// the per-test reads, and (proven switch) the read after its last test.
func TestRunTestSetFeedsTheKeptInsideReport(t *testing.T) {
	perTest := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /", Destination: "sqs.us-east-1.amazonaws.com"}}, nil
	}
	for _, tc := range []struct {
		name  string
		on    bool
		reads func() ([]models.UnmatchedCall, error)
		want  string
		calls int
	}{
		{name: "per-test reads", reads: misses(perTest, perTest), want: "sqs.us-east-1.amazonaws.com", calls: 2},
		{name: "the read after the last test", on: true, reads: misses(nil, nil, afterLast), want: "broker:9099", calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.on)
			h := newPartialRunHarness(t, 2, 0)
			h.instr.mockErrorsFor = tc.reads

			h.run(t)

			got := h.replayer.keptInside.snapshot()
			if len(got) != 1 || got[0].destination != tc.want || got[0].calls != tc.calls {
				t.Fatalf("kept inside after the set = %+v, want %s with %d calls", got, tc.want, tc.calls)
			}
		})
	}
}
