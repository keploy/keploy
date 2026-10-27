//go:build darwin

package routes

import (
	"fmt"
	"net/http"
	"net/netip"
)

// DarwinCallerPID resolves the PID of the process that owns the client end of
// an HTTP request's TCP connection on macOS.
//
// It is nil in the OSS build and registered by the enterprise darwin hooks at
// agent startup. The interception shim sends its PID in every CONNECT message
// to the agent's control socket. When the connection targets the agent's own
// HTTP port the hooks record the PID keyed by the connection's source port;
// this function looks it up by the same source port seen in r.RemoteAddr.
var DarwinCallerPID func(remotePort uint16) (int, bool)

func callerPID(r *http.Request) (int, error) {
	if DarwinCallerPID == nil {
		return 0, fmt.Errorf("no darwin caller resolver registered; the macOS interception shim may not be active")
	}
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return 0, fmt.Errorf("the connection's remote address %q: %w", r.RemoteAddr, err)
	}
	pid, ok := DarwinCallerPID(remote.Port())
	if !ok {
		return 0, fmt.Errorf("no shim-registered process for source port %d; the connection may not have passed through the interception shim", remote.Port())
	}
	return pid, nil
}
