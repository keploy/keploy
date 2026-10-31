package mock

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// readPIDMapRefusals reads the agent's count, as an epilogue read
// (epilogueRead): with the proven switch off it is bounded by
// agentEpilogueTimeout, like every other end-of-run agent read; with it on,
// it has no deadline of any length (design rule 15): the agent's answer, the
// connection's close or ctx ends it, or the user, and the count is then
// unread.
func readPIDMapRefusals(ctx context.Context, logger *zap.Logger, reader PIDMapReader) (models.PIDMapRefusals, error) {
	var refusals models.PIDMapRefusals
	err := epilogueRead(ctx, logger,
		"waiting for keploy's agent to say whether it tracked every process of the test command",
		"press Ctrl+C again to stop now, and the run is not verified",
		func(ctx context.Context) error {
			var err error
			refusals, err = reader.GetPIDMapRefusals(ctx)
			return err
		})
	return refusals, err
}

// PIDMapReader is an optional Instrumentation extension: the inserts into the
// agent's PID maps the kernel has refused since its hooks loaded, each of them
// a process the hooks could not track (models.PIDMapRefusals).
// *pkg/platform/http.AgentClient implements it.
//
// A process the kernel refuses a place in the agent's PID maps is not
// intercepted: in record its calls are not recorded, in replay they are not
// mocked, and they reach the real dependency, which may well answer. The run
// looks complete and passes. So Record and Replay read the count before the
// test command runs and again after it, and a session in which it grew is
// capture incomplete: Record keeps nothing and Replay fails.
type PIDMapReader interface {
	GetPIDMapRefusals(ctx context.Context) (models.PIDMapRefusals, error)
}

// pidMapBase is what the agent had refused before the test command ran.
// known is false when the agent could not say; unreadable is then why, for an
// agent that counts but whose count could not be read (nil when it does not
// count at all: ErrPIDMapRefusalsUnsupported).
type pidMapBase struct {
	at         models.PIDMapRefusals
	known      bool
	unreadable error
}

// pidMapCheck is what a session's refusals are, read once the test command
// has exited: refused when known; otherwise unreadable says why the count was
// lost (completeness unproven), and both unset mean an agent that does not
// count (completeness not checkable, as before this check existed).
type pidMapCheck struct {
	refused    models.PIDMapRefusals
	known      bool
	unreadable error
}

// pidMapBaseline reads the agent's refusals before the test command runs.
// Under compose the agent is reachable once startComposeApp has returned,
// while the app is still parked behind keploy-app-gate; a compose recording
// is not checked at all (pidMapRefusedDuring), so it is not read.
func (m *mockService) pidMapBaseline(ctx context.Context, phase string) pidMapBase {
	reader, ok := m.instrumentation.(PIDMapReader)
	if !ok || (m.isDockerCompose() && phase != "replay") {
		return pidMapBase{}
	}
	at, err := readPIDMapRefusals(ctx, m.logger, reader)
	if err != nil {
		return pidMapBase{unreadable: m.pidMapReadFailed("before the test command ran", err, false)}
	}
	return pidMapBase{at: at, known: true}
}

// pidMapRefusedDuring is what the agent refused from base on, read once the
// test command has exited. Natively the agent is still up and is asked. Under
// compose it has stopped with the project: a replay reads the account it left
// on stopping, and a recording, for which it leaves none, cannot be checked.
func (m *mockService) pidMapRefusedDuring(ctx context.Context, base pidMapBase, phase string) pidMapCheck {
	if !base.known {
		return pidMapCheck{unreadable: base.unreadable}
	}
	var now models.PIDMapRefusals
	if m.isDockerCompose() {
		reader, ok := m.instrumentation.(ComposeOutcomeReader)
		if !ok || phase != "replay" {
			m.logger.Debug("under docker compose the agent stops with the project and leaves no account of a recording; capture completeness is not checked")
			return pidMapCheck{}
		}
		outcome, err := reader.ComposeAgentOutcome()
		switch {
		case err != nil:
			return pidMapCheck{unreadable: m.pidMapReadFailed("after the test command ran", err, true)}
		case outcome.PIDMapRefusals == nil && outcome.PIDMapRefusalsError != "":
			// Hooks that count, whose count the stopping agent could not read.
			return pidMapCheck{unreadable: m.pidMapReadFailed("after the test command ran", errors.New("the agent could not read its count as it stopped: "+outcome.PIDMapRefusalsError), true)}
		case outcome.PIDMapRefusals == nil:
			return pidMapCheck{unreadable: m.pidMapReadFailed("after the test command ran", models.ErrPIDMapRefusalsUnsupported, true)}
		}
		now = *outcome.PIDMapRefusals
	} else {
		reader := m.instrumentation.(PIDMapReader) // base.known: it is one
		var err error
		if now, err = readPIDMapRefusals(ctx, m.logger, reader); err != nil {
			return pidMapCheck{unreadable: m.pidMapReadFailed("after the test command ran", err, true)}
		}
	}
	return pidMapCheck{refused: now.Since(base.at), known: true}
}

// pidMapReadFailed notes a read of the agent's count that failed, and returns
// the failure as the session's "unreadable" -- nil for an agent that cannot
// count at all (an older agent, hooks without PID maps), whose sessions are
// left as they were: failing them would fail every run against it, and there
// is nothing of its to lose. Any other failure is a count that was lost, which
// the session's verdict says (pidMapUnverified), so it is only noted here.
func (m *mockService) pidMapReadFailed(when string, err error, established bool) error {
	if errors.Is(err, models.ErrPIDMapRefusalsUnsupported) {
		if !established {
			// No count was ever read: an agent that does not count at all (an
			// older image, hooks without PID maps). Its sessions pass, as
			// before this check existed -- failing them would fail every run.
			m.logger.Debug("the agent cannot say whether it tracked every process of the test command; capture completeness is not checked", zap.String("when", when), zap.Error(err))
			return nil
		}
		// A count WAS read earlier, so the agent counts; a now-"unsupported"
		// answer (a 404/501) is a count that was lost -- the agent restarted,
		// perhaps to an older image -- not "never counts". It is unreadable.
		m.logger.Debug("the agent counted before but no longer does; treating it as an unreadable count", zap.String("when", when), zap.Error(err))
		return fmt.Errorf("%s: %w", when, err)
	}
	m.logger.Debug("failed to read the agent's refused PID map inserts", zap.String("when", when), zap.Error(err))
	return fmt.Errorf("%s: %w", when, err)
}

// pidMapFields describes a session's refusals for its ERROR line. The counts
// are refused inserts, one for every socket() of a process a map had no room
// for, and are named so: they are not a number of processes.
func pidMapFields(refused models.PIDMapRefusals) []zap.Field {
	fields := []zap.Field{
		zap.Uint64("refused_target_inserts", refused.Target),
		zap.Uint64("refused_excluded_inserts", refused.Excluded),
	}
	if refused.LastTargetTGID != 0 {
		fields = append(fields, zap.Uint32("last_untracked_tgid", refused.LastTargetTGID))
	}
	return fields
}
