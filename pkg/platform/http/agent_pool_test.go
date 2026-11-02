package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Another client in keploy's process reaches the same agent on the default
// pool: enterprise's own client to the agent does, for its time-freeze window
// calls, which the agent answers with no body. Its cancel at the instant the
// transport has put such a request's connection back in the pool closes that
// connection under the request that took it next (golang/go#82547). The
// AgentClient's connections are a pool of its own (agentPool), so that request
// is never the AgentClient's: on the default pool, its read of what a run did
// failed with the other client's cancel, on a context nothing cancelled
// ("failed to get scope windows ...: context canceled"). The trace holds the
// other client's transport at that instant, the AgentClient's read takes a
// connection then, and the other client's request is cancelled, on every run.
// It runs on a client with no token and on one with the session's token
// (agentHTTPClient), and on New's, which is the latter: New takes the token
// token.Session mints when the process was handed none.
func TestAnotherClientsCancelLeavesTheAgentClientsReadAlone(t *testing.T) {
	for _, row := range []struct {
		name   string
		client func() http.Client // nil: New's own
	}{
		{"a client with no token", func() http.Client { return agentHTTPClient("") }},
		{"a client with the session's token", func() http.Client { return agentHTTPClient("session-token") }},
		{"New's client", nil},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/scope/windows" {
					_, _ = w.Write([]byte("[]"))
					return
				}
				w.WriteHeader(http.StatusNoContent) // the other client's answer: no body
			}))
			defer srv.Close()
			cfg := &config.Config{Agent: config.Agent{AgentURI: srv.URL}}
			a := New(zap.NewNop(), nil, cfg)
			if row.client != nil {
				a.client = row.client()
			}
			// The other client: on http.DefaultTransport, as enterprise's
			// agentBearerTransport with its nil base is.
			other := &http.Client{}

			otherCtx, cancelOther := context.WithCancel(context.Background())
			defer cancelOther()
			var pooled atomic.Bool
			read := make(chan error, 1)
			trace := &httptrace.ClientTrace{PutIdleConn: func(err error) {
				if err != nil || !pooled.CompareAndSwap(false, true) {
					return
				}
				tookConn := make(chan struct{})
				var took sync.Once
				readCtx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
					GotConn: func(httptrace.GotConnInfo) { took.Do(func() { close(tookConn) }) },
				})
				ended := make(chan error, 1)
				go func() {
					_, err := a.GetScopeWindows(readCtx)
					ended <- err
				}()
				<-tookConn
				cancelOther()
				// On the shared pool the read has no answer while the
				// transport is held here: it ends only when the cancel closes
				// the connection it took.
				read <- <-ended
			}}
			req, err := http.NewRequestWithContext(httptrace.WithClientTrace(otherCtx, trace), http.MethodPost, srv.URL+"/time-freeze/window", nil)
			if err != nil {
				t.Fatal(err)
			}
			if res, err := other.Do(req); err == nil {
				_ = res.Body.Close()
			}
			if !pooled.Load() {
				t.Fatal("precondition: the other client's connection was never put back in the pool")
			}
			if err := <-read; err != nil {
				t.Fatalf("the AgentClient's read failed with another client's cancel, on a context nothing cancelled: %v", err)
			}
		})
	}
}

// schemeSeq names each scheme a row registers: http.DefaultTransport keeps a
// registration for the life of the process and refuses a second one of the
// same name.
var schemeSeq atomic.Int64

