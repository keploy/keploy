package replay

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/localfile"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// A call a test set's application makes outside every test (as it starts,
// before the set's first test, or after its last) that no mock answers is
// answered by keploy itself: a DNS lookup gets keploy's synthetic answer, a
// connection is refused or closed. A call outside every test is served, not
// required: it fails nothing that passed without it (with the proven switch
// on, a call that counts against a test still fails the set:
// before_set_calls.go, unclaimed_calls.go). It was named nowhere: the agent
// dropped those made before the set's first test with the switch off, and a
// DNS lookup's with it on, and with the switch off no one read those made
// after the set's last test (readAfterLastTest), so a start-up lookup made
// only in a replay (a configuration that
// differs from the recording's) could be neither seen nor corrected. The
// set's report names each now (TestReport.OutsideTestMisses), by its shape,
// the log says each with what to do, and the set's keploy test misses file
// (localfile.TestMissesFile, local and git-ignored) names each whole. It is
// keploy test's own: keploy mock replay's (localfile.MissesFile) is never
// written or removed by a keploy test run.

// readAfterLastTest is, with the proven switch off, the set's end read of the
// calls its application made after its last test that no mock answered
// (unclaimedCalls, which makes it under the switch, marked as the set's end
// read): with the switch off none of them fails the set, so they are read
// for its report alone, which names each (outsideTestMisses), as made after
// its last test. They were read by no one: cut at the next set's staging,
// said only by their shape in a line that keeps no record of them, or, after
// the run's last set, never read. An application keploy stops before the
// set's report is stopped first, after the set's post-script
// (stopForSetEndRead), so the read takes the calls it makes as it stops.
// One keploy stops only later (kept alive for a later set, or not started
// by keploy) is read running: a call it makes after the read is cut at the
// next set's staging, as before, or, after the run's last set, never read.
// The set's verdict is what it would be without the read, whatever it
// returns. A read that cannot vouch for every such call is said at WARN, but
// one the run's user cut short by stopping the run is the stop's: the stop
// is the set's status (USER_ABORT, cutReadStatus), so it is said at Debug,
// as under the switch. cut reports that stop.
func (r *Replayer) readAfterLastTest(ctx context.Context, testSetID string) (cut bool) {
	_, _, err := r.unclaimedCalls(ctx)
	switch {
	case err == nil:
	case stoppedByItsUser(ctx):
		r.logger.Debug("the read of the calls the application made after the test-set's last test was cut short by the run's stop; the stop is the test-set's status",
			zap.String("test-set", testSetID), zap.Error(err))
		return true
	default:
		r.logger.Warn("keploy could not read every call the application made after the test-set's last test that no mock answered: its report may not name each (outsideTestMisses); they fail nothing, so the test-set's verdict stands",
			zap.String("test-set", testSetID), zap.Error(err))
	}
	return false
}

// cutReadStatus is the status of a set whose set-end read (readAfterLastTest)
// its run's user cut short by stopping the run, from the status it had: a
// judged set (PASSED or FAILED) is USER_ABORT, as runEnded makes a set its
// user stopped before its status was final; the stop came while its
// application's last calls were read, and it is the set's. Any other status
// stands: the set was not judged, or an end of its own (its application's
// exit, keploy's error) came first, or it is IGNORED.
func cutReadStatus(status models.TestSetStatus) models.TestSetStatus {
	if status == models.TestSetStatusPassed || status == models.TestSetStatusFailed {
		return models.TestSetStatusUserAbort
	}
	return status
}

// stopForSetEndRead is, with the proven switch off, the stop of the set's
// application its end gives before it reads the calls the application made
// after its last test (readAfterLastTest) and writes its report: the stop of
// the application keploy started for the set, or of the app's container of
// its docker compose stack, the stack running on, when no later set or
// attempt is served by it (stopAppBeforeReport, which stops it as the set's
// teardown does), or, on the run's last set that runs (noLaterSetRuns: a
// later one wholly ignored, or with no test case, runs none), on an attempt
// no other follows, the stop of the one --keep-app-alive kept across the
// sets (keepAliveStop).
// nil when keploy stops the application only later: one kept alive, or kept
// in its compose stack, for a later set or attempt, or one it did not start;
// the read is then made before the set's post-script, with it running.
// keepAliveStop is set only for a run whose sets are each served by the
// application kept alive. Read only with the switch off, whose set's end
// stops the application before it judges the set (stopAppBeforeSetEnd).
func (r *Replayer) stopForSetEndRead(stopAppBeforeReport func()) func() {
	switch {
	case stopAppBeforeReport != nil:
		return stopAppBeforeReport
	case r.noLaterSetRuns && r.finalAttempt && r.keepAliveStop != nil:
		return func() { r.keepAliveStop() }
	}
	return nil
}

// stopBeforeSetEndRead stops the set's application with stop
// (stopForSetEndRead), the agent told first, as the set's teardown tells it,
// so it says the application's shutdown errors as such.
func (r *Replayer) stopBeforeSetEndRead(stop func()) {
	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := r.instrumentation.NotifyGracefulShutdown(notifyCtx); err != nil {
		r.logger.Debug("failed to notify agent of graceful shutdown", zap.Error(err))
	}
	notifyCancel()
	stop()
}

// outsideWriter is keploy test, as a set's misses file names it.
var outsideWriter = localfile.Writer{
	File:      localfile.TestMissesFile,
	Command:   "keploy test",
	Holds:     "each call the application made outside every test in the last run of this set that no recorded mock answered and that failed nothing",
	Unwritten: "could not write the calls the application made outside every test whole; the log names each by its shape",
}

