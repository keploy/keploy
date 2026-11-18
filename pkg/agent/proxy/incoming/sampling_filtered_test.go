package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// probeRequest is a kubelet's HTTP probe as the kubelet sends it: a GET that
// asks for its connection to be closed after it.
const probeRequest = "GET /health HTTP/1.1\r\nHost: app\r\nUser-Agent: kube-probe/1.31\r\nAccept: */*\r\nConnection: close\r\n\r\n"

// leaveOutProbes is the recording's options with k8s-proxy's built-in ingress
// filter, which leaves the health checkers' probes out of the recording
// unless probe traffic is asked for.
func leaveOutProbes() *models.IncomingOptions {
	return &models.IncomingOptions{Filters: []models.Filter{{
		Headers:      map[string]string{"User-Agent": `^(kube-probe/|GoogleHC/|ELB-HealthChecker/|Amazon-Route53-Health-Check-Service)`},
		FilterPolicy: models.Exclude,
	}}}
}

// recordingPM is an ingress that records under the recording options opts,
// with sampling slots (--enable-sampling=sampling; 0 is no sampling), and the
// paths of the requests it hands to its capture, in the order handed.
func recordingPM(t *testing.T, sampling int, opts *models.IncomingOptions) (*IngressProxyManager, func() []string) {
	t.Helper()
	stubIngressPaused(t, func() bool { return false })
	var (
		mu       sync.Mutex
		captured []string
	)
	stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
		req *http.Request, _ *http.Response, _, _ time.Time,
		_ models.IncomingOptions, _ bool, _ bool, _ uint16) {
		mu.Lock()
		captured = append(captured, req.URL.Path)
		mu.Unlock()
	})
	cfg := &config.Config{}
	cfg.Agent.RecordRequests = true
	cfg.Agent.EnableSampling = sampling
	pm := New(zap.NewNop(), nil, cfg)
	pm.incomingOpts.Store(opts)
	return pm, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), captured...)
	}
}

// A request the recording's filters leave out takes no part of the sampling
// budget past its head: with one sampling slot, a request that comes on
// another connection while the app answers it is captured, and its own
// answer still closes its connection, so that its client's next request
// comes on a new connection, where it can be sampled. Held through the app's
// answer, the slot was full as that request came, and it was passed through
// uncaptured: missing from the recording and from the static-dedup counts,
// though what held the slot was never to be recorded.
func TestARequestTheFiltersLeaveOutGivesItsSamplingSlotBackAtItsHead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    *models.IncomingOptions
		request string
	}{
		{"a kubelet's probe", leaveOutProbes(), probeRequest},
		{"a GKE load balancer's health check", leaveOutProbes(),
			"GET /health HTTP/1.1\r\nHost: app\r\nUser-Agent: GoogleHC/1.0\r\n\r\n"},
		{"an AWS load balancer's health check", leaveOutProbes(),
			"GET /health HTTP/1.1\r\nHost: app\r\nUser-Agent: ELB-HealthChecker/2.0\r\n\r\n"},
		{"a request a user's filter leaves out", &models.IncomingOptions{Filters: []models.Filter{{
			BypassRule:   models.BypassRule{Path: "^/internal"},
			FilterPolicy: models.Exclude,
		}}}, "GET /internal HTTP/1.1\r\nHost: app\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			heldIn := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			letGo := func() { releaseOnce.Do(func() { close(release) }) }
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/order" {
					close(heldIn)
					<-release
				}
				w.Header().Set("Content-Length", "2")
				_, _ = w.Write([]byte("ok"))
			}))
			t.Cleanup(app.Close)
			appAddr := app.Listener.Addr().String()

			pm, captured := recordingPM(t, 1, tc.opts)
			serve := servedConns(t, pm, letGo)

			held, heldServed := serve.open(t, appAddr, tc.name, tc.request)
			select {
			case <-heldIn:
			case <-time.After(5 * time.Second):
				t.Fatal("the request the filters leave out never reached the app")
			}
			orderServed := serve.exchange(t, appAddr, "/order", `{"sku":"a","qty":1}`)
			<-orderServed
			letGo()
			resp := answerOf(t, tc.name, held)
			<-heldServed
			pm.captures.Wait()

			if got := captured(); !reflect.DeepEqual(got, []string{"/order"}) {
				t.Fatalf("with one sampling slot, a request sent while the app answered %s had %v handed to the capture, want [/order]: what the filters leave out held the slot, and the request was passed through uncaptured", tc.name, got)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s was answered %d, want 200", tc.name, resp.StatusCode)
			}
			if !resp.Close {
				t.Fatalf("the answer to %s on a sampled connection kept the connection open (Connection: %q): its client's next requests on it would pass uncaptured", tc.name, resp.Header.Get("Connection"))
			}
		})
	}
}

