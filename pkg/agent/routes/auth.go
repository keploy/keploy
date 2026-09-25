// Package routes defines the routes for the agent service.
package routes

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
)

// agentRoutePrefix is where DefaultRoutes.New mounts every route.
const agentRoutePrefix = "/agent"

// isAuthExempt reports whether a path is served without a token. Exactly one
// is: liveness takes no input, returns no captured data, and callers need it
// before they can be sure the agent is up at all.
//
// A switch rather than a package-level map, so the allow-list cannot be
// appended to from somewhere else at run time — adding to it should require
// editing this function.
func isAuthExempt(path string) bool {
	switch path {
	case "/agent/health":
		return true
	default:
		return false
	}
}

// Authenticate guards the control-plane API.
//
// The API is worth guarding because of what it carries, not because of who can
// route to it: GET /agent/pcap/keylog streams live TLS session keys,
// GET /agent/pcap/traffic streams captured traffic, and POST /agent/stop and
// /agent/storemocks mutate a running session. Binding to loopback (native) and
// publishing only to the host's loopback (docker) narrowed the network reach,
// but every one of these is still reachable by:
//
//   - any other local user or process on a shared dev box or CI runner, since
//     loopback is not a privilege boundary;
//   - any container on the same compose network, and the application under test
//     itself, which runs in the agent's own network namespace
//     (network_mode: service:keploy-agent);
//   - a web page the developer happens to open, for the side-effecting routes.
//
// So: a bearer token on every route, compared in constant time.
//
// The Origin check is not redundant with the token. A browser cannot read a
// cross-origin response without CORS, but it CAN send a simple POST — no
// preflight, no custom headers — and POST /agent/stop ignores its request
// entirely, so the side effect lands. No programmatic client sends Origin, so
// refusing requests that carry one costs nothing and closes that path even if a
// token were to leak.
func Authenticate(logger *zap.Logger, sessionToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Browser-originated: refused before anything else, and refused
			// even on exempt paths, because the point is to keep pages from
			// probing the API at all.
			if r.Header.Get("Origin") != "" {
				logger.Warn("rejecting agent API request carrying an Origin header; no keploy client sends one, so this is a browser",
					zap.String("path", r.URL.Path), zap.String("origin", r.Header.Get("Origin")))
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}

			if isAuthExempt(r.URL.Path) || sessionToken == "" {
				next.ServeHTTP(w, r)
				return
			}

			if !tokenMatches(r.Header.Get("Authorization"), sessionToken) {
				// The keploy CLI deliberately sends a wrong token here once per
				// run to confirm this guard is live. Refusing it is the correct
				// and expected outcome, so it must not be reported as an
				// intruder — every healthy run would print a security warning.
				//
				// Matched exactly, against the mount prefix these routes are
				// registered under in DefaultRoutes.New. A suffix match would
				// also quiet a crafted path like /agent/stop/../<probe>, which
				// costs nothing to refuse loudly.
				if r.URL.Path == agentRoutePrefix+token.ProbePath {
					logger.Debug("refused the keploy control-plane authentication probe, as expected",
						zap.String("path", r.URL.Path))
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				logger.Warn("rejecting unauthenticated agent API request",
					zap.String("path", r.URL.Path), zap.String("remote", r.RemoteAddr),
					zap.String("next_step", nextStepFor(r.URL.Path)))
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// nextStepFor tells the operator what to do about a rejected request.
//
// The scope API gets its own answer because it is the one route a request can
// legitimately arrive on from code keploy did not write — a user's test runner
// — and because the published examples for it predate authentication. A runner
// that swallows the error (the documented pytest fixture does) would otherwise
// just stop scoping, with nothing but a bare 401 in the log to explain it.
func nextStepFor(path string) string {
	if strings.HasPrefix(path, "/agent/scope/") {
		return "your test runner must send 'Authorization: Bearer $" + token.MockAgentTokenEnv +
			"' on the scope API; keploy exports that variable into the wrapped command alongside KEPLOY_MOCK_AGENT"
	}
	return "this API is reachable only by the keploy process that started this agent; if you are that process, check that the agent was started with a token"
}

// tokenMatches compares an Authorization header against the session token
// without leaking the token's contents through timing.
func tokenMatches(header, sessionToken string) bool {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := header[len(prefix):]
	// ConstantTimeCompare is only constant-time for equal lengths, and it
	// returns 0 on a length mismatch, which already leaks length — acceptable
	// here since the length is a fixed, public 64 hex chars.
	return subtle.ConstantTimeCompare([]byte(got), []byte(sessionToken)) == 1
}

// ConsumeSessionToken resolves the token the process that started this agent
// passed down, and reports loudly when there isn't one — an agent launched
// without a token serves its control plane to anything that can reach it.
//
// It consumes rather than merely reads: the token file is unlinked once it has
// been read, so calling this twice is not the same as calling it once.
//
// The file comes first because it is the only handoff that survives the common
// native path: the CLI starts the agent as `sudo keploy agent`, and sudo's
// default env_reset drops the variable. The environment serves the container
// modes, where the docker or compose client passes it straight through.
//
// A --token-file that yields no token is an error, and the agent must not
// start: only a launcher that hands its agent a token passes the flag at all,
// so when the file cannot be read the handoff has failed or something else is
// sitting at that path. Falling back to the environment there would serve the
// control plane open — or, with a planted file, enforce a token the real
// client does not hold. Serving unauthenticated remains only for an agent
// started with no handoff whatsoever, which is what a launcher that predates
// the token does.
//
// isDocker is the agent's --is-docker: whether it runs in a container, which is
// what tells an in-cluster agent apart (see inClusterAgent).
func ConsumeSessionToken(logger *zap.Logger, tokenFile string, isDocker bool) (string, error) {
	if tokenFile != "" {
		tok, err := token.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("the keploy client that started this agent passed --token-file, but no control-plane token could be read from it, "+
				"so the agent will not start with its control plane open: %w", err)
		}
		// Read once and then gone: it has done its job, and leaving it
		// behind leaves the session's token on disk for the rest of the run.
		if rmErr := os.Remove(tokenFile); rmErr != nil {
			logger.Debug("could not remove the agent control-plane token file after reading it",
				zap.String("path", tokenFile), zap.Error(rmErr))
		}
		// So that anything in this process that later asks for the
		// session token gets the one being enforced, rather than
		// minting an unrelated value that would look just as valid.
		token.Adopt(tok)
		return tok, nil
	}

	tok := token.FromEnv()
	token.Adopt(tok)
	if tok != "" {
		return tok, nil
	}
	if inClusterAgent(isDocker) {
		// In-cluster agents do not use token authentication: the cluster
		// network is trusted, and nothing in Kubernetes hands an agent a
		// token. So this is the state every such agent is in by design, not a
		// handoff that broke. Said once, and at info, with no remedy: the
		// warning below names handoffs (--token-file, the launching client's
		// environment) that do not exist in a pod, for a state that needs no
		// fixing.
		logger.Info("agent control-plane API is running without authentication: this agent runs in a Kubernetes pod, " +
			"and in-cluster agents do not use token authentication because the cluster network is trusted, so anything " +
			"that can reach this pod's agent port can use the API")
		return "", nil
	}
	logger.Warn("agent control-plane API is running WITHOUT authentication: the process that started this agent supplied no " + token.Env +
		" and no --token-file. Any local user or neighbouring container that can reach the port can read TLS session keys and " +
		"captured traffic, and can stop or alter the session.")
	return "", nil
}

// inClusterAgent reports whether this agent is one Kubernetes runs, as a
// sidecar or in a replay pod: a container (--is-docker) with
// KUBERNETES_SERVICE_HOST set, which the kubelet puts in every container it
// starts.
//
// Both, not the variable alone. keploy run natively as root inside a CI pod
// starts its agent without sudo, so that agent inherits the variable too — and
// a handoff that failed there is a local one like any other. A container keploy
// launches with docker gets only the environment keploy gives it, which never
// includes the variable.
func inClusterAgent(isDocker bool) bool {
	return isDocker && os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}
