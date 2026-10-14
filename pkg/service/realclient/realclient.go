// Package realclient is the CLI's side of mock.real (design v14 §4): the
// list handed to the agent as a run starts, and what the agent saw of it,
// read as the run ends. keploy mock, keploy record and keploy test share it.
package realclient

import (
	"context"
	"errors"
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Agent is an optional Instrumentation extension: under the proven switch the
// CLI hands its agent the run's mock.real as the run starts, and reads at its
// end what the run saw of it beyond its tests.
type Agent interface {
	PushRealDeps(ctx context.Context, real []string) error
	RealRun(ctx context.Context) (models.RealRun, error)
}

// Check refuses, before anything runs, a mock.real keploy cannot read (proven
// switch only; without it the key is not read).
func Check(real []string) error {
	if !provenswitch.On() {
		return nil
	}
	if _, err := realdeps.Parse(real); err != nil {
		return fmt.Errorf("keploy.yml mock.real: %w", err)
	}
	return nil
}

// Push hands inst's agent the run's mock.real (proven switch only), an empty
// list too: it clears what an agent that served an earlier run kept. An agent
// client or an agent that cannot take a list that names anything is an
// error: those destinations would be recorded and mocked instead of reached.
// An agent without the route (another build) has nothing to clear when the
// list is empty.
func Push(ctx context.Context, logger *zap.Logger, inst any, real []string) error {
	if !provenswitch.On() {
		return nil
	}
	empty := len(real) == 0
	a, ok := inst.(Agent)
	if !ok {
		if empty {
			return nil
		}
		return errors.New("this keploy's agent client cannot hand its agent mock.real")
	}
	if err := a.PushRealDeps(ctx, real); err != nil {
		if empty && errors.Is(err, models.ErrNoRealRoute) {
			return nil
		}
		return err
	}
	if !empty {
		logger.Info("these destinations are reached for real in this run, never recorded or mocked (mock.real)", zap.Strings("real", real))
	}
	return nil
}

// keeper is an agent client that pushes a later part of the same run.
type keeper interface {
	KeepRealDeps(ctx context.Context, real []string) error
}

// Keep is Push for a later part of the same run (keploy test's next test
// set): an agent that already holds the list keeps what the run saw of it,
// and one that holds another, or none, installs it afresh. A client that
// cannot keep pushes afresh.
func Keep(ctx context.Context, logger *zap.Logger, inst any, real []string) error {
	if !provenswitch.On() {
		return nil
	}
	if k, ok := inst.(keeper); ok {
		if err := k.KeepRealDeps(ctx, real); err != nil && !(len(real) == 0 && errors.Is(err, models.ErrNoRealRoute)) {
			return err
		}
		return nil
	}
	return Push(ctx, logger, inst, real)
}

// Run reads what the run saw of mock.real beyond its tests, after the run's
// last windows (proven switch only; nil without it, or with an empty list). A
// read that fails is an error: a REAL destination never reached, or a
// connection keploy could not capture outside every test, must not go
// unsaid.
func Run(ctx context.Context, inst any, real []string) (*models.RealRun, error) {
	if !provenswitch.On() || len(real) == 0 {
		return nil, nil
	}
	a, ok := inst.(Agent)
	if !ok {
		return nil, errors.New("this keploy's agent client cannot read the run's mock.real destinations")
	}
	run, err := a.RealRun(ctx)
	if err != nil {
		return nil, err
	}
	return &run, nil
}
