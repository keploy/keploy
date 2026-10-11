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
	r.pidMark = pidMapMark{}
	reader, ok := r.instrumentation.(PIDMapReader)
	if !ok {
		return
	}
	at, err := readPIDMapRefusals(ctx, reader)
	if err != nil {
		r.pidMark = pidMapMark{unreadable: r.pidMapReadFailed(err, false)}
		return
	}
	r.pidMark = pidMapMark{at: at, known: true}
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
	r.logger.Error("test-set failed: the agent could not track every process the application ran, so their calls were not mocked",
		append(fields, zap.String("reason", reason))...)
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
