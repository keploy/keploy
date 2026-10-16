package replay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// PIDMapReader is an optional Instrumentation extension: the inserts into the
// agent's PID maps the kernel has refused since its hooks loaded, each of them
// a process the hooks could not track (models.PIDMapRefusals).
// *pkg/platform/http.AgentClient implements it.
//
// A process the kernel refuses a place in the agent's PID maps is not
// intercepted, so its calls are not mocked: they reach the real dependency,
// and a test whose response it shaped passes or fails against that, not
// against the recording. A test set during which the count grew is therefore
// FAILED, whatever its tests said.
type PIDMapReader interface {
	GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error)
}

// pidMapReadTimeout bounds each read of the agent's refusals while the proven
// switch is off, as before it. Under the switch a read has no deadline of any
// length (design rule 15): the agent's answer, the connection's close or the
// run's own context ends it, so a slow agent is never taken for a lost count.
const pidMapReadTimeout = 10 * time.Second

// readPIDMapRefusals reads the agent's count. With the switch off it outlives
// the run's context (a stopped run still reports what was refused) under
// pidMapReadTimeout; with it on, it waits on the run's context alone.
func readPIDMapRefusals(ctx context.Context, reader PIDMapReader) (models.PIDMapRefusals, error) {
	if !provenswitch.On() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), pidMapReadTimeout)
		defer cancel()
	}
	return reader.GetPIDMapRefusals(ctx)
}

// pidMapMark is the agent's refusal count that the next test set's own are
// counted from. known is false when the agent could not say; unreadable is
// then why, for an agent that counts but whose count could not be read (nil
// for one that does not count at all: ErrPIDMapRefusalsUnsupported).
type pidMapMark struct {
	at         models.PIDMapRefusals
	known      bool
	unreadable error
}

// pidMapVerdict is what a test set's refusals say about its capture.
// incomplete is the capture-incomplete reason when the agent refused a
// process during it; unverified is the capture-unverified reason when the
// agent counts but the count could not be read, so neither a loss nor its
// absence is shown. Both are "" when nothing was refused, and for an agent
// that cannot count.
type pidMapVerdict struct {
	incomplete, unverified string
}

// markPIDMaps reads the agent's refusals as the point the next test set's
// are counted from. It is called once the agent is up and before the
// application it serves starts: in Start for an agent that serves the whole
// run, and in RunTestSet for a compose agent that a test set starts afresh.
// Each test set's end then moves the mark to its own end
// (pidMapCaptureIncomplete), so that every refusal of the run falls in exactly
// one test set.
func (r *Replayer) markPIDMaps(ctx context.Context) {
	r.markCaptureLosses(ctx)
	r.pidMark = pidMapMark{}
	r.pidMarked = true
	reader, ok := r.instrumentation.(PIDMapReader)
	if !ok || !r.mocksDependencies() {
		return
	}
	at, err := readPIDMapRefusals(ctx, reader)
	if err != nil {
		r.pidMark = pidMapMark{unreadable: r.pidMapReadFailed(err, false)}
		return
	}
	r.pidMark = pidMapMark{at: at, known: true}
}

// mocksDependencies reports whether this run answers dependency calls from
// the recording. With --mocking=false, or a global passthrough, every call
// goes to the real dependency by design, so a process the agent could not
// track loses nothing, and its test sets are not checked (no mark is taken).
func (r *Replayer) mocksDependencies() bool {
	return r.config.Test.Mocking && !r.config.Record.GlobalPassthrough
}

