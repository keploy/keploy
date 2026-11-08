package replay

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// readsMarked is the harness's agent, noting whether each read of the misses
// was marked as the set's end.
type readsMarked struct {
	*prInstr
	mu     sync.Mutex
	marked []bool
}

func (r *readsMarked) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	r.mu.Lock()
	r.marked = append(r.marked, models.SetEndRead(ctx))
	r.mu.Unlock()
	return r.prInstr.GetMockErrors(ctx)
}

// Under the proven switch the set's end read, after its last test, is marked
// as the set's end (the agent's parsers file what they hold for a test of
// the set first); each test's own read is not.
func TestRunTestSet_OnlyTheSetsEndReadIsMarked(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	instr := &readsMarked{prInstr: h.instr}
	h.replayer.instrumentation = instr

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s, want PASSED", status)
	}
	instr.mu.Lock()
	defer instr.mu.Unlock()
	if n := len(instr.marked); n != 3 {
		t.Fatalf("%d reads of the misses, want each test's and the set's end", n)
	}
	last := len(instr.marked) - 1
	if !instr.marked[last] {
		t.Fatalf("the set's end read (the last, of %d) is not marked: %v", len(instr.marked), instr.marked)
	}
	for i, m := range instr.marked[:last] {
		if m {
			t.Fatalf("read %d of %d, a test's, is marked as the set's end: %v", i+1, len(instr.marked), instr.marked)
		}
	}
}

// keploy test tells its agent, at each set's session and at a repair's
// re-registration of it, that it reads each set's end wherever it makes that
// read: under the proven switch (unclaimedCalls), and with it off where an
// agent serves the set's mocks (setEndOff). An agent's parsers hold a call
// for a later test of the set only then.
func TestTheSessionSaysItReadsSetEndsWhereItReadsThem(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		on, instrument, noAgent bool
		mocking, globalPassThru bool
		want                    bool
	}{
		{name: "switch on", on: true, want: true},
		{name: "switch on, mocks served", on: true, instrument: true, mocking: true, want: true},
		{name: "switch off, mocks served", instrument: true, mocking: true, want: true},
		{name: "switch off, no mocking", instrument: true},
		{name: "switch off, global passthrough", instrument: true, mocking: true, globalPassThru: true},
		{name: "switch off, no agent", instrument: true, mocking: true, noAgent: true},
		{name: "switch off, not instrumented", mocking: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, tc.on)
			cfg := &config.Config{}
			cfg.Test.Mocking = tc.mocking
			cfg.Record.GlobalPassthrough = tc.globalPassThru
			f := &latchInstr{}
			r := &Replayer{logger: zap.NewNop(), instrumentation: f, config: cfg, instrument: tc.instrument, noAgent: tc.noAgent}
			if err := r.mockOutgoingForTestSet(context.Background(), models.OutgoingOptions{}); err != nil {
				t.Fatal(err)
			}
			if f.opts.ReadsSetEnds != tc.want {
				t.Fatalf("the set's session said ReadsSetEnds=%v, want %v", f.opts.ReadsSetEnds, tc.want)
			}

			// A repair runs only for an instrumented run.
			rf := &rearmInstr{storeTakesEffect: true}
			rr := newRearmReplayer(rf)
			rr.config, rr.noAgent = cfg, tc.noAgent
			want := tc.want || (!tc.on && !tc.instrument && tc.mocking && !tc.noAgent && !tc.globalPassThru)
			filtered, unfiltered, consumed, cases := rearmSessionArgs()
			if err := rr.ensureAgentHoldsStoredMocks(context.Background(), "run-1", "test-set-0", models.OutgoingOptions{}, filtered, unfiltered, consumed, false, cases); err != nil {
				t.Fatal(err)
			}
			rf.mu.Lock()
			reads := rf.outgoingOpts.ReadsSetEnds
			rf.mu.Unlock()
			if reads != want {
				t.Fatalf("the repair's re-registration said ReadsSetEnds=%v, want %v", reads, want)
			}
		})
	}
}

