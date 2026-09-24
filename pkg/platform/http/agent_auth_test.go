package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/routes"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// guardedAgent stands in for a real agent: it enforces the same middleware the
// agent installs, so a client request that carries no token is refused here for
// exactly the reason it would be refused in a live run.
func guardedAgent(t *testing.T, token string) (*AgentClient, *[]string) {
	t.Helper()

	var mu sync.Mutex
	var seen []string
	handler := routes.Authenticate(zap.NewNop(), token)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &AgentClient{
		logger:     zap.NewNop(),
		client:     agentHTTPClient(token),
		hookClient: agentHTTPClientWithTimeout(token, agentHookTimeout),
		conf:       &config.Config{Agent: config.Agent{AgentURI: srv.URL}},
	}, &seen
}

// TestAgentHooks_AuthenticateAgainstAGuardedAgent is the regression test for a
// real break: the five /hooks/* methods each built their own
// &http.Client{Timeout: 50s} instead of going through the client that carries
// the bearer token, so every one of them got a 401 the moment the agent started
// enforcing auth. Nothing about that was a compile error.
func TestAgentHooks_AuthenticateAgainstAGuardedAgent(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	hooks := []struct {
		name string
		path string
		call func(*AgentClient) error
	}{
		{"BeforeSimulate", "/hooks/before-simulate", func(a *AgentClient) error {
			now := time.Now()
			return a.BeforeSimulate(context.Background(), &now, "test-set-0", "test-1")
		}},
		{"AfterSimulate", "/hooks/after-simulate", func(a *AgentClient) error {
			return a.AfterSimulate(context.Background(), "test-1", "test-set-0")
		}},
		{"BeforeTestRun", "/hooks/before-test-run", func(a *AgentClient) error {
			return a.BeforeTestRun(context.Background(), "test-run-0")
		}},
		{"BeforeTestSetCompose", "/hooks/before-test-set-compose", func(a *AgentClient) error {
			return a.BeforeTestSetCompose(context.Background(), "test-run-0", "test-set-0", true)
		}},
		{"AfterTestRun", "/hooks/after-test-run", func(a *AgentClient) error {
			return a.AfterTestRun(context.Background(), "test-run-0", []string{"test-set-0"}, models.TestCoverage{})
		}},
	}

	for _, h := range hooks {
		t.Run(h.name, func(t *testing.T) {
			client, seen := guardedAgent(t, token)
			// The hook methods swallow transport errors and return nil, so a
			// nil error alone proves nothing. What proves it is that the
			// request reached the handler behind the middleware.
			require.NoError(t, client.call(h.call))
			require.Equal(t, []string{h.path}, *seen,
				"the request never got past the agent's auth middleware")
		})
	}
}

// call runs f, keeping the table above readable.
func (a *AgentClient) call(f func(*AgentClient) error) error { return f(a) }

// TestAgentHooks_SurfaceARejectionRatherThanPassingSilently pins the other half:
// a hook that IS rejected must not be mistaken for a hook that succeeded.
func TestAgentHooks_SurfaceARejectionRatherThanPassingSilently(t *testing.T) {
	client, seen := guardedAgent(t, "the-agents-token")
	// A client holding the wrong token is what a botched handoff looks like.
	client.hookClient = agentHTTPClientWithTimeout("a-different-token", agentHookTimeout)

	now := time.Now()
	err := client.BeforeSimulate(context.Background(), &now, "test-set-0", "test-1")
	require.Error(t, err, "a 401 from the agent must reach the caller, not be logged and dropped")
	require.Empty(t, *seen)
}

func TestAgentHTTPClient_SendsTheTokenAsABearerCredential(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := agentHTTPClient("tok")
	resp, err := c.Get(srv.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, "Bearer tok", got)
}

func TestAgentHTTPClient_WithoutATokenSendsNoAuthorizationHeader(t *testing.T) {
	// An agent launched by something that predates the token serves
	// unauthenticated; sending a bogus "Bearer " would be worse than sending
	// nothing.
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header["Authorization"]
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := agentHTTPClient("")
	resp, err := c.Get(srv.URL)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.False(t, present)
}

func TestBearerTransport_DoesNotMutateTheCallersRequest(t *testing.T) {
	// http.RoundTripper's contract forbids it, and a mutated request is a real
	// hazard for the retried and replayed requests AgentClient builds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	require.NoError(t, err)

	tr := &bearerTransport{token: "tok"}
	resp, err := tr.RoundTrip(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Empty(t, req.Header.Get("Authorization"),
		"RoundTrip mutated the request it was given")
}

func TestAgentHookTimeout_IsPreservedByTheAuthenticatedClient(t *testing.T) {
	// The bare clients this replaced carried a 50s deadline. Dropping it while
	// adding the token would turn a hung agent into a hung test run.
	require.Equal(t, agentHookTimeout, agentHTTPClientWithTimeout("tok", agentHookTimeout).Timeout)
	require.Equal(t, 50*time.Second, agentHookTimeout)
}

func TestAppendTokenFileArg_PassesThePathAndNeverTheToken(t *testing.T) {
	args := appendTokenFileArg(zap.NewNop(), []string{"--port", "16789"})

	require.Len(t, args, 4, "the agent was spawned without --token-file and will serve unauthenticated")
	require.Equal(t, "--token-file", args[2])

	path := args[3]
	t.Cleanup(func() { _ = token.RemoveFile() })

	got, err := token.ReadFile(path)
	require.NoError(t, err, "--token-file points at something the agent cannot read a token from")
	require.Equal(t, token.Session(), got)

	// argv is world-readable through /proc/<pid>/cmdline; the token must never
	// be in it, only the path to a 0600 file.
	for _, a := range args {
		require.NotContains(t, a, token.Session(), "the token itself ended up in the agent's command line")
	}
}

func TestRedactToken_KeepsTheTokenOutOfLogs(t *testing.T) {
	// The docker alias normally carries only a path, but the windows arm falls
	// back to an inline -e NAME=value. Debug logs are the first thing a user
	// pastes into a bug report.
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "agent token as a docker env",
			cmd:  "docker run -e " + token.Env + "=abc123 --rm img",
			want: "docker run -e " + token.Env + "=<redacted> --rm img",
		},
		{
			name: "scope token as a docker env",
			cmd:  "docker run -e " + token.MockAgentTokenEnv + "=abc123 --rm img",
			want: "docker run -e " + token.MockAgentTokenEnv + "=<redacted> --rm img",
		},
		{
			name: "a command with no token is untouched",
			cmd:  "docker run --env-file /tmp/keploy-agent-token-42 --rm img",
			want: "docker run --env-file /tmp/keploy-agent-token-42 --rm img",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactToken(tt.cmd)
			require.Equal(t, tt.want, got)
			if tt.cmd != tt.want {
				require.NotContains(t, got, "abc123")
			}
		})
	}
}
