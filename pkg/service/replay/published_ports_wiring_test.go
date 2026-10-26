package replay

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// publishedApp is the app as the host reaches it in Docker mode when its port
// (the one its tests were recorded on, where nothing on the host listens) is
// published on one other host port: an HTTP server on that host port that
// counts the connections it gets.
type publishedApp struct {
	appPort  uint16 // recorded; nothing listens on the host
	hostPort uint16
	conns    atomic.Int32
}

// reachedSince waits for the app to accept a connection beyond the first
// before of them: a gate's connect returns once the kernel has completed the
// handshake, before the app's server has accepted it.
func (a *publishedApp) reachedSince(t *testing.T, before int32, what string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); a.conns.Load() <= before; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached host port %d, where the app's port is published", what, a.hostPort)
		}
	}
}

func newPublishedApp(t *testing.T) *publishedApp {
	t.Helper()
	a := &publishedApp{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			a.conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	a.hostPort = portOfAddr(t, srv.Listener.Addr().String())
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.appPort = portOfAddr(t, ln.Addr().String())
	_ = ln.Close() // the recorded port: nothing answers there
	return a
}

func portOfAddr(t *testing.T, addr string) uint16 {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	return uint16(n)
}

// instrumentation that knows the app's port is published on a.hostPort.
func (a *publishedApp) instrumentation() *publishingInstrumentation {
	return publishing(map[uint16][2]uint16{a.appPort: {a.hostPort, 1}})
}

func (a *publishedApp) httpTest() *models.TestCase {
	url := "http://localhost:" + strconv.Itoa(int(a.appPort)) + "/x"
	return &models.TestCase{Name: "test-1", Kind: models.HTTP, AppPort: a.appPort, HTTPReq: models.HTTPReq{Method: "GET", URL: url}}
}

func replayConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Test.Host = "localhost"
	return cfg
}

// The readiness gate before a set's first test probes where its tests go: with
// the proven switch off (the --delay floor, then the app's address) and on
// (the first test's address taking a connect, without an agent to ask), an
// app port published on one other host port is probed there, as the tests
// will be sent there, not at the recorded port nothing answers.
func TestTheReadinessGateProbesWhereTheTestsGo(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(map[bool]string{false: "switch off", true: "switch on"}[proven], func(t *testing.T) {
			if proven {
				t.Setenv(provenswitch.Env, "1")
			}
			app := newPublishedApp(t)
			r := &Replayer{logger: zap.NewNop(), config: replayConfig(), instrumentation: app.instrumentation()}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ready, why := r.appReady(ctx, []*models.TestCase{app.httpTest()}, "test-set-0", true, false)
			if !ready {
				t.Fatalf("the gate did not find the app ready (%q): it probed the recorded port %d, not host port %d where it is published", why, app.appPort, app.hostPort)
			}
			app.reachedSince(t, 0, "the gate")
		})
	}
}

// The re-gate before a reset test is re-sent probes where the test goes.
func TestTheResetReSendGateProbesWhereTheTestGoes(t *testing.T) {
	app := newPublishedApp(t)
	r := &Replayer{logger: zap.NewNop(), config: replayConfig(), instrumentation: app.instrumentation()}
	r.waitForResetResendReady(context.Background(), app.httpTest(), "test-set-0")
	app.reachedSince(t, 0, "the re-send gate")
}

// simulatingInstrumentation is a publishing one that lets a test be sent.
type simulatingInstrumentation struct {
	*publishingInstrumentation
}

func (simulatingInstrumentation) BeforeSimulate(context.Context, *time.Time, string, string) error {
	return nil
}
func (simulatingInstrumentation) AfterSimulate(context.Context, string, string) error { return nil }

// The hooks send an HTTP test and a gRPC test of an app port published on one
// other host port there.
func TestTheHooksSendHTTPAndGRPCTestsWhereTheAppsPortIsPublished(t *testing.T) {
	app := newPublishedApp(t)
	inst := simulatingInstrumentation{app.instrumentation()}
	h := NewHooks(zap.NewNop(), replayConfig(), inst).(*Hooks)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := h.SimulateRequest(ctx, app.httpTest(), "test-set-0"); err != nil {
		t.Fatalf("the HTTP test: %v", err)
	}
	app.reachedSince(t, 0, "the HTTP test")

	before := app.conns.Load()
	authority := "localhost:" + strconv.Itoa(int(app.appPort))
	grpcTest := &models.TestCase{Name: "test-2", Kind: models.GRPC_EXPORT, AppPort: app.appPort, GrpcReq: models.GrpcReq{
		Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": authority, ":path": "/svc/Method", ":method": "POST", ":scheme": "http"}},
	}}
	// The app answers HTTP/1 only, so the call itself fails; where it
	// connected is what is asked.
	_, _ = h.SimulateRequest(ctx, grpcTest, "test-set-0")
	app.reachedSince(t, before, "the gRPC test")
}

