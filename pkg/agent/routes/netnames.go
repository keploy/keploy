package routes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"go.keploy.io/server/v3/pkg/agent/netnames"
)

// maxNetworkNamesBody bounds a pushed name list: the names of a network's
// containers, a few dozen bytes each.
const maxNetworkNamesBody = 4 << 20

// HandleNetworkNames takes the keploy CLI's list of the names the replay's
// docker networks answer themselves (netnames.List), the only names a
// docker-mode replay's DNS mock miss is forwarded to docker's resolver for.
func (a *Agent) HandleNetworkNames(w http.ResponseWriter, r *http.Request) {
	var l netnames.List
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxNetworkNamesBody)).Decode(&l); err != nil {
		http.Error(w, fmt.Sprintf("invalid name list: %v", err), http.StatusBadRequest)
		return
	}
	netnames.Default().Apply(l)
	w.WriteHeader(http.StatusNoContent)
}

// HandleNetworkNamesAsk is the keploy CLI's ask, held open, for the agent's
// next ask for a fresh name list after the one named by after: a DNS mock miss
// for a name the last list does not hold (netnames.Store.Refresh). It answers
// with the ask once there is one, and says nothing when the CLI goes away.
func (a *Agent) HandleNetworkNamesAsk(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if err != nil && r.URL.Query().Get("after") != "" {
		http.Error(w, fmt.Sprintf("invalid after: %v", err), http.StatusBadRequest)
		return
	}
	ask, ok := netnames.Default().AwaitAsk(r.Context(), after)
	if !ok {
		return // the CLI went away
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ask)
}