// The set's verdict and its log tell a call made before the set's first test
// from one a parser held for a later test's recording that none answered
// (models.UnmatchedCall.Held): both are the set's (setsCall). A held call may
// have been made in a test, between two, or be one more than its test
// recorded: the verdict keys on Held, never on where it was made.
func TestSetCallsWhyTellsAHeldCallFromOneMadeBeforeTheFirstTest(t *testing.T) {
	before := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "insert on events", Destination: "mongo:27017", Kind: models.MissOutsideTest, BeforeSet: true}
	const beforeWhy = "before the set's first test that no mock answered"
	const heldWhy = "that keploy held for a later test's recording, which no test of the set answered"
	for _, kind := range []string{models.MissInTest, models.MissOutsideTest, models.MissExtraCall} {
		held := models.UnmatchedCall{Protocol: "Kafka", ActualSummary: "Fetch orders", Destination: "kafka:9092", Kind: kind, Held: true}
		if !setsCall(held) {
			t.Fatalf("kind %q: a held call is not the set's", kind)
		}
		got := setCallsWhy([]models.UnmatchedCall{before, held})
		for _, want := range []string{
			"1 call(s) " + beforeWhy + ": Mongo insert on events to mongo:27017",
			"1 call(s) " + heldWhy + ": Kafka Fetch orders to kafka:9092",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("kind %q: %q does not say %q", kind, got, want)
			}
		}
		if got := setCallsWhy([]models.UnmatchedCall{held}); strings.Contains(got, beforeWhy) || !strings.Contains(got, heldWhy) {
			t.Fatalf("kind %q: a held call alone is said %q", kind, got)
		}

		core, logs := observer.New(zapcore.ErrorLevel)
		(&Replayer{logger: zap.New(core)}).logSetCalls("test-set-0", []models.UnmatchedCall{before, held})
		entries := logs.All()
		if len(entries) != 2 {
			t.Fatalf("kind %q: %d lines logged, want one per call", kind, len(entries))
		}
		if m := entries[0].Message; !strings.Contains(m, "before its first test") || strings.Contains(m, "held") {
			t.Fatalf("kind %q: the call before the first test logged %q", kind, m)
		}
		if next := entries[0].ContextMap()["next_step"]; strings.Contains(fmt.Sprint(next), "held for a later test") {
			t.Fatalf("kind %q: the call before the first test's next step is %q", kind, next)
		}
		if m := entries[1].Message; !strings.Contains(m, "held a call of the application for a later test's recording") || strings.Contains(m, "before its first test") {
			t.Fatalf("kind %q: the held call logged %q", kind, m)
		}
		if next := entries[1].ContextMap()["next_step"]; !strings.Contains(fmt.Sprint(next), "held for a later test") {
			t.Fatalf("kind %q: the held call's next step is %q", kind, next)
		}
	}
	if got := setCallsWhy([]models.UnmatchedCall{before}); strings.Contains(got, heldWhy) || !strings.Contains(got, beforeWhy) {
		t.Fatalf("a call no parser held is said %q", got)
	}
	if setsCall(models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /own", Kind: models.MissInTest}) {
		t.Fatal("a test's own call, neither held nor made before the set, is the set's")
	}
}

// heldRead is a read that carries a consumer's poll a parser held for a later
// test's recording that none answered (models.UnmatchedCall.Held): its
// connection ended while a test's window was open (the client gave up on the
// poll), so the read of that test takes its miss.
func heldRead() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "Kafka", ActualSummary: "Fetch orders", Destination: "kafka:9092",
		Kind: models.MissInTest, Held: true}}, nil
}

// A held call whose miss a test's read takes (its connection ended in that
// test) was not that test's: the test passes, and the set fails with the
// call, named as held.
func TestRunTestSet_AHeldCallATestsReadTakesFailsTheSetNotTheTest(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 3, 0)
	h.instr.mockErrorsFor = misses(nil, heldRead)

	status := h.run(t)

	rep := h.report.report
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed {
			t.Fatalf("test %s is %s; the held call is no test's", r.TestCaseID, r.Status)
		}
		if len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("the held call was charged to %s: %+v", r.TestCaseID, r.FailureInfo.UnmatchedCalls)
		}
	}
	if status != models.TestSetStatusFailed || rep.Status != string(models.TestSetStatusFailed) {
		t.Fatalf("status %s (report %s); a held call no test answered must fail the set", status, rep.Status)
	}
	if len(rep.UnclaimedCalls) != 1 || !rep.UnclaimedCalls[0].Held || rep.UnclaimedCalls[0].ActualSummary != "Fetch orders" {
		t.Fatalf("unclaimed calls %+v; want the held poll", rep.UnclaimedCalls)
	}
	reason := rep.FailureReason
	if !strings.Contains(reason, "that keploy held for a later test's recording") || !strings.Contains(reason, "Kafka Fetch orders to kafka:9092") {
		t.Fatalf("failure reason %q does not name the held call", reason)
	}
	if strings.Contains(reason, "before the set's first test") || strings.Contains(reason, "after the set's last test") {
		t.Fatalf("failure reason %q says the held call was made outside the set's tests", reason)
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is marked not judged; it was judged, FAILED")
	}
}

// A held call the set's end read files is named as held, apart from a call
// made after the set's last test. It is no call made after the last test: a
// recording whose stop had to kill the app (which leaves a call made after
// the last test unjudged) still fails the set with it.
func TestRunTestSet_AHeldCallTheSetsEndFilesIsNamedHeld(t *testing.T) {
	provenOn(t, true)
	both := func() ([]models.UnmatchedCall, error) {
		held, _ := heldRead()
		after, _ := afterLast()
		return append(held, after...), nil
	}
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(nil, nil, both)
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 2 {
		t.Fatalf("unclaimed calls %+v; want the held poll and the publish", rep.UnclaimedCalls)
	}
	reason := rep.FailureReason
	for _, want := range []string{
		"1 call(s) that keploy held for a later test's recording, which no test of the set answered: Kafka Fetch orders to kafka:9092",
		"1 call(s) after the set's last test that no mock answered, which no later test could take: HTTP POST /publish to broker:9099",
	} {
		if !strings.Contains(reason, want) {
			t.Fatalf("failure reason %q does not say %q", reason, want)
		}
	}

	h = newPartialRunHarness(t, 2, 0)
	h.replayer.testSetConf = cutRecordingConf{why: recordingKill}
	h.instr.mockErrorsFor = misses(nil, nil, heldRead)
	if status := h.run(t); status != models.TestSetStatusFailed || h.replayer.notJudged("test-set-0") {
		t.Fatalf("a held call in a recording its stop cut: status %s, not judged %v; want FAILED, judged", status, h.replayer.notJudged("test-set-0"))
	}
	if reason := h.report.report.FailureReason; strings.Contains(reason, "the recording's stop had to kill the application") {
		t.Fatalf("failure reason %q blames the recording's stop for a held call", reason)
	}
}
