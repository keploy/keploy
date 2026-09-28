package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	t.Cleanup(func() { _ = token.Cleanup() })
	args := appendTokenFileArg(zap.NewNop(), []string{"--port", "16789"})

	require.Len(t, args, 4, "the agent was spawned without --token-file and will serve unauthenticated")
	require.Equal(t, "--token-file", args[2])

	path := args[3]
	got, err := token.ReadFile(path)
	require.NoError(t, err, "--token-file points at something the agent cannot read a token from")
	require.Equal(t, token.Session(), got)

	// argv is world-readable through /proc/<pid>/cmdline; the token must never
	// be in it, only the path to a 0600 file.
	for _, a := range args {
		require.NotContains(t, a, token.Session(), "the token itself ended up in the agent's command line")
	}
}

// TestNativeAgentArgs_HandTheAgentItsToken pins the call site. appendTokenFileArg
// has its own test, but dropping the one line that calls it from the native
// launcher left every unit test green while every native agent came up with no
// token.
func TestNativeAgentArgs_HandTheAgentItsToken(t *testing.T) {
	t.Cleanup(func() { _ = token.Cleanup() })
	const agentURI = "http://localhost:26789/agent"
	a := New(zap.NewNop(), nil, &config.Config{})

	args := a.nativeAgentArgs(models.SetupOptions{AgentPort: 26789, ProxyPort: 26790, DnsPort: 26791, AgentURI: agentURI, Mode: models.MODE_RECORD})

	i := slices.Index(args, "--token-file")
	require.GreaterOrEqual(t, i, 0, "the native agent is started without --token-file: %v", args)
	require.Less(t, i+1, len(args))
	got, err := token.ReadFile(args[i+1])
	require.NoError(t, err)
	require.Equal(t, token.Session(), got, "the native agent is handed a token the client does not send")
	for _, arg := range args {
		require.NotContains(t, arg, token.Session(), "the token itself is in the native agent's argv")
	}
	_, launched := token.Launched(agentURI)
	require.True(t, launched, "the client does not know it launched this agent, so it will never check the agent enforces the token")
}

// TestNativeAgentArgs_AnAgentThatGotNoTokenIsStillChecked: a token file that
// cannot be written leaves the native agent without a token, serving open. The
// client's readiness check is what reports that, and it only checks agents
// this process launched — so the launch has to be on record whether or not the
// handoff went through, or the one agent that needs checking is the one that
// is skipped.
func TestNativeAgentArgs_AnAgentThatGotNoTokenIsStillChecked(t *testing.T) {
	require.NoError(t, token.Cleanup()) // so the next WriteFile needs a new private directory
	t.Cleanup(func() { _ = token.Cleanup() })
	unwritable := filepath.Join(t.TempDir(), "does-not-exist")
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, unwritable)
	}
	const agentURI = "http://localhost:26792/agent"
	a := New(zap.NewNop(), nil, &config.Config{})

	args := a.nativeAgentArgs(models.SetupOptions{AgentPort: 26792, ProxyPort: 26793, DnsPort: 26794, AgentURI: agentURI, Mode: models.MODE_RECORD})

	require.NotContains(t, args, "--token-file", "a token file was written to a temp directory that does not exist")
	_, launched := token.Launched(agentURI)
	require.True(t, launched, "an agent that got no token is not on record as launched, so nothing checks it and it serves open unreported")
}

// TestNew_BuildsClientsThatCarryTheSessionToken pins what New wires up. The
// transport has its own tests, but New handing it an empty token left them all
// green while every request the CLI made was a 401 — or, against an agent that
// was handed nothing either, unauthenticated.
func TestNew_BuildsClientsThatCarryTheSessionToken(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)

	a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
	_, err := a.GetMockErrors(context.Background()) // a.client
	require.NoError(t, err)
	require.NoError(t, a.BeforeTestRun(context.Background(), "test-run-0")) // a.hookClient

	mu.Lock()
	defer mu.Unlock()
	want := "Bearer " + token.Session()
	require.Equal(t, want, seen["/mockerrors"], "the client's requests do not carry the session token")
	require.Equal(t, want, seen["/hooks/before-test-run"], "the hook client's requests do not carry the session token")
}

// TestExportMockScopeEnv_HandsTheTokenOnlyToAMockRunner pins the guard around
// the one place the token leaves keploy on purpose. In mock mode the wrapped
// command is the user's test runner, the intended client of the scope API. In
// record and test mode it is the application under test, and moving the export
// outside the guard would hand it a working credential for /agent/pcap/keylog,
// /agent/stop and /agent/storemocks with no test noticing.
func TestExportMockScopeEnv_HandsTheTokenOnlyToAMockRunner(t *testing.T) {
	for _, name := range []string{"KEPLOY_MOCK_AGENT", token.MockAgentTokenEnv, "KEPLOY_MOCK_SESSION"} {
		t.Setenv(name, "") // restored afterwards
		require.NoError(t, os.Unsetenv(name))
	}

	exportMockScopeEnv(zap.NewNop(), models.SetupOptions{MockMode: false, Mode: models.MODE_RECORD}, 16789)
	for _, name := range []string{"KEPLOY_MOCK_AGENT", token.MockAgentTokenEnv} {
		_, set := os.LookupEnv(name)
		require.False(t, set, "%s was exported outside mock mode, into the application under test", name)
	}

	exportMockScopeEnv(zap.NewNop(), models.SetupOptions{MockMode: true, Mode: models.MODE_RECORD}, 16789)
	require.Equal(t, "http://localhost:16789", os.Getenv("KEPLOY_MOCK_AGENT"))
	require.Equal(t, token.Session(), os.Getenv(token.MockAgentTokenEnv),
		"a mock-mode runner gets no credential, so every scope call it makes is a 401")
}

func TestRedactToken_KeepsTheTokenOutOfLogs(t *testing.T) {
	// The docker alias names the token without a value, but an assignment that
	// did reach a logged command line must not survive into it. Debug logs are
	// the first thing a user pastes into a bug report.
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
			name: "the token named without a value is untouched",
			cmd:  "docker run -e " + token.Env + " --rm img",
			want: "docker run -e " + token.Env + " --rm img",
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