// outsideTestMisses is the set's outside-test misses
// (TestReport.OutsideTestMisses), as its report names them, by their shape.
// It writes them whole, each with the same next step, in the set's misses
// file (localfile.WriteMisses: in place of the last run's, and only where it
// is git-ignored; a set with none removes the last run's), and says them in
// one log line (logOutsideTestMisses), with the file's path and their next
// steps.
func (r *Replayer) outsideTestMisses(testSetID string) []models.OutsideTestMiss {
	calls := r.setCalls.outside(provenswitch.On())
	whole := make([]models.UnmatchedCall, 0, len(calls))
	for _, c := range calls {
		c.NextSteps = models.OutsideTestNextStep(c)
		if c.Whole != nil {
			w := *c.Whole
			w.NextStep = c.NextSteps
			c.Whole = &w
		}
		whole = append(whole, c)
	}
	file := localfile.WriteMisses(r.logger, r.config.Path, testSetID, outsideWriter, whole)
	misses := models.OutsideTestMissesOf(calls)
	r.logOutsideTestMisses(testSetID, misses, file)
	return misses
}

// logOutsideTestMisses emits ONE Info summary line for the set's outside-test
// misses (with total call count, per-protocol breakdown, the misses file path
// when written, and each distinct next step its misses carry: next_steps, one
// for DNS lookups and one for other calls, each of which first says to record
// the test set again) and logs each per-call entry at Debug. The misses file
// keeps every miss whole and the report by its shape, each beside its own
// next step.
func (r *Replayer) logOutsideTestMisses(testSetID string, misses []models.OutsideTestMiss, file string) {
	if len(misses) == 0 {
		return
	}
	totalCalls, breakdown := formatOutsideTestSummary(misses)
	callWord := "calls"
	if totalCalls == 1 {
		callWord = "call"
	}
	fields := []zap.Field{
		zap.String("test-set", testSetID),
		zap.Int("calls", totalCalls),
		zap.Int("unique", len(misses)),
	}
	var msg string
	if file != "" {
		msg = fmt.Sprintf("%d %s outside every test got no recorded answer (fail nothing): %s; each is named in the misses file (%s)", totalCalls, callWord, breakdown, file)
		fields = append(fields,
			zap.String("misses_file", file),
			zap.String("next_step", fmt.Sprintf("inspect %s, which names each call whole beside its next step; next_steps says what to do for each kind of call", file)),
		)
	} else {
		msg = fmt.Sprintf("%d %s outside every test got no recorded answer (fail nothing): %s; each is named in the test-set report (outsideTestMisses)", totalCalls, callWord, breakdown)
		fields = append(fields,
			zap.String("next_step", "inspect outsideTestMisses in the test-set report, which names each call by its shape beside its next step; next_steps says what to do for each kind of call"),
		)
	}
	fields = append(fields, zap.Strings("next_steps", outsideNextSteps(misses)))
	r.logger.Info(msg, fields...)

	for _, m := range misses {
		r.logger.Debug("the application made a call outside every test that no mock answered; it fails nothing (a call outside every test is served, not required), and the test-set's report names it (outsideTestMisses)",
			zap.String("test-set", testSetID),
			zap.String("protocol", m.Protocol),
			zap.String("call", m.Call),
			utils.PathTagField(m.PathTag),
			zap.String("destination", m.Destination),
			zap.String("when", m.When),
			zap.Int("count", m.Count),
			zap.String("next_step", m.NextStep))
	}
}

// outsideNextSteps is each distinct next step of misses, in the order first
// carried: what the summary line says to do (models.OutsideTestNextStep gives
// a DNS lookup one and any other call another).
func outsideNextSteps(misses []models.OutsideTestMiss) []string {
	var steps []string
	for _, m := range misses {
		if m.NextStep != "" && !slices.Contains(steps, m.NextStep) {
			steps = append(steps, m.NextStep)
		}
	}
	return steps
}

func formatOutsideTestSummary(misses []models.OutsideTestMiss) (int, string) {
	type protoStats struct {
		calls int
		dests map[string]struct{}
	}
	byProto := map[string]*protoStats{}
	total := 0
	for _, m := range misses {
		n := m.Count
		if n <= 0 {
			n = 1
		}
		total += n
		proto := strings.TrimSpace(m.Protocol)
		if proto == "" {
			proto = "unknown"
		}
		st := byProto[proto]
		if st == nil {
			st = &protoStats{dests: map[string]struct{}{}}
			byProto[proto] = st
		}
		st.calls += n
		if d := strings.TrimSpace(m.Destination); d != "" {
			st.dests[d] = struct{}{}
		}
	}
	protos := make([]string, 0, len(byProto))
	for p := range byProto {
		protos = append(protos, p)
	}
	sort.Slice(protos, func(i, j int) bool {
		if byProto[protos[i]].calls != byProto[protos[j]].calls {
			return byProto[protos[i]].calls > byProto[protos[j]].calls
		}
		return protos[i] < protos[j]
	})
	parts := make([]string, 0, len(protos))
	for _, p := range protos {
		st := byProto[p]
		if !strings.EqualFold(p, "DNS") && len(st.dests) > 0 {
			hostWord := "hosts"
			if len(st.dests) == 1 {
				hostWord = "host"
			}
			parts = append(parts, fmt.Sprintf("%s x%d to %d %s", p, st.calls, len(st.dests), hostWord))
		} else {
			parts = append(parts, fmt.Sprintf("%s x%d", p, st.calls))
		}
	}
	return total, strings.Join(parts, ", ")
}
