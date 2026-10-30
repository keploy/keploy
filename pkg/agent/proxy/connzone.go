package proxy

import (
	"context"
	"errors"
	"net"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// connZoneOf is, for a docker-mode agent, the zone of the app container that
// made conn, an accepted connection: its remote end is the app's socket, its
// local end the proxy's (containerkey.ConnZone).
func connZoneOf(ctx context.Context, conn net.Conn) (learn.ProcZone, error) {
	app, okApp := clientEnd(conn.RemoteAddr())
	proxy, okProxy := clientEnd(conn.LocalAddr())
	if !okApp || !okProxy {
		return learn.ProcZone{}, errors.New("keploy cannot tell which app container made the connection: it is no TCP connection")
	}
	return connZone(ctx, app, proxy)
}

// connZone is containerkey.ConnZone, swapped by tests.
var connZone = containerkey.ConnZone
