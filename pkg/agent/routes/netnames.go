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

// HandleNetworkNamesAsk is the keploy CLI's stream of the agent's asks for a
// fresh name list, held open: a DNS mock miss for a name the last list does
// not hold asks for one (netnames.Store.Refresh). It writes each ask after
// the one named by after as a line of JSON as it comes, until the CLI goes
// away. The CLI is done with the ask named by after, so a miss waiting for
// that one's list is answered at once.
func (a *Agent) HandleNetworkNamesAsk(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if err != nil && r.URL.Query().Get("after") != "" {
		http.Error(w, fmt.Sprintf("invalid after: %v", err), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "the agent cannot stream its asks for a name list", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	enc := json.NewEncoder(w)
	netnames.Default().Serve(r.Context(), after, func(ask netnames.Ask) error {
		if err := enc.Encode(ask); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})
}
