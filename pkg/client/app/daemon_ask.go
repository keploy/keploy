package app

import (
	"context"
	"fmt"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
)

// A READ OF THE DOCKER DAEMON ENDS ON ITS ANSWER, NEVER ON A NUMBER OF SECONDS.
//
// keploy's reads of the daemon (an inspect, a list) each had a wall-clock
// budget of 2 s. On a loaded host the daemon's answer
// took longer, the read gave up, and its failure was taken for an answer: a
// container that had exited for one that had not, a project keploy could not
// list for one it had to take down whole, a compose label it could not read
// for none, so the down fell back to the project compose takes from the cwd.
// A read the daemon fails says nothing, so keploy asks again; what bounds the
// wait for the daemon's answer is the caller's ctx and keploy's runnable time
// (utils.StopStallBudget of it with no answer, stall.Meter), never a fixed
// short budget.

// askDaemon is the daemon's answer to ask, however slow the daemon is to give
// it. An ask the daemon fails (its connection broke, or a loaded daemon was
// slow and the call gave up) is asked again, at once and then at a walking
// pace (composeDaemonAskPace), until the daemon answers (a nil error), says
// it has no such object (a NotFound error, returned as it came: an answer
// too), ctx ends, or utils.StopStallBudget of keploy's runnable time passes
// with no answer. The error is then the last failure's, wrapped; what, in the
// log, names what keploy asked for and what it does with no answer.
func askDaemon[T any](ctx context.Context, logger *zap.Logger, what string, ask func(context.Context) (T, error)) (T, error) {
	meter := stall.NewMeter(nil, utils.StopStallBudget)
	defer meter.Stop()
	type answer struct {
		v   T
		err error
	}
	var zero T
	var last error
	unanswered := func(why string) (T, error) {
		err := fmt.Errorf("the docker daemon did not answer keploy's read of %s: %s (last failure: %v)", what, why, last)
		logger.Debug("the docker daemon did not answer", zap.String("read", what), zap.String("why", why), zap.Error(last))
		return zero, err
	}
	for asked := 0; ; asked++ {
		if asked > 1 {
			select {
			case <-ctx.Done():
				return unanswered("its caller stopped waiting")
			case <-time.After(composeDaemonAskPace):
			}
		}
		actx, cancel := context.WithCancel(ctx)
		answered := make(chan answer, 1)
		go func() {
			v, err := ask(actx)
			answered <- answer{v, err}
		}()
		var r answer
	wait:
		for {
			select {
			case r = <-answered:
				break wait
			case <-meter.Wait():
				if meter.Expired() {
					cancel()
					return unanswered("no answer for " + utils.StopStallSaid())
				}
			case <-ctx.Done():
				cancel()
				return unanswered("its caller stopped waiting")
			}
		}
		cancel()
		if r.err == nil || cerrdefs.IsNotFound(r.err) {
			return r.v, r.err
		}
		last = r.err
		if ctx.Err() != nil {
			return unanswered("its caller stopped waiting")
		}
		if meter.Expired() {
			return unanswered("no answer for " + utils.StopStallSaid())
		}
		logger.Debug("the docker daemon failed keploy's read; keploy asks it again", zap.String("read", what), zap.Error(r.err))
	}
}

// inspectContainer is the daemon's description of container ref, asked for
// with askDaemon: an answer that gives no state of the container is no
// answer, and is asked again. read is false when the daemon never answered,
// or said it has no such container (notFound).
func (a *App) inspectContainer(ctx context.Context, ref, what string) (info container.InspectResponse, read, notFound bool) {
	info, err := askDaemon(ctx, a.logger, what, func(ctx context.Context) (container.InspectResponse, error) {
		info, err := a.docker.ContainerInspect(ctx, ref)
		if err == nil && (info.ContainerJSONBase == nil || info.State == nil) {
			return info, fmt.Errorf("the daemon's description of container %q gave no state of it", ref)
		}
		return info, err
	})
	if err != nil {
		return container.InspectResponse{}, false, cerrdefs.IsNotFound(err)
	}
	return info, true, false
}
