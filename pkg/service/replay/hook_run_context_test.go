package replay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	httpclient "go.keploy.io/server/v3/pkg/platform/http"
	"go.uber.org/zap"
)

// hookCtxSpy is keploy's agent client, noting the context a test case's last
// hook was called on.
type hookCtxSpy struct {
	*httpclient.AgentClient
	mu  sync.Mutex
	ctx context.Context
}

func (s *hookCtxSpy) noted() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

func (s *hookCtxSpy) note(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
}

func (s *hookCtxSpy) BeforeSimulate(ctx context.Context, at *time.Time, testSetID, tcName string) error {
	s.note(ctx)
	return s.AgentClient.BeforeSimulate(ctx, at, testSetID, tcName)
}

func (s *hookCtxSpy) AfterSimulate(ctx context.Context, tcName, testSetID string) error {
	s.note(ctx)
	return s.AgentClient.AfterSimulate(ctx, tcName, testSetID)
}

// hookKinds are the kinds of test SimulateRequest calls the agent's hooks
// for, each in a test of its own against an app that keeps none of its
// connections (only the agent's go back in the pool, for the trace in
// TestASetsCancelLeavesTheNextReadOfTheAgentAlone).
var hookKinds = []struct {
	name string
	// test is the test case, sent to the app at the base path base.
	test func(t *testing.T, base string) *models.TestCase
}{
	{"an HTTP test", func(t *testing.T, base string) *models.TestCase {
		return atBasePath(t, base, bpTestCase("orders", 1))
	}},
	{"an HTTP streaming test", func(t *testing.T, base string) *models.TestCase {
		test := bpRequest("stream", 1, "/stream")
		test.HTTPResp.Header = map[string]string{"Content-Type": "application/x-ndjson"}
		test.HTTPResp.Body = ""
		for _, line := range heldOpenLines {
			test.HTTPResp.StreamBody = append(test.HTTPResp.StreamBody, models.HTTPStreamChunk{
				Data: []models.HTTPStreamDataField{{Key: "raw", Value: line}},
			})
		}
		if !pkg.IsHTTPStreamingTestCase(test) {
			t.Fatal("precondition: the test is not sent as a stream")
		}
		return atBasePath(t, base, test)
	}},
	{"a gRPC test", func(t *testing.T, _ string) *models.TestCase {
		test := newBPGRPCApp(t).testCase(t, "grpc-1")
		// As recorded: the hook before a test with no time is not called.
		test.GrpcReq.Timestamp = time.Now()
		return test
	}},
}

// atBasePath is test sent to the app at the base path base.
func atBasePath(t *testing.T, base string, test *models.TestCase) *models.TestCase {
	t.Helper()
	rewritten, err := ReplaceBaseURL(base, test.HTTPReq.URL)
	if err != nil {
		t.Fatal(err)
	}
	test.HTTPReq.URL = rewritten
	return test
}

