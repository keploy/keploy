package app

import (
	"context"
	"time"

	"github.com/docker/docker/api/types/container"
)

// ExitedState reports how the app's container ended once it no longer runs:
// it exited, died, or waits on its restart policy to start it again after an
// exit. exited is false while it runs, before it ever started, when the run
// has no app container, and when docker cannot say -- none of which is an
// exit.
//
// Under docker compose keploy's agent is a service in the app's project, and
// compose stops it the moment the app exits: an agent that stops answering
// during the bring-up may be one that went with the app, and then the app's
// exit is what failed.
func (a *App) ExitedState(ctx context.Context) (name string, state container.State, exited bool) {
	if a.container == "" || a.docker == nil {
		return "", container.State{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	st, err := a.containerState(ctx, a.container)
	if err != nil || !(st.Restarting || st.Status == "exited" || st.Status == "dead") {
		return "", container.State{}, false
	}
	return a.container, *st, true
}
