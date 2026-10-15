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

// captureLossesDuring is what the agent could not capture while the test
// command ran, every kind but the PID-map refusals (the PID-map check reports
// those): a TLS handshake a client aborted against keploy's CA, a STARTTLS
// upgrade keploy could not make, a parser stall or dropped bytes. Under the proven
// switch every test after such a loss already failed with it; this is the
// run's own verdict, which also covers a loss after the last test. An error
// is a count that could not be read, at the start or now.
func (m *mockService) captureLossesDuring(ctx context.Context, base lossBase) (models.CaptureLosses, error) {
	if !base.checked {
		return nil, nil
	}
	if base.err != nil {
		return nil, fmt.Errorf("before the test command ran: %w", base.err)
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
	return out, nil
}
