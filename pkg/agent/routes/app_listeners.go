package routes

import (
	"context"
	"net/http"
	"net/netip"
	"strconv"

	"github.com/go-chi/render"
	"go.keploy.io/server/v3/pkg/models"
)

type appListenAddrsReader interface {
	AppListenAddrs(ctx context.Context, port uint16) ([]netip.Addr, error)
}

// HandleAppListenAddrs answers GET /app/listen-addrs?port=N with the addresses
// the app's sockets listen on for its port N, as seen from the network
// namespace the agent shares with the app. 501 from an agent that cannot tell,
// and 500 when it cannot tell right now: both mean "unknown", never "nothing".
func (a *Agent) HandleAppListenAddrs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	reader, ok := a.svc.(appListenAddrsReader)
	if !ok {
		render.Status(r, http.StatusNotImplemented)
		render.JSON(w, r, map[string]string{"error": "this agent cannot report where the app listens"})
		return
	}
	port, err := strconv.ParseUint(r.URL.Query().Get("port"), 10, 16)
	if err != nil || port == 0 {
		render.Status(r, http.StatusBadRequest)
		render.JSON(w, r, map[string]string{"error": "port must be a TCP port number"})
		return
	}
	addrs, err := reader.AppListenAddrs(r.Context(), uint16(port))
	if err != nil {
		render.Status(r, http.StatusInternalServerError)
		render.JSON(w, r, map[string]string{"error": err.Error()})
		return
	}
	resp := models.AppListenAddrs{Addrs: make([]string, 0, len(addrs))}
	for _, addr := range addrs {
		resp.Addrs = append(resp.Addrs, addr.String())
	}
	render.Status(r, http.StatusOK)
	render.JSON(w, r, resp)
}
