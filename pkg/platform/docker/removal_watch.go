package docker

import (
	"context"
	"sync"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"go.uber.org/zap"
)

// ContainerWaiter is the part of the engine's API a RemovalWatch takes.
type ContainerWaiter interface {
	ContainerWait(ctx context.Context, containerID string, condition container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
}

// RemovalWatch is the engine's word that a container run with --rm has been
// removed, asked for before keploy stops it.
//
// keploy's stop of a docker-run agent returns once the agent's container has
// exited, and then kills its `docker run` client, which was itself waiting
// for the removal. The engine removes the container after that, on its own,
// and frees the volumes it mounts only then: a volume removed right after
// the stop is refused as in use. Nor can the removal be asked about after
// the stop: the engine takes a container out of its lists before it frees
// its volumes, so a list or a wait asked in between finds no container while
// the volume is still held. A wait asked before the stop is answered only
// once the engine has freed them.
type RemovalWatch struct {
	api  ContainerWaiter
	name string

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	said   <-chan container.WaitResponse
	failed <-chan error
}

// NewRemovalWatch is a watch of the removal of the container named name.
func NewRemovalWatch(api ContainerWaiter, name string) *RemovalWatch {
	ctx, cancel := context.WithCancel(context.Background())
	return &RemovalWatch{api: api, name: name, ctx: ctx, cancel: cancel}
}

// Watch asks the engine to say when the container is removed. It returns
// once the engine has taken the question -- the engine answers a wait's
// request only after it has registered it -- or once ask ends (keploy's
// bound on an answer from the engine's daemon). A daemon that has not taken
// the question by then is not waited for: the question is withdrawn, and the
// stop goes on. Wait then finds the engine's wait ended and returns at once.
// A question the engine has taken is kept past ask, for Wait -- unless ask
// ends in the moment between the engine's taking it and Watch's return. It
// is withdrawn then too, and Wait returns at once: the run's volumes may
// still be held when RemoveRunVolumes asks, which leaves them for a later
// run's SweepRunVolumes.
func (w *RemovalWatch) Watch(ask context.Context) {
	withdraw := context.AfterFunc(ask, w.cancel)
	said, failed := w.api.ContainerWait(w.ctx, w.name, container.WaitConditionRemoved)
	withdraw()
	w.mu.Lock()
	w.said, w.failed = said, failed
	w.mu.Unlock()
}

// Wait returns once the engine has said the container is removed, or that
// it is gone or could not be removed, or once ctx ends. It returns at once
// when Watch never asked -- the container ended on its own, and its `docker
// run` client, which waits for the removal, was not killed -- and when the
// container still runs: the stop did not reach it, and nothing removes it.
func (w *RemovalWatch) Wait(ctx context.Context, logger *zap.Logger) {
	defer w.cancel()
	w.mu.Lock()
	said, failed := w.said, w.failed
	w.mu.Unlock()
	if said == nil {
		return
	}
	if info, err := w.api.ContainerInspect(ctx, w.name); err == nil && info.ContainerJSONBase != nil && info.State != nil && info.State.Running {
		logger.Debug("the container still runs; not waiting for its removal", zap.String("container", w.name))
		return
	}
	select {
	case <-said:
	case err := <-failed:
		if !cerrdefs.IsNotFound(err) {
			logger.Debug("the engine did not say the container was removed", zap.String("container", w.name), zap.Error(err))
		}
	case <-ctx.Done():
	}
}
