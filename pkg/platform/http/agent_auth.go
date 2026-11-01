package http

import (
	"net/http"
	"regexp"

	"go.keploy.io/server/v3/pkg/agent/token"
	"go.uber.org/zap"
)

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

// ownConnections returns rt sending each request on a connection of its own,
// never one kept from an earlier request (http.Transport.DisableKeepAlives).
// A RoundTripper it does not know is returned as it is.
func ownConnections(rt http.RoundTripper) http.RoundTripper {
	switch t := rt.(type) {
	case nil:
		return ownConnections(http.DefaultTransport)
	case *bearerTransport:
		c := *t
		c.base = ownConnections(t.base)
		return &c
	case *instanceTransport:
		c := *t
		c.base = ownConnections(t.base)
		return &c
	case *http.Transport:
		c := t.Clone()
		c.DisableKeepAlives = true
		return c
	default:
		return rt
	}
}

// ownConnection is the agent's client with every request on a connection of
// its own (ownConnections), for a request that can be cancelled before it is
// answered, from outside the call that waits for it, when the agent answers
// with no body. The transport puts a kept connection back in its pool before it
// hands the request such an answer, so the cancel can find the request still
// waiting and close that connection under the next request, which took it from
// the pool: that one fails with this request's cancel, on a context nothing
// cancelled, and keploy's read of what a run did failed so ("failed to get
// scope windows ...: context canceled") after a run that passed.
func (a *AgentClient) ownConnection() http.Client {
	client := a.client
	client.Transport = ownConnections(client.Transport)
	return client
}

// agentHTTPClient returns the client the AgentClient uses to reach its agent,
// carrying the token when one is in play for this session.
func agentHTTPClient(token string) http.Client {
	var c http.Client
	var base http.RoundTripper
	if token != "" {
		base = &bearerTransport{token: token}
	}
	// Every answer is noted for which agent process gave it
	// (agent_instance.go), with or without a token.
	c.Transport = &instanceTransport{base: base}
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