// A test set's context is cancelled at any instant: when its app exits, or a
// test loop fails. A test case's hooks are keploy's own calls to its agent,
// answered with no body on the client's shared connections, and the set's
// cancel must not reach them (agentHookContext): one that found a hook still
// waiting for its answer after the client had put its connection back in the
// pool closed that connection under the next request, which had taken it, and
// keploy's next read of the agent failed with the hook's cancel ("failed to
// get scope windows ...: context canceled"). The trace holds the transport at
// the instant the hook's connection is back in the pool; the read takes it,
// and the set is cancelled then, on every run. Each kind of test calls its
// hooks at call sites of its own, so each is a row: an HTTP test, an HTTP
// streaming one (whose hook before is the HTTP test's) and a gRPC one.
func TestASetsCancelLeavesTheNextReadOfTheAgentAlone(t *testing.T) {
	for _, kind := range hookKinds {
		for _, tc := range []struct {
			name string
			hook int32 // the test case's call to the agent the cancel comes at
		}{
			{"as the hook before the test is answered", 1},
			{"as the hook after the test is answered", 2},
		} {
			t.Run(kind.name+", "+tc.name, func(t *testing.T) {
				agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/scope/windows" {
						_, _ = w.Write([]byte("[]"))
						return
					}
					w.WriteHeader(http.StatusOK) // a hook's answer: no body
				}))
				defer agent.Close()
				app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// Not kept: only the agent's connections go back in the
					// pool, for the trace below.
					w.Header().Set("Connection", "close")
					if r.URL.Path == "/stream" {
						w.Header().Set("Content-Type", "application/x-ndjson")
						for _, line := range heldOpenLines {
							_, _ = w.Write([]byte(line + "\n"))
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(bpBody))
				}))
				defer app.Close()
				cfg := &config.Config{}
				cfg.Agent.AgentURI = agent.URL
				cfg.Test.BasePath = app.URL
				cfg.Test.APITimeout = 5
				spy := &hookCtxSpy{AgentClient: httpclient.New(zap.NewNop(), nil, cfg)}
				test := kind.test(t, app.URL)

				var cancelSet context.CancelFunc
				var pooled atomic.Int32
				read := make(chan error, 1)
				trace := &httptrace.ClientTrace{PutIdleConn: func(err error) {
					if err != nil || pooled.Add(1) != tc.hook {
						return
					}
					tookConn := make(chan struct{})
					var took sync.Once
					readCtx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
						GotConn: func(httptrace.GotConnInfo) { took.Do(func() { close(tookConn) }) },
					})
					ended := make(chan error, 1)
					go func() {
						_, err := spy.GetScopeWindows(readCtx)
						ended <- err
					}()
					<-tookConn
					cancelSet()
					if spy.noted().Err() != nil {
						// The hook's request has the cancel before its
						// answer, which this trace holds: the read ends
						// only when the cancel closes the connection it
						// took.
						read <- <-ended
						return
					}
					// The read is answered once the hook's answer is
					// handed on.
					go func() { read <- <-ended }()
				}}
				runCtx := httptrace.WithClientTrace(context.Background(), trace)
				setCtx, cancel := context.WithCancel(runCtx)
				cancelSet = cancel
				defer cancelSet()
				_, _ = NewHooks(zap.NewNop(), cfg, spy).SimulateRequest(withRunLife(setCtx, runCtx), test, "test-set-0")
				if pooled.Load() < tc.hook {
					t.Fatalf("precondition: the agent's connection was put back in the pool %d times, want %d", pooled.Load(), tc.hook)
				}
				if err := <-read; err != nil {
					t.Fatalf("keploy's next read of the agent failed with the set's cancel, on a context nothing cancelled: %v", err)
				}
			})
		}
	}
}

// hooksWhenTheAppExits is prHooks with its test in flight when the set's app
// exits on its own: it waits for the app watcher to cancel the set's context,
// and says whether the context the agent's hooks are called on
// (agentHookContext) was cancelled with it.
type hooksWhenTheAppExits struct {
	prHooks
	app       *prInstr
	hookEnded chan bool
}

func (f hooksWhenTheAppExits) SimulateRequest(ctx context.Context, _ *models.TestCase, _ string) (interface{}, error) {
	close(f.app.appStopped)
	<-ctx.Done()
	f.hookEnded <- agentHookContext(ctx).Err() != nil
	return nil, context.Cause(ctx)
}

// RunTestSet hands a test case the set's context marked with the run's
// (withRunLife): the app's exit, which cancels the set's, leaves a test case's
// hooks to the agent running.
func TestRunTestSet_AnAppsExitLeavesATestsHooksToTheAgentRunning(t *testing.T) {
	h := newPartialRunHarness(t, 1, 0)
	hookEnded := make(chan bool, 1)
	h.replayer.hookImpl = hooksWhenTheAppExits{app: h.instr, hookEnded: hookEnded}
	h.run(t)
	select {
	case ended := <-hookEnded:
		if ended {
			t.Fatal("the app's exit cancelled the context of the test's hooks to the agent: they are on the set's context, not the run's")
		}
	default:
		t.Fatal("precondition: no test was in flight when the app exited")
	}
}
