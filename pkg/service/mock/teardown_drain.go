package mock

import (
	"time"

	"go.keploy.io/server/v3/utils"
	"golang.org/x/sync/errgroup"
)

// teardownDrainStall bounds the drain of a mock run's goroutines at its end:
// how long, in time keploy could run, the run's teardown may make no
// progress (teardownDrain).
const teardownDrainStall = 30 * time.Second

// teardownDrain waits for g, the goroutines of the mock run whose teardown
// scope is scope, for as long as that teardown moves (utils.TeardownScope:
// the agent's stop reporting its steps, the test command or the daemons it
// left taking theirs on their way out, a compose project's down), and gives
// it up only once the teardown has made no progress for teardownDrainStall
// of keploy's runnable time. A fixed 30 s of wall time forced the shutdown of
// a stop still under way on a loaded host. A drain that gives up returns
// nil: a timeout never fails the teardown (utils.DrainErrGroup).
func (m *mockService) teardownDrain(name string, g *errgroup.Group, scope *utils.TeardownScope) error {
	err, _ := utils.DrainErrGroupProgressOn(m.drainClock, m.logger, name, g, teardownDrainStall, scope.Moves)
	return err
}