// registerScheme registers a scheme of its own on http.DefaultTransport, as a
// program that embeds keploy's client does for its agent URIs (k8s-proxy's
// keploy-agent://), and returns its name. Its round tripper does what
// k8s-proxy's does: it puts the embedder's token on the request, in place of
// any Authorization the client set, and sends it on as http, on a transport
// of the embedder's.
func registerScheme(t *testing.T, token string) string {
	t.Helper()
	scheme := fmt.Sprintf("keploy-test-agent-%d", schemeSeq.Add(1))
	embedders := &http.Transport{}
	t.Cleanup(embedders.CloseIdleConnections)
	http.DefaultTransport.(*http.Transport).RegisterProtocol(scheme, roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		out := req.Clone(req.Context())
		u := *req.URL
		u.Scheme = "http"
		out.URL = &u
		out.Header.Set("Authorization", "Bearer "+token)
		return embedders.RoundTrip(out)
	}))
	return scheme
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// clientsOfTheAgentClient are the clients an AgentClient sends on: New's,
// with no token and with the session's token (agentHTTPClient), and a client
// on the default transport, as one built before the client had a pool of its
// own was.
var clientsOfTheAgentClient = map[string]func() http.Client{
	"a client with no token":            func() http.Client { return agentHTTPClient("") },
	"a client with the session's token": func() http.Client { return agentHTTPClient("session-token") },
	"a client on the default transport": func() http.Client { return http.Client{Transport: &instanceTransport{}} },
}

// requestsOnBothPaths are a request on the client's pool and one on a
// connection of its own (ownConnection).
var requestsOnBothPaths = map[string]func(ctx context.Context, a *AgentClient) error{
	"on the client's pool": func(ctx context.Context, a *AgentClient) error {
		_, err := a.GetScopeWindows(ctx)
		return err
	},
	"on a connection of its own": func(ctx context.Context, a *AgentClient) error {
		return a.EndRecordingStreams(ctx, "rec-1")
	},
}

// A program that embeds keploy's client and reaches its agent at a URI of a
// scheme it registered on http.DefaultTransport has every request in that
// scheme go to the round tripper it registered, on the client's pool and on a
// connection of its own, whatever token the client carries (ownPool). That
// round tripper's token is what reaches the agent, and the answer is noted for
// the agent process that gave it (agent_instance.go). On a clone of the default
// transport, which carries no registration, every one of these requests failed
// before it left the process: k8s-proxy's sidecar recording ended with
// unsupported protocol scheme "keploy-agent".
func TestASchemeTheEmbedderRegisteredReachesItsRoundTripper(t *testing.T) {
	for client, newClient := range clientsOfTheAgentClient {
		for path, ask := range requestsOnBothPaths {
			t.Run(client+", "+path, func(t *testing.T) {
				scheme := registerScheme(t, "pod-token")
				// The note is kept per address for the life of the process,
				// and a later server can get an earlier one's port: an
				// instance of this subtest's own cannot be an earlier note.
				instance := "proc-of-" + scheme
				var auth atomic.Value
				auth.Store("")
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					auth.Store(r.Header.Get("Authorization"))
					w.Header().Set(models.AgentInstanceHeader, instance)
					_, _ = w.Write([]byte("[]"))
				}))
				defer srv.Close()
				host := strings.TrimPrefix(srv.URL, "http://")
				t.Cleanup(func() { instances.Delete(host) })
				a := &AgentClient{logger: zap.NewNop(), client: newClient(), conf: &config.Config{Agent: config.Agent{AgentURI: scheme + "://" + host}}}
				if err := ask(context.Background(), a); err != nil {
					t.Fatalf("the request in the embedder's scheme failed: %v", err)
				}
				if got := auth.Load().(string); got != "Bearer pod-token" {
					t.Fatalf("the agent saw Authorization %q, want the embedder's round tripper's token", got)
				}
				s := instancesAt(host)
				s.mu.Lock()
				latest := s.latest
				s.mu.Unlock()
				if latest != instance {
					t.Fatalf("the agent process noted last is %q, want %q: this answer was not noted", latest, instance)
				}
			})
		}
	}
}

