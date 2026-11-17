package replay

import (
	"context"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// appStartNotifier is an optional Instrumentation extension: the agent is
// told keploy starts the application anew for a test set (the keploy agent
// client's AppStartsAnew, agent.AppStartHook).
type appStartNotifier interface {
	AppStartsAnew(ctx context.Context, testSetID string, first time.Time) error
}

// tellAppStartsAnew tells the agent, under the proven switch, that keploy
// starts the application anew for the test set testSetID, whose test cases
// are tcs, once its mocks are stored and before the application starts
// (appStartsAnew reported it starts anew). The agent's clock then keeps no
// instant an earlier application read: a new process has read none, so the
// set's first case begins at its own recorded instant, never late for an
// instant only a stopped application saw. A notice the agent cannot take is
// said, and the set runs on: its clock then never moves back past what the
// earlier application read, and a test that checks a recorded token's age
// can fail for it.
func (r *Replayer) tellAppStartsAnew(ctx context.Context, testSetID string, tcs []*models.TestCase) {
	if !provenswitch.On() {
		return
	}
	n, ok := r.instrumentation.(appStartNotifier)
	if !ok {
		return
	}
	if err := n.AppStartsAnew(ctx, testSetID, earliestRecorded(tcs)); err != nil && ctx.Err() == nil {
		r.logger.Warn("keploy's agent could not be told the application starts anew for this test-set: its clock keeps what the earlier application read, and never moves back past it, so the set's tests can begin after their recorded instants",
			zap.String("test-set", testSetID), zap.Error(err),
			zap.String("next_step", "if a test that checks a recorded token's or cache's age fails, rerun the test-set alone, or re-record it"))
	}
}

// earliestRecorded is the earliest instant one of the test cases tcs was
// recorded at: its request's, the instant the agent's clock puts the case at
// (the replay hooks' beforeSimulate). Zero where none has one.
func earliestRecorded(tcs []*models.TestCase) time.Time {
	var first time.Time
	for _, tc := range tcs {
		if tc == nil {
			continue
		}
		at := tc.HTTPReq.Timestamp
		if tc.Kind == models.GRPC_EXPORT {
			at = tc.GrpcReq.Timestamp
		}
		if !at.IsZero() && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	return first
}
