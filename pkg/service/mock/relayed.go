package mock

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// RelayedLiveReader is an optional Instrumentation extension: the connections
// the agent relayed live to a dependency since it started, instead of
// answering them from a recording (models.RelayedLives): a Redis the
// open-source keploy does not mock, the agent's pass-through, mock.real's,
// and the DNS lookups its DNS answered with the live resolver's answer.
// *pkg/platform/http.AgentClient implements it.
//
// A replay whose agent relayed one did not answer every dependency from the
// recording, though nothing missed and the suite passed: `keploy mock replay`
// claims isolation only when the agent says it relayed none. An agent that
// cannot say (an older one) leaves the claim unknown, never made.
type RelayedLiveReader interface {
	GetRelayedLive(ctx context.Context) (models.RelayedLives, error)
}

// relayedBase is what the agent had relayed before the test command ran;
// known is false when it could not say, and unread is then why.
type relayedBase struct {
	at     models.RelayedLives
	known  bool
	unread error
}

// relayedCheck is what the agent relayed live during the run: rows when
// known; otherwise unread says why it is unknown.
type relayedCheck struct {
	rows   models.RelayedLives
	known  bool
	unread error
}

// readRelayedLive reads the agent's count, as an epilogue read
// (epilogueRead).
func readRelayedLive(ctx context.Context, logger *zap.Logger, reader RelayedLiveReader) (models.RelayedLives, error) {
	var rows models.RelayedLives
	err := epilogueRead(ctx, logger,
		"waiting for keploy's agent to say which calls it relayed to a live server",
		"press Ctrl+C again to stop now, and the run is not verified",
		func(ctx context.Context) error {
			var err error
			rows, err = reader.GetRelayedLive(ctx)
			return err
		})
	return rows, err
}

// relayedBaseline reads what the agent relayed before the test command runs.
// Under compose the agent leaves its whole life's count as it stops, which is
// the run's (it came up with the project), so nothing is read here.
func (m *mockService) relayedBaseline(ctx context.Context) relayedBase {
	if m.isDockerCompose() {
		return relayedBase{known: true}
	}
	reader, ok := m.instrumentation.(RelayedLiveReader)
	if !ok {
		return relayedBase{unread: models.ErrRelayedLiveUnsupported}
	}
	at, err := readRelayedLive(ctx, m.logger, reader)
	if err != nil {
		m.logger.Debug("keploy could not read which calls its agent relayed to a live server before the test command ran", zap.Error(err))
		return relayedBase{unread: err}
	}
	return relayedBase{at: at, known: true}
}

// relayedDuring is what the agent relayed live from base on, read once the
// test command has exited: natively by asking the agent, under compose from
// the account it left as it stopped.
func (m *mockService) relayedDuring(ctx context.Context, base relayedBase) relayedCheck {
	if !base.known {
		return relayedCheck{unread: base.unread}
	}
	if m.isDockerCompose() {
		reader, ok := m.instrumentation.(ComposeOutcomeReader)
		if !ok {
			return relayedCheck{unread: models.ErrRelayedLiveUnsupported}
		}
		outcome, err := reader.ComposeAgentOutcome()
		switch {
		case err != nil:
			return relayedCheck{unread: err}
		case outcome.Relayed == nil:
			// An agent older than the count, which leaves no such field.
			return relayedCheck{unread: models.ErrRelayedLiveUnsupported}
		}
		return relayedCheck{rows: outcome.Relayed.Sorted(), known: true}
	}
	reader, ok := m.instrumentation.(RelayedLiveReader)
	if !ok {
		return relayedCheck{unread: models.ErrRelayedLiveUnsupported}
	}
	now, err := readRelayedLive(ctx, m.logger, reader)
	if err != nil {
		m.logger.Debug("keploy could not read which calls its agent relayed to a live server", zap.Error(err))
		return relayedCheck{unread: err}
	}
	return relayedCheck{rows: now.Since(base.at), known: true}
}

// note is why c leaves isolation unproven, "" when it does not: what was
// relayed and why, or that it is unknown.
func (c relayedCheck) note() string {
	switch {
	case !c.known && errors.Is(c.unread, models.ErrRelayedLiveUnsupported):
		return "unknown: keploy's agent does not say which calls it relayed to a live server (an agent older than this keploy, or one that does not count them), so nothing shows every dependency was answered from the recording"
	case !c.known:
		return fmt.Sprintf("unknown: keploy could not read which calls its agent relayed to a live server (%v), so nothing shows every dependency was answered from the recording", c.unread)
	case len(c.rows) > 0:
		return c.rows.Text()
	}
	return ""
}

// receiptRows is what the receipt lists (Receipt.Relayed): the rows when
// known, nothing otherwise.
func (c relayedCheck) receiptRows() models.RelayedLives {
	if !c.known || len(c.rows) == 0 {
		return nil
	}
	return c.rows
}

// nextStep is what to do for the calls c says were relayed live to be
// answered from a recording (models.RelayedLives.NextStep), or, when an agent
// that does not say what it relays leaves it unknown, for it to say; "" when
// none were, or a read failed.
func (c relayedCheck) nextStep() string {
	switch {
	case !c.known && errors.Is(c.unread, models.ErrRelayedLiveUnsupported):
		return "run keploy's agent at this keploy's version, which says which calls it relays to a live server"
	case !c.known:
		return ""
	}
	return c.rows.NextStep()
}