// Under the proven switch the first test's address is where the readiness
// wait dials; for a gRPC test it is where the test is sent: its app port over
// the authority's, test.grpcPort over that, and the one host port its app
// port is published on.
func TestTheFirstGRPCDialIsWhereTheTestGoes(t *testing.T) {
	grpcTest := func(authority string, appPort uint16) *models.TestCase {
		return &models.TestCase{Kind: models.GRPC_EXPORT, AppPort: appPort, GrpcReq: models.GrpcReq{
			Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": authority}}}}
	}
	published := func(_ context.Context, _ string, appPort uint16) uint16 {
		if appPort == 50051 {
			return 15051
		}
		return 0
	}
	for _, tc := range []struct {
		name string
		cfg  config.Test
		tc   *models.TestCase
		pub  func(context.Context, string, uint16) uint16
		want string
	}{
		{"its app port published elsewhere", config.Test{}, grpcTest("api:50051", 50051), published, "localhost:15051"},
		{"its app port over the authority's", config.Test{}, grpcTest("api:9000", 50051), nil, "localhost:50051"},
		{"test.grpcPort", config.Test{GRPCPort: 7000}, grpcTest("api:50051", 50051), published, "localhost:7000"},
		{"test.host", config.Test{Host: "127.0.0.1"}, grpcTest("api:50051", 50051), nil, "127.0.0.1:50051"},
		// A replaceWith that matches the recorded authority and names the
		// port is the user's choice, asked of the recorded authority.
		{"a replaceWith naming the port", withReplace(map[string]string{"api:50051": "localhost:50051"}), grpcTest("api:50051", 50051), published, "localhost:50051"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, h, p, ok := firstDialTarget(context.Background(), tc.cfg, []*models.TestCase{tc.tc}, "test-set-0", zap.NewNop(), tc.pub)
			if !ok || net.JoinHostPort(h, p) != tc.want {
				t.Fatalf("first dial %s:%s (%v), want %s", h, p, ok, tc.want)
			}
		})
	}
}

// publishingPrInstr is the RunTestSet harness's instrumentation, saying the
// app's port is published on whatever host port hostPort holds now, as an
// agent container started again for a set's next attempt may.
type publishingPrInstr struct {
	*prInstr
	hostPort atomic.Uint32
}

func (p *publishingPrInstr) PublishedHostPort(context.Context, string, uint16) (uint16, bool) {
	return uint16(p.hostPort.Load()), true
}

// A run of a set asks where the app's port is published afresh: a second run
// of the same set (an attempt, after the agent container started again) whose
// app port is now published on another host port sends its test there, not
// where the first run's was.
func TestEachRunOfASetSendsItsTestsWhereTheAppsPortIsPublishedNow(t *testing.T) {
	routes := func(a *tmplApp, mux *http.ServeMux) {
		mux.HandleFunc("/x", func(w http.ResponseWriter, r *http.Request) { a.record(r); writeJSON(w, 200, `{"ok":true}`) })
	}
	first, second := newTmplApp(t, routes), newTmplApp(t, routes)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	appPort := portOfAddr(t, ln.Addr().String())
	_ = ln.Close() // the recorded port: nothing answers there
	tc := tmplCase(1, "127.0.0.1:"+strconv.Itoa(int(appPort)), "GET", "/x", nil, "", recorded(200, nil, `{"ok":true}`, nil))
	tc.AppPort = appPort

	h := newTmplHarness(t, &tmplConf{}, []*models.TestCase{tc}, nil)
	instr := &publishingPrInstr{prInstr: &prInstr{appStopped: make(chan struct{})}}
	h.r.instrumentation = instr
	h.r.hookImpl = newHooks(prLogger(), h.r.config, instr, false)

	instr.hostPort.Store(uint32(portOfAddr(t, first.addr)))
	h.run(t)
	if n := len(first.sent("GET", "/x")); n != 1 {
		t.Fatalf("the first run sent %d tests to host port %s, where the app's port was published, want 1", n, first.addr)
	}
	instr.hostPort.Store(uint32(portOfAddr(t, second.addr)))
	h.run(t)
	if n := len(second.sent("GET", "/x")); n != 1 {
		t.Fatalf("the second run sent %d tests to host port %s, where the app's port is published now (and %d to the first run's), want 1",
			n, second.addr, len(first.sent("GET", "/x"))-1)
	}
}

func withReplace(urls map[string]string) config.Test {
	var t config.Test
	t.ReplaceWith.Global.URL = urls
	return t
}
