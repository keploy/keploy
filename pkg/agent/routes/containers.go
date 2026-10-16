package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
)

// maxContainersBody bounds a pushed container list: a host's containers, a
// few hundred bytes each.
const maxContainersBody = 8 << 20

// HandleTakesContainers answers whether this agent names container
// destinations from lists its keploy CLI gives it: 204 when it does (a
// docker-mode agent), 409 when it reads Docker itself. Any client of the
// agent asks it, so whichever drives its sessions gives it the list.
func (a *Agent) HandleTakesContainers(w http.ResponseWriter, _ *http.Request) {
	if !containerkey.TakesPushed() {
		http.Error(w, containerkey.ErrNotPushed.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleContainers takes the keploy CLI's latest list of the local Docker
// daemon's containers, which a docker-mode agent names container
// destinations from (containerkey.ApplySnapshot). An agent that reads Docker
// itself refuses it.
func (a *Agent) HandleContainers(w http.ResponseWriter, r *http.Request) {
	var s containerkey.Snapshot
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxContainersBody)).Decode(&s); err != nil {
		http.Error(w, fmt.Sprintf("invalid container list: %v", err), http.StatusBadRequest)
		return
	}
	if err := containerkey.ApplySnapshot(s); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, containerkey.ErrNotPushed) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleSettleContainers is the keploy CLI's ask, held open, for its
// docker-mode agent's next settle (containerkey.AwaitSettleAsk): a
// connection's first use that found nothing placed asks the CLI to list the
// daemon. It answers with the agent's count of settles and its run once the
// agent has asked one more than after (any, when boot names another run), or
// says nothing when the CLI goes away. An agent that reads Docker itself
// refuses it.
func (a *Agent) HandleSettleContainers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, err := strconv.ParseUint(q.Get("after"), 10, 64)
	if err != nil && q.Get("after") != "" {
		http.Error(w, fmt.Sprintf("invalid after: %v", err), http.StatusBadRequest)
		return
	}
	ask, err := containerkey.AwaitSettleAsk(r.Context(), q.Get("boot"), after)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, containerkey.ErrNotPushed) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	if r.Context().Err() != nil {
		return // the CLI went away
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ask)
}
