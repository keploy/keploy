package mock

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// CaptureLossReader is an optional Instrumentation extension: every kind of
// capture loss the agent has counted since it started (models.CaptureLosses).
type CaptureLossReader interface {
	GetCaptureLosses(ctx context.Context) (models.CaptureLosses, error)
}

// lossBase is, under the proven switch, the agent's capture-loss counts when
// the test command started. checked is false with the switch off, against an
// agent that cannot count, or under compose (whose agent stops with the
// project and leaves no account of these); err is a read that failed.
type lossBase struct {
	at      models.CaptureLosses
	checked bool
	err     error
}

// captureLossBaseline reads the counts the run's losses are counted from.
func (m *mockService) captureLossBaseline(ctx context.Context) lossBase {
	reader, ok := m.instrumentation.(CaptureLossReader)
	if !provenswitch.On() || !ok || m.isDockerCompose() {
		return lossBase{}
	}
	at, err := reader.GetCaptureLosses(ctx)
	if errors.Is(err, models.ErrCaptureLossesUnsupported) {
		m.logger.Debug("the agent cannot count what it could not capture; capture losses are not checked", zap.Error(err))
		return lossBase{}
	}
	return lossBase{at: at, checked: true, err: err}
}

// UnchargedLossReader is an optional Instrumentation extension: what the
// agent could not capture since the run began that no test was charged with
// (a loss after the last test ended).
type UnchargedLossReader interface {
	GetUnchargedCaptureLosses(ctx context.Context) (models.CaptureLosses, error)
}

// runLosses is, for a record that is judged, what the agent could not
// capture that no test was charged with: no recording holds what it carried,
// so judging fails the run on it (design v14 §7.5). Read once the run's last
// test has ended, before the agent may stop. What keploy cannot capture at
// all is left out (NotJudged's). An agent that cannot say which losses no
// test was charged with leaves every loss of the run as the run's (lost, and
// the PID-map refusals refused): judging then fails the run rather than pass
// on a guess. A read that failed otherwise (the agent's drain before it, or
// the read itself) leaves what was lost after the last test unknown, never
// "none": every loss of the run is the run's, and that is the error too, so
// judging fails the run however few were counted. When the run's own count
// could not be read either (lostErr), that is the error, as it is the same
// unknown.
func (m *mockService) runLosses(ctx context.Context, base lossBase, lost models.CaptureLosses, lostErr error, refused models.PIDMapRefusals) (models.CaptureLosses, error) {
	if !base.checked {
		return nil, nil
	}
	var unchargedErr error
	if reader, ok := m.instrumentation.(UnchargedLossReader); ok {
		un, err := reader.GetUnchargedCaptureLosses(ctx)
		if err == nil {
			judged, _ := un.Split()
			return judged, nil
		}
		unchargedErr = err
		m.logger.Warn("keploy could not read which of the run's capture losses no test was charged with, so every one of them is the run's", zap.Error(err))
	}
	all := append(append(models.CaptureLosses(nil), lost...), pidMapLosses(refused)...)
	switch {
	case lostErr != nil && unchargedErr != nil:
		return all, fmt.Errorf("%w; nor which of them no test was charged with: %v", lostErr, unchargedErr)
	case lostErr != nil:
		return all, lostErr
	case unchargedErr != nil && !errors.Is(unchargedErr, models.ErrCaptureLossesUnsupported):
		return all, fmt.Errorf("keploy could not read which of the run's capture losses no test was charged with: %w", unchargedErr)
	}
	return all, nil
}

// pidMapLosses are refused as capture losses, one for each PID map that
// refused an insert: the inserts it refused, the run's sentence for both
// maps (PIDMapRefusals.CaptureIncomplete), and the kernel's errno for that
// map's latest refusal, so a reader takes each map's cause from its own
// loss. nil when neither refused one.
func pidMapLosses(refused models.PIDMapRefusals) models.CaptureLosses {
	var out models.CaptureLosses
	if refused.Target > 0 {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMap, Count: refused.Target, What: refused.CaptureIncomplete(), Err: refused.LastTargetErr})
	}
	if refused.Excluded > 0 {
		out = append(out, models.CaptureLoss{Kind: models.CaptureLossPIDMapExcluded, Count: refused.Excluded, What: refused.CaptureIncomplete(), Err: refused.LastExcludedErr})
	}
	return out
}

// captureLossesDuring is what the agent could not capture while the test
// command ran, every kind but the PID-map refusals (the PID-map check reports
// those): a client-side TLS handshake with keploy's proxy that failed other
// than by the client hanging up, a STARTTLS upgrade keploy could not make, a
// parser stall or dropped bytes. Under the proven switch the test running at
// such a loss already failed with it, and every later one too but for a
// failed handshake, which is only its own test's; this is the run's own
// verdict, which also covers a loss outside every test. An error is a count
// that could not be read, at the start or now.
//
// What the kernel dropped at keploy's proxy listener after the last test is
// counted only when the agent closes the drops' window no test holds, which
// its read of the losses no test was charged with does (the agent's
// closeDropsGap). That read is made first, so this one has those drops,
// where every other loss after the last test is: a record's warning and its
// per-test file's CannotCapture say them, as they do a TLS handshake
// abandoned at the same moment. A test still open (its process went away;
// the record's read of the windows ends it) keeps its drops to its end.
// That read also has the agent drain first: its proxy counts the close of
// every connection the run's stop ended, or owns one it cannot count by name
// (not judged), so this read has them. If that read fails, this one still
// reads the counts, and returns what they lost with the read's error: whether
// a close the stop made lost a call cannot be told, so the run is never
// judged clean on what was counted. An agent older than it (one that cannot
// say) has no such drops, and this one goes on without them.
func (m *mockService) captureLossesDuring(ctx context.Context, base lossBase) (models.CaptureLosses, error) {
	if !base.checked {
		return nil, nil
	}
	if base.err != nil {
		return nil, fmt.Errorf("before the test command ran: %w", base.err)
	}
	var unchargedErr error
	if un, ok := m.instrumentation.(UnchargedLossReader); ok {
		if _, err := un.GetUnchargedCaptureLosses(ctx); err != nil && !errors.Is(err, models.ErrCaptureLossesUnsupported) {
			unchargedErr = fmt.Errorf("after the test command ran, keploy could not read whether its agent had counted every close the run's stop made: %w", err)
		}
	}
	reader := m.instrumentation.(CaptureLossReader)
	now, err := reader.GetCaptureLosses(ctx)
	if err != nil {
		return nil, fmt.Errorf("after the test command ran: %w", err)
	}
	var out models.CaptureLosses
	for _, l := range now.Since(base.at) {
		if l.Kind == models.CaptureLossPIDMap || l.Kind == models.CaptureLossPIDMapExcluded {
			continue
		}
		out = append(out, l)
	}
	return out, unchargedErr
}