// A sampled request is read at its head as its capture reads it, so a
// request the capture's filters keep is not taken out of the sample there.
// The capture is handed the request rebuilt from its dump, with a Host
// header and the forced close's "Connection: close"; read off the wire, it
// has no Host header and the client's Connection, and a filter on either
// would leave out at the head what the capture keeps: with
// --enable-sampling, such a recording would record nothing.
func TestARequestItsCaptureKeepsIsNotLeftOutAtItsHead(t *testing.T) {
	const order = "POST /order HTTP/1.1\r\nHost: app\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	const orderKeepAlive = "POST /order HTTP/1.1\r\nHost: app\r\nConnection: keep-alive\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	for _, tc := range []struct {
		name    string
		filter  models.Filter
		request string
	}{
		{"an include filter on Host",
			models.Filter{Headers: map[string]string{"Host": "^app$"}, FilterPolicy: models.Include}, order},
		{"an include filter on Host keyed in lower case",
			models.Filter{Headers: map[string]string{"host": "^app$"}, FilterPolicy: models.Include}, order},
		{"an exclude filter on the client's keep-alive",
			models.Filter{Headers: map[string]string{"Connection": "keep-alive"}, FilterPolicy: models.Exclude}, orderKeepAlive},
		{"an include filter on the forced close",
			models.Filter{Headers: map[string]string{"Connection": "^close$"}, FilterPolicy: models.Include}, order},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "2")
				_, _ = w.Write([]byte("ok"))
			}))
			t.Cleanup(app.Close)
			appAddr := app.Listener.Addr().String()

			pm, _ := recordingPM(t, 1, &models.IncomingOptions{Filters: []models.Filter{tc.filter}})
			// What the capture keeps: CaptureHook leaves out of the
			// recording what the filters leave out of the request it is
			// handed.
			var (
				mu   sync.Mutex
				kept []string
			)
			stubCaptureHook(t, func(_ context.Context, _ *zap.Logger, _ chan *models.TestCase,
				req *http.Request, _ *http.Response, _, _ time.Time,
				opts models.IncomingOptions, _ bool, _ bool, _ uint16) {
				if !hooksUtils.IsFiltered(zap.NewNop(), req, opts) {
					mu.Lock()
					kept = append(kept, req.URL.Path)
					mu.Unlock()
				}
			})
			serve := servedConns(t, pm, func() {})

			client, served := serve.open(t, appAddr, tc.name, tc.request)
			_ = answerOf(t, tc.name, client)
			<-served
			pm.captures.Wait()

			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(kept, []string{"/order"}) {
				t.Fatalf("with %s and one sampling slot, the capture kept %v, want [/order]: the head read the request unlike its capture and left out what the capture keeps", tc.name, kept)
			}
		})
	}
}

// The head's filter check on a sampled connection reads a copy of the
// request: the request itself gets the forced close's "Connection: close"
// only once no demote is left, so an exchange the recording's pause demotes
// to bypass is forwarded to the app as its client sent it, and its
// connection stays open.
func TestAPausedSampledExchangeIsForwardedAsItsClientSentIt(t *testing.T) {
	var (
		mu       sync.Mutex
		appSawIt []string
	)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		appSawIt = append(appSawIt, r.Header.Get("Connection"))
		mu.Unlock()
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)
	appAddr := app.Listener.Addr().String()

	pm, captured := recordingPM(t, 1, &models.IncomingOptions{})
	stubIngressPaused(t, func() bool { return true })
	serve := servedConns(t, pm, func() {})

	client, served := serve.open(t, appAddr, "/order", "GET /order HTTP/1.1\r\nHost: app\r\n\r\n")
	resp := answerOf(t, "/order", client)
	<-served
	pm.captures.Wait()

	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(appSawIt, []string{""}) {
		t.Fatalf("under a pause, the app was sent a sampled connection's request with Connection %q, want it as the client sent it (none)", appSawIt)
	}
	if resp.Close {
		t.Fatalf("under a pause, the answer closed the sampled connection (Connection: %q): an exchange demoted to bypass is passed through unchanged", resp.Header.Get("Connection"))
	}
	if got := captured(); len(got) != 0 {
		t.Fatalf("under a pause, %v was handed to the capture", got)
	}
}

// A probe the recording's filters keep (probe traffic asked for, or no filter
// that leaves it out) is captured like any request: it is sampled.
func TestAProbeTheFiltersKeepIsSampled(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)
	appAddr := app.Listener.Addr().String()

	pm, captured := recordingPM(t, 1, &models.IncomingOptions{})
	serve := servedConns(t, pm, func() {})

	probe, served := serve.open(t, appAddr, "the probe", probeRequest)
	_ = answerOf(t, "the probe", probe)
	<-served
	pm.captures.Wait()

	if got := captured(); !reflect.DeepEqual(got, []string{"/health"}) {
		t.Fatalf("a probe no filter leaves out had %v handed to the capture, want [/health]: a probe that is to be recorded was passed through", got)
	}
}

// Without sampling, a request the filters leave out costs nothing to the
// requests after it on its keep-alive connection: the next one is handed to
// the capture. Only a sampled connection's exchange is taken out of the
// sample at its head; taken out of the capture for the connection's life,
// every later request of a client that keeps its connection alive would go
// unrecorded.
func TestWithoutSamplingAFilteredRequestLeavesItsConnectionCaptured(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(app.Close)
	appAddr := app.Listener.Addr().String()

	pm, captured := recordingPM(t, 0, &models.IncomingOptions{Filters: []models.Filter{{
		BypassRule:   models.BypassRule{Path: "^/internal"},
		FilterPolicy: models.Exclude,
	}}})
	serve := servedConns(t, pm, func() {})

	client, served := serve.open(t, appAddr, "/internal", "GET /internal HTTP/1.1\r\nHost: app\r\n\r\n")
	reader := bufio.NewReader(client)
	for i, next := range []string{"GET /order HTTP/1.1\r\nHost: app\r\n\r\n", ""} {
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("answer %d: the client had no answer: %v", i+1, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.Close {
			t.Fatalf("answer %d closed the keep-alive connection without sampling", i+1)
		}
		if next != "" {
			if _, err := client.Write([]byte(next)); err != nil {
				t.Fatalf("the client could not send its next request: %v", err)
			}
		}
	}
	_ = client.Close()
	<-served
	pm.captures.Wait()

	if got := captured(); !slices.Contains(got, "/order") {
		t.Fatalf("without sampling, the request after one the filters leave out on its keep-alive connection was not handed to the capture (handed: %v)", got)
	}
}