// pidMapCaptureIncomplete reads what the agent refused during the test set
// that is ending and moves the mark to now. When the agent refused a process
// it logs that at ERROR and returns why the test set's capture is incomplete.
// When the agent counts but the count could not be read -- now, or at the
// mark -- it logs that at WARN and returns why the capture is unverified: not
// a failure of the test set (no flag of keploy test asks for that), but no
// basis for deleting or mapping anything either. The next read that succeeds
// is the next mark, so a lost read costs only the test sets on either side of
// it their verdict -- the one it ends and the one it starts -- not the rest of
// the run. Nothing is returned when nothing was refused, or for an agent that
// cannot count, which would otherwise fail or hold every test set.
func (r *Replayer) pidMapCaptureIncomplete(ctx context.Context, testSetID string) pidMapVerdict {
	reader, ok := r.instrumentation.(PIDMapReader)
	if !ok || (!r.pidMark.known && r.pidMark.unreadable == nil) {
		return pidMapVerdict{}
	}
	now, err := readPIDMapRefusals(ctx, reader)
	if err != nil {
		r.pidMark = pidMapMark{unreadable: r.pidMapReadFailed(err, true)}
		if r.pidMark.unreadable == nil { // the agent never counted at all
			return pidMapVerdict{}
		}
		if ctx.Err() != nil {
			// The user stopped the run under the proven switch (its read
			// waits on the run's context alone): unverified, as any lost
			// read, but not a WARN on top of the stop.
			r.logger.Debug("the run was stopped before this test set's count was read", zap.String("test-set", testSetID), zap.Error(err))
			return pidMapVerdict{unverified: models.CaptureUnverified(r.pidMark.unreadable)}
		}
		return r.pidMapUnverified(testSetID, r.pidMark.unreadable)
	}
	mark := r.pidMark
	r.pidMark = pidMapMark{at: now, known: true}
	if !mark.known {
		// Read now, but not at the mark: what this test set refused is
		// unknown. The next one counts from here.
		return r.pidMapUnverified(testSetID, fmt.Errorf("at the start of this test set: %w", mark.unreadable))
	}
	refused := now.Since(mark.at)
	if !refused.Any() {
		return pidMapVerdict{}
	}
	reason := refused.CaptureIncomplete()
	fields := []zap.Field{
		zap.String("test-set", testSetID),
		// Refused inserts, one for every socket() of a process a map had no
		// room for: not a number of processes.
		zap.Uint64("refused_target_inserts", refused.Target),
		zap.Uint64("refused_excluded_inserts", refused.Excluded),
	}
	if refused.LastTargetTGID != 0 {
		fields = append(fields, zap.Uint32("last_untracked_tgid", refused.LastTargetTGID))
	}
	msg := "test-set failed: the agent could not track every process the application ran, so their calls were not mocked"
	if refused.Target == 0 {
		msg = "test-set failed: the agent could not exclude every process that predates the application, so their children's calls may have been mocked as the application's"
	}
	r.logger.Error(msg, append(fields, zap.String("reason", reason))...)
	return pidMapVerdict{incomplete: reason}
}

// pidMapUnverified says, at WARN, that a test set's capture could not be
// verified, and returns that as its verdict.
func (r *Replayer) pidMapUnverified(testSetID string, err error) pidMapVerdict {
	reason := models.CaptureUnverified(err)
	r.logger.Warn("this test set is not verified: keploy could not read whether the agent tracked every process the application ran, so a call may have reached the real dependency unmocked; its mocks are not pruned and mappings.yaml is not written from it",
		zap.String("test-set", testSetID), zap.Error(err), zap.String("reason", reason))
	return pidMapVerdict{unverified: reason}
}

// pidMapReadFailed notes a read of the agent's count that failed, and returns
// the failure -- nil for an agent that cannot count at all (an older agent,
// hooks without PID maps), whose test sets are left as they were. Any other
// failure is a count that was lost, which the test set's verdict says.
func (r *Replayer) pidMapReadFailed(err error, established bool) error {
	if errors.Is(err, models.ErrPIDMapRefusalsUnsupported) {
		if !established {
			// Never counted: an agent that does not count at all (an older
			// image, non-Linux hooks). Its test sets are left as decided.
			r.logger.Debug("the agent cannot say whether it tracked every process of the application; capture completeness is not checked", zap.Error(err))
			return nil
		}
		// A mark WAS read earlier, so the agent counts; a now-"unsupported"
		// answer (a 404/501) is a lost count, not "never counts".
		r.logger.Debug("the agent counted before but no longer does; treating it as an unreadable count", zap.Error(err))
		return err
	}
	r.logger.Debug("failed to read the agent's refused PID map inserts", zap.Error(err))
	return err
}

// withCaptureIncomplete puts a test set's capture-incomplete reason ahead of
// the failure reason its status gives.
func withCaptureIncomplete(captureIncomplete, reason string) string {
	switch {
	case captureIncomplete == "":
		return reason
	case reason == "":
		return captureIncomplete
	}
	return captureIncomplete + "; " + reason
}

// CaptureLossReader is an optional Instrumentation extension: every kind of
// capture loss the agent has counted since it started (models.CaptureLosses).
type CaptureLossReader interface {
	GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error)
}

// captureLossMark is, under the proven switch, the agent's capture-loss counts
// a test set's tests are counted from. known is false when they could not be
// read; err says why (models.ErrCaptureLossesUnsupported for an agent that
// cannot count, which leaves the tests as they were).
type captureLossMark struct {
	at    models.CaptureLosses
	known bool
	err   error
}