// A scheme nothing registered fails as it did on the clone, on both paths and
// on every client, before the request leaves the process.
func TestASchemeNothingRegisteredFailsAsBefore(t *testing.T) {
	for client, newClient := range clientsOfTheAgentClient {
		for path, ask := range requestsOnBothPaths {
			t.Run(client+", "+path, func(t *testing.T) {
				a := &AgentClient{logger: zap.NewNop(), client: newClient(), conf: &config.Config{Agent: config.Agent{AgentURI: "keploy-test-unregistered://127.0.0.1:1"}}}
				err := ask(context.Background(), a)
				if err == nil || !strings.Contains(err.Error(), `unsupported protocol scheme "keploy-test-unregistered"`) {
					t.Fatalf("got %v, want unsupported protocol scheme \"keploy-test-unregistered\"", err)
				}
			})
		}
	}
}

// A request on http or https, and a request with no URL, goes on the pool of
// keploy's (ownPool), never to the round tripper a registered scheme goes to:
// that is the default transport, whose connections are shared with every
// other client on it (agentPool). The stand-in for it here fails every
// request it gets with an error of its own, so a request it served cannot
// pass as one the pool served.
func TestHTTPAndHTTPSStayOnThePool(t *testing.T) {
	onRegistered := errors.New("served by the round tripper a registered scheme goes to")
	registered := roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, onRegistered })
	answer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	plain := httptest.NewServer(answer)
	defer plain.Close()
	tls := httptest.NewTLSServer(answer)
	defer tls.Close()
	for _, row := range []struct {
		name string
		url  string
		pool *http.Transport
	}{
		{"http", plain.URL, &http.Transport{}},
		{"https", tls.URL, tls.Client().Transport.(*http.Transport).Clone()},
	} {
		t.Run(row.name, func(t *testing.T) {
			defer row.pool.CloseIdleConnections()
			rt := &ownPool{pool: row.pool, registered: registered}
			req, err := http.NewRequest(http.MethodGet, row.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			res, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatalf("a request on %s left the pool: %v", row.name, err)
			}
			_ = res.Body.Close()
		})
	}
	t.Run("no URL", func(t *testing.T) {
		_, err := (&ownPool{pool: &http.Transport{}, registered: registered}).RoundTrip(&http.Request{Header: http.Header{}})
		if errors.Is(err, onRegistered) {
			t.Fatalf("a request with no URL left the pool: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "nil Request.URL") {
			t.Fatalf("got %v, want the pool's nil Request.URL error", err)
		}
	})
}

// A connection of its own (ownConnection) is made on a copy of the client's
// pool, for its one request: the copy keeps no connection, and the client's
// pool, and the default transport a registered scheme goes to, keep theirs
// for every request after it. Keep-alives turned off on the pool itself
// would be gone for every later request on the client, and that write would
// race with the requests on the pool.
func TestAnOwnConnectionLeavesThePoolItsConnections(t *testing.T) {
	for client, newClient := range clientsOfTheAgentClient {
		t.Run(client, func(t *testing.T) {
			a := &AgentClient{logger: zap.NewNop(), client: newClient()}
			own := a.ownConnection()
			if !poolOf(t, own.Transport).DisableKeepAlives {
				t.Fatal("the connection of its own is on a pool that keeps connections")
			}
			if poolOf(t, a.client.Transport).DisableKeepAlives {
				t.Fatal("the client's pool keeps no connection after a connection of its own was made")
			}
			if http.DefaultTransport.(*http.Transport).DisableKeepAlives {
				t.Fatal("the default transport keeps no connection after a connection of its own was made")
			}
		})
	}
}

// poolOf is the transport an AgentClient's http request goes on, under its
// instance note and token: the pool of an ownPool, or the default transport
// when there is none.
func poolOf(t *testing.T, rt http.RoundTripper) *http.Transport {
	t.Helper()
	for {
		switch r := rt.(type) {
		case nil:
			rt = http.DefaultTransport
		case *instanceTransport:
			rt = r.base
		case *bearerTransport:
			rt = r.base
		case *ownPool:
			return r.pool
		case *http.Transport:
			return r
		default:
			t.Fatalf("%T is no transport of the agent client's", rt)
			return nil
		}
	}
}
