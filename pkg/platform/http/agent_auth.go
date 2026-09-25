package http

import (
	"net/http"
	"regexp"
	"time"

	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
)

// agentHookTimeout is the deadline the /hooks/* calls run under. It was
// spelled out at each of those call sites before they shared a client.
const agentHookTimeout = 50 * time.Second

// bearerTransport attaches the session's control-plane token to every request
// made through a client built here.
//
// A RoundTripper rather than a header set at each call site: AgentClient builds
// requests in more than twenty places, including the long-lived gob streams,
// and a missed one is not a compile error — it is a request that fails at
// runtime, in whichever mode happens to exercise it.
//
// The transport only covers requests that actually go through one of these
// clients, which is why every client AgentClient uses is built by a
// constructor in this file. A call site that reaches for &http.Client{}
// directly gets no token and a 401, which is how the /hooks/* calls were
// missed the first time round.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.token == "" {
		return base.RoundTrip(req)
	}
	// Per the RoundTripper contract the request must not be mutated, so clone
	// before adding the header.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.token)
	return base.RoundTrip(clone)
}

// agentHTTPClient returns the client the AgentClient uses to reach its agent,
// carrying the token when one is in play for this session.
func agentHTTPClient(token string) http.Client {
	return agentHTTPClientWithTimeout(token, 0)
}

// agentHTTPClientWithTimeout is agentHTTPClient with a per-request deadline,
// for the /hooks/* calls that want one. Same transport: the timeout is the only
// thing that differs, and building a bare client to get it is what left those
// calls unauthenticated.
func agentHTTPClientWithTimeout(token string, timeout time.Duration) http.Client {
	c := http.Client{Timeout: timeout}
	if token != "" {
		c.Transport = &bearerTransport{token: token}
	}
	return c
}

// tokenInCommand matches the control-plane token wherever it appears as an
// environment assignment inside a command line.
var tokenInCommand = regexp.MustCompile(`(` + token.Env + `|` + token.MockAgentTokenEnv + `)=[^\s'"]+`)

// redactToken removes the control-plane token from a command line before it is
// logged.
//
// The docker alias names the token without a value (`-e KEPLOY_AGENT_TOKEN`)
// and carries it only in the environment, which a logged command line does not
// include. But the alias also carries whatever extra arguments the caller
// supplied, and debug logs are the first thing a user pastes into a bug report,
// so an assignment that did end up in one is still scrubbed.
func redactToken(cmd string) string {
	return tokenInCommand.ReplaceAllString(cmd, "${1}=<redacted>")
}

// appendTokenFileArg adds the --token-file argument a natively spawned agent
// needs in order to authenticate its control plane.
//
// By path, not by value. The agent is usually started through sudo, whose
// default env_reset drops the variable, so the token cannot ride the
// environment here; and it must not ride argv either, where /proc/<pid>/cmdline
// and `ps` would show it to every user on the box. So the token sits in a 0600
// file in a directory only this user can enter, and only its path is passed.
//
// A failure is not fatal: the agent then starts unauthenticated and says so,
// which is how it behaved before this existed, rather than leaving the user
// unable to record at all.
func appendTokenFileArg(logger *zap.Logger, args []string) []string {
	tokenFile, err := token.WriteFile()
	if err != nil {
		logger.Warn("could not hand a control-plane token to the agent; it will start without authentication",
			zap.Error(err))
		return args
	}
	return append(args, "--token-file", tokenFile)
}