// markCaptureLosses reads, under the proven switch, the counts the next test
// set's tests are counted from: where the PID maps are marked, and again at
// each test set's end, so a loss between two test sets is the next one's.
func (r *Replayer) markCaptureLosses(ctx context.Context) {
	r.lossMark = captureLossMark{}
	r.lossMarked = true
	// A run that does not mock sends every call to the real dependency by
	// design: nothing is lost, and nothing is checked (as for the PID maps).
	// Nor does a run with no agent to ask (a --base-path run): nothing of
	// its is captured.
	if !provenswitch.On() || !r.mocksDependencies() || r.noAgent {
		r.lossMark.err = models.ErrCaptureLossesUnsupported
		return
	}
	reader, ok := r.instrumentation.(CaptureLossReader)
	if !ok {
		r.lossMark.err = models.ErrCaptureLossesUnsupported
		return
	}
	at, err := readCaptureLosses(ctx, reader)
	if err != nil {
		r.lossMark.err = err
		return
	}
	r.lossMark = captureLossMark{at: at, known: true}
	if !r.lossRunBaseSet {
		// The run's first mark, before its application starts: what keploy
		// cannot capture at all is counted from here for the whole run.
		r.lossRunBase, r.lossRunBaseSet = at, true
	}
}

// readCaptureLosses reads the agent's counts with no deadline of its own (the
// proven switch's reads: the answer, the connection's close or ctx ends it).
func readCaptureLosses(ctx context.Context, reader CaptureLossReader) (models.CaptureLosses, error) {
	return reader.GetCaptureLosses(ctx)
}

// testCaptureLosses is, under the proven switch, what the agent could not
// capture that a test is charged, read as the test ends: processes the kernel
// refused a place in the agent's PID maps and TLS upgrades keploy could not
// make since the test set's mark -- such a loss taints the test running when
// it happened and every later test of the set (design v14 §3.2), and the
// counts only grow, so every later read carries it -- and client-side TLS
// handshakes that failed since the previous test's read (the set's mark for
// its first test): a handshake's loss is the one call it lost, the test's it
// happened in (models.CaptureLossKind.PerTest). What keploy cannot capture
// at all (decision 20) is counted from the run's base, however far back it
// was made (models.CaptureLosses.SinceWithin). A read that failed leaves the
// next test to count from the read before, so it is charged the failed
// test's handshakes too. An agent that cannot count leaves every test as it
// was, as the set's own check does; one that counts but could not be read
// now, or at the mark, is an error: the test's capture cannot be told.
func (r *Replayer) testCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	reader, ok := r.instrumentation.(CaptureLossReader)
	if !ok || errors.Is(r.lossMark.err, models.ErrCaptureLossesUnsupported) {
		return nil, nil
	}
	if !r.lossMark.known {
		if r.lossMark.err == nil {
			return nil, errors.New("keploy did not read the agent's capture-loss counts when the test set began")
		}
		return nil, fmt.Errorf("at the start of the test set: %w", r.lossMark.err)
	}
	now, err := readCaptureLosses(ctx, reader)
	if err != nil {
		return nil, err
	}
	from := r.lossMark.at
	if r.lossLastRead {
		from = r.lossLast
	}
	r.lossLast, r.lossLastRead = now, true
	return now.Charged(r.lossMark.at, r.lossRunBase, from), nil
}

// endCaptureLosses reads, at a test set's end under the proven switch, what
// the agent lost after the set's last per-test read (after its mark, when no
// test read), and moves the mark to now for the next test set. Nothing for
// an agent that cannot count or a run that does not mock. An error is a read
// that failed: what the set's tail lost cannot be told, and the next set's
// tests cannot be counted.
func (r *Replayer) endCaptureLosses(ctx context.Context) (models.CaptureLosses, error) {
	reader, ok := r.instrumentation.(CaptureLossReader)
	mark, last, lastRead := r.lossMark, r.lossLast, r.lossLastRead
	r.lossLast, r.lossLastRead = nil, false
	if !provenswitch.On() || !ok || !r.mocksDependencies() || r.noAgent || errors.Is(mark.err, models.ErrCaptureLossesUnsupported) {
		r.markCaptureLosses(ctx)
		return nil, nil
	}
	now, err := readCaptureLosses(ctx, reader)
	r.lossMarked = true
	if err != nil {
		r.lossMark = captureLossMark{err: err}
		return nil, err
	}
	r.lossMark = captureLossMark{at: now, known: true}
	if !mark.known {
		return nil, nil // the set's tests were unjudged for it already
	}
	from := mark.at
	if lastRead {
		from = last
	}
	// What keploy cannot capture at all over a connection that outlives
	// tests: only what is new since the set's last read (its tests already
	// carry the rest), counted from the run's base when no test read. A
	// standing unix_unseen is always new (no mark takes it away): such a set
	// is not judged. Every other kind, an abandoned TLS handshake too, from
	// the set's last read or its mark (SinceWithin).
	notFrom := r.lossRunBase
	if lastRead {
		notFrom = last
	}
	// Not the PID-map refusals: the set's PID-map check
	// (pidMapCaptureIncomplete) reports those, once.
	var out models.CaptureLosses
	for _, l := range now.SinceWithin(from, notFrom) {
		if l.Kind != models.CaptureLossPIDMap && l.Kind != models.CaptureLossPIDMapExcluded {
			out = append(out, l)
		}
	}
	return out, nil
}
