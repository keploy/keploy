// Package routes defines the routes for the agent service.
package routes

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// podIPOnce caches the result of utils.GetContainerIPv4 so the network
// interface scan runs at most once per process.
var (
	podIPOnce  sync.Once
	podIPValue string
	podIPErr   error
)

// resolvePodIP returns the pod's non-loopback IPv4 address, cached after the
// first call. It is a var so unit tests can replace it without triggering a
// real network-interface scan.
var resolvePodIP = func() (string, error) {
	podIPOnce.Do(func() {
		podIPValue, podIPErr = utils.GetContainerIPv4()
	})
	return podIPValue, podIPErr
}

// HandleRelay is a reverse-proxy handler that forwards an incoming HTTP
// request to the app container on the pod's real IP address.
//
// Route: /agent/relay/{port}/*
//
// The port is extracted from the URL path; the remainder of the path (after
// /agent/relay/{port}/) becomes the forwarded request's path. The original
// Host header is preserved so the app sees the same Host it saw in
// production — this is the fix for issue #1062, where containerd's
// port-forward delivered requests on localhost and triggered DNS-rebinding
// guards (mcp-go, etc.).
func (a *Agent) HandleRelay(w http.ResponseWriter, r *http.Request) {
	portStr := extractRelayPort(r.URL.Path)
	if portStr == "" {
		http.Error(w, "missing port in relay path", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, fmt.Sprintf("invalid port %q: must be 1-65535", portStr), http.StatusBadRequest)
		return
	}

	podIP, err := resolvePodIP()
	if err != nil {
		a.logger.Error("relay: failed to determine pod IP", zap.Error(err))
		http.Error(w, "failed to determine pod IP", http.StatusBadGateway)
		return
	}

	forwardPath := extractRelayForwardPath(r.URL.Path, portStr)

	target := &url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(podIP, portStr),
		Path:     forwardPath,
		RawQuery: r.URL.RawQuery,
	}

	// Preserve the original Host header from the incoming request.
	// By default httputil.ReverseProxy sets req.Host = req.URL.Host,
	// which would be podIP:port — the app must see the original Host
	// (e.g. "api.keploy.io") for DNS-rebinding guards to pass.
	originalHost := r.Host

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = target
			req.Host = originalHost
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			a.logger.Error("relay: upstream dial failed",
				zap.String("target", target.String()), zap.Error(err))
			http.Error(w, fmt.Sprintf("relay: upstream dial failed: %v", err), http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
}

// extractRelayPort extracts the port segment from a path like
// /agent/relay/{port}/... . Returns "" if the path does not match.
func extractRelayPort(path string) string {
	const prefix = "/agent/relay/"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := path[len(prefix):]
	if rest == "" {
		return ""
	}
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// extractRelayForwardPath extracts the path after /agent/relay/{port},
// including the leading slash. Returns "/" if there is no trailing path.
func extractRelayForwardPath(path, port string) string {
	prefix := "/agent/relay/" + port
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || rest[0] != '/' {
		return "/" + rest
	}
	return rest
}
