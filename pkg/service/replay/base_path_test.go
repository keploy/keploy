package replay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// `keploy test --base-path <url>` replays a recording against an app that is
// already running. Keploy starts neither the app nor an agent, mocks nothing,
// and sends each recorded request to the base path.
//
// No test ran that mode, and it had stopped working in layers, each hidden by
// the one before: the test set's setup spoke to the agent that was never
// started, so no test ran at all; the run that failed that way exited 0; and
// under both, every request was sent to localhost on the recorded port,
// whatever the base path said, asked for the app by the name it was recorded
// under, and had its URL unescaped on the way. These tests drive Start, as
// the command does.

// bpNoAgent is the agent client of a run that started no agent, as
// pkg/platform/http.AgentClient is before Setup gives it the agent's address:
// the /hooks/* calls and the shutdown notice return at once, and every call
// that needs the agent fails as a request with no host does. It keeps the
// names of those, and counts the run's two hooks.
type bpNoAgent struct {
	mu    sync.Mutex
	calls []string
	// begun names the tests whose request was about to be sent, in order.
	begun []string
	// afterSimulate, when set, runs once a test's request has its answer.
	afterSimulate func(test string)

	beforeTestRuns, afterTestRuns atomic.Int32
}

func (a *bpNoAgent) needsAgent(call string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, call)
	return fmt.Errorf("send request for %s: Post %q: unsupported protocol scheme \"\"", call, "/"+call)
}

func (a *bpNoAgent) made() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

func (a *bpNoAgent) Setup(context.Context, string, models.SetupOptions) error {
	return a.needsAgent("setup")
}
func (a *bpNoAgent) MockOutgoing(context.Context, models.OutgoingOptions) error {
	return a.needsAgent("mock")
}
func (a *bpNoAgent) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, a.needsAgent("consumedmocks")
}
func (a *bpNoAgent) Run(context.Context, models.RunOptions) models.AppError {
	return models.AppError{AppErrorType: models.ErrInternal, Err: a.needsAgent("run")}
}
func (a *bpNoAgent) GetErrorChannel() <-chan error { return nil }
func (a *bpNoAgent) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, a.needsAgent("mockerrors")
}
func (a *bpNoAgent) BeforeSimulate(_ context.Context, _ *time.Time, _ string, test string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.begun = append(a.begun, test)
	return nil
}
func (a *bpNoAgent) AfterSimulate(_ context.Context, test string, _ string) error {
	if a.afterSimulate != nil {
		a.afterSimulate(test)
	}
	return nil
}

// started is the tests the run went as far as sending, in order.
func (a *bpNoAgent) started() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.begun...)
}
func (a *bpNoAgent) BeforeTestRun(context.Context, string) error {
	a.beforeTestRuns.Add(1)
	return nil
}
func (a *bpNoAgent) BeforeTestSetCompose(context.Context, string, string, bool) error { return nil }
func (a *bpNoAgent) AfterTestRun(context.Context, string, []string, models.TestCoverage) error {
	a.afterTestRuns.Add(1)
	return nil
}
func (a *bpNoAgent) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return a.needsAgent("storemocks")
}
func (a *bpNoAgent) UpdateMockParams(context.Context, models.MockFilterParams) error {
	return a.needsAgent("updatemockparams")
}
func (a *bpNoAgent) GetMockStats(context.Context) (models.MockStats, error) {
	return models.MockStats{}, a.needsAgent("mock/stats")
}
func (a *bpNoAgent) GetRecentAppLogs(context.Context) string { return "" }
func (a *bpNoAgent) MakeAgentReadyForDockerCompose(context.Context) error {
	return a.needsAgent("agent/ready")
}
func (a *bpNoAgent) NotifyGracefulShutdown(context.Context) error { return nil }
func (a *bpNoAgent) ComposeDownOnSetupFailure(context.Context) error {
	return a.needsAgent("compose down")
}
func (a *bpNoAgent) BeginTestErrorCapture(context.Context) error {
	return a.needsAgent("test-capture/begin")
}
func (a *bpNoAgent) ContinueTestErrorCapture(context.Context) error {
	return a.needsAgent("test-capture/begin?carry=1")
}

// bpMockDB and bpMappingDB count the reads of a test set's mocks and of its
// mappings.
type bpMockDB struct {
	*prMockDB
	reads atomic.Int32
}

func (m *bpMockDB) GetFilteredMocks(ctx context.Context, id string, after, before time.Time, mapped, needed map[string]bool) ([]*models.Mock, error) {
	m.reads.Add(1)
	return m.prMockDB.GetFilteredMocks(ctx, id, after, before, mapped, needed)
}
func (m *bpMockDB) GetUnFilteredMocks(ctx context.Context, id string, after, before time.Time, mapped, needed map[string]bool) ([]*models.Mock, error) {
	m.reads.Add(1)
	return m.prMockDB.GetUnFilteredMocks(ctx, id, after, before, mapped, needed)
}

type bpMappingDB struct {
	*prMappingDB
	reads atomic.Int32
}

func (m *bpMappingDB) Get(ctx context.Context, id string) (map[string][]models.MockEntry, bool, error) {
	m.reads.Add(1)
	return m.prMappingDB.Get(ctx, id)
}
func (m *bpMappingDB) GetStartup(ctx context.Context, id string) ([]models.MockEntry, error) {
	m.reads.Add(1)
	return m.prMappingDB.GetStartup(ctx, id)
}

// bpReportDB can refuse the test set's report, which is how a run fails
// without a single test failing.
type bpReportDB struct {
	*prReportDB
	refuseReport bool
	// onResult, when set, runs once a test's result is written.
	onResult func(test string)
}

func (f *bpReportDB) InsertTestCaseResult(ctx context.Context, runID, setID string, res *models.TestResult) error {
	err := f.prReportDB.InsertTestCaseResult(ctx, runID, setID, res)
	if f.onResult != nil {
		f.onResult(res.TestCaseID)
	}
	return err
}

// bpTestDB counts the test cases a run deleted, which --must-pass does to the
// ones that failed.
type bpTestDB struct {
	*prTestDB
	deleted atomic.Int32
}

func (f *bpTestDB) DeleteTests(_ context.Context, _ string, ids []string) error {
	f.deleted.Add(int32(len(ids)))
	return nil
}

func (f *bpReportDB) InsertReport(ctx context.Context, runID, setID string, rep *models.TestReport) error {
	if f.refuseReport {
		return errors.New("report store is unwritable")
	}
	return f.prReportDB.InsertReport(ctx, runID, setID, rep)
}

// bpApp is the application at the base path. It answers every request with
// the body the tests were recorded with, keeping what it was asked, in order:
// the request target as it came over the wire, and the Host it was asked as.
type bpApp struct {
	*httptest.Server
	mu    sync.Mutex
	seen  []string
	hosts []string
	// wrong names the paths it answers differently from the recording.
	wrong map[string]bool
	// hold names the paths it takes a request for and never answers, and cut
	// the streamed ones whose answer it stops after the first line. held says
	// when it has got that far with one.
	hold, cut map[string]bool
	held      chan struct{}
}

const (
	bpBody         = `{"ok":true}`
	bpRecordedDate = "Wed, 07 Oct 2026 20:15:45 GMT"
)

// bpStreamLines is what bpApp streams on a path ending in "-stream".
var bpStreamLines = []string{`{"id":1,"ok":true}`, `{"id":2,"ok":true}`}

func newBPApp(t *testing.T) *bpApp {
	t.Helper()
	app := &bpApp{wrong: map[string]bool{}, hold: map[string]bool{}, cut: map[string]bool{}, held: make(chan struct{}, 1)}
	app.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		app.mu.Lock()
		app.seen = append(app.seen, r.Method+" "+r.RequestURI)
		app.hosts = append(app.hosts, r.Host)
		wrong := app.wrong[r.URL.Path]
		hold, cut := app.hold[r.URL.Path], app.cut[r.URL.Path]
		app.mu.Unlock()
		// keep holds the request until whoever sent it gives up on it.
		keep := func() {
			app.held <- struct{}{}
			<-r.Context().Done()
		}
		if hold {
			keep()
			return
		}
		if strings.HasSuffix(r.URL.Path, "-stream") {
			w.Header().Set("Content-Type", "application/x-ndjson")
			for i, line := range bpStreamLines {
				if cut && i == 1 {
					keep()
					return
				}
				_, _ = w.Write([]byte(line + "\n"))
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if wrong {
			_, _ = w.Write([]byte(`{"ok":false}`))
			return
		}
		_, _ = w.Write([]byte(bpBody))
	}))
	t.Cleanup(app.Close)
	return app
}

func (a *bpApp) asked() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

// askedAs is the Host header of each request, in order.
func (a *bpApp) askedAs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.hosts...)
}

// host is the app's own address, which is what a client of its URL calls it.
func (a *bpApp) host() string { return strings.TrimPrefix(a.URL, "http://") }

// bpTestCase is a test recorded somewhere else: at an address that is not the
// base path's host, on a port nothing listens on any more. The test case keeps
// that port apart from its URL as well, as recording does (app_port), and the
// Host header the app was asked under then. A name ending in "-stream" makes
// it a streaming test.
func bpTestCase(name string, recordedPort uint16) *models.TestCase {
	return bpRequest(name, recordedPort, "/"+name)
}

// bpRequest is bpTestCase asking for target, the request target as recorded.
func bpRequest(name string, recordedPort uint16, target string) *models.TestCase {
	recordedHost := net.JoinHostPort("127.0.0.9", strconv.Itoa(int(recordedPort)))
	tc := &models.TestCase{
		Version: models.GetVersion(),
		Kind:    models.HTTP,
		Name:    name,
		AppPort: recordedPort,
		HTTPReq: models.HTTPReq{
			Method:    models.Method("GET"),
			URL:       "http://" + recordedHost + target,
			Header:    map[string]string{"Host": recordedHost},
			Timestamp: time.Now(),
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header: map[string]string{
				"Content-Type":   "application/json",
				"Content-Length": strconv.Itoa(len(bpBody)),
				"Date":           bpRecordedDate,
			},
			Body:      bpBody,
			Timestamp: time.Now(),
		},
		// As recording marks it: the app stamps each response with the time.
		Noise: map[string][]string{"header.Date": {}},
	}
	if strings.HasSuffix(name, "-stream") {
		tc.HTTPResp.Header = map[string]string{"Content-Type": "application/x-ndjson", "Date": bpRecordedDate}
		tc.HTTPResp.Body = ""
		for _, line := range bpStreamLines {
			tc.HTTPResp.StreamBody = append(tc.HTTPResp.StreamBody, models.HTTPStreamChunk{
				Data: []models.HTTPStreamDataField{{Key: "raw", Value: line}},
			})
		}
	}
	return tc
}

// bpGRPCApp is the app's gRPC side, on a port of its own as a gRPC server
// usually is. It answers every call with one message, keeping the authority
// each call asked for it under, in order.
type bpGRPCApp struct {
	addr string
	mu   sync.Mutex
	as   []string
}

// bpRawCodec hands gRPC messages through as the bytes they are, so the app
// needs no .proto.
type bpRawCodec struct{}

func (bpRawCodec) Name() string { return "proto" }
func (bpRawCodec) Marshal(v interface{}) ([]byte, error) {
	return *(v.(*[]byte)), nil
}
func (bpRawCodec) Unmarshal(data []byte, v interface{}) error {
	*(v.(*[]byte)) = append([]byte(nil), data...)
	return nil
}

func newBPGRPCApp(t *testing.T) *bpGRPCApp {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	app := &bpGRPCApp{addr: ln.Addr().String()}
	server := grpc.NewServer(grpc.ForceServerCodec(bpRawCodec{}),
		grpc.UnknownServiceHandler(func(_ interface{}, stream grpc.ServerStream) error {
			md, _ := metadata.FromIncomingContext(stream.Context())
			app.mu.Lock()
			app.as = append(app.as, strings.Join(md.Get(":authority"), ","))
			app.mu.Unlock()
			var in []byte
			for stream.RecvMsg(&in) == nil {
			}
			out := []byte{0x0a, 0x02, 'o', 'k'}
			return stream.SendMsg(&out)
		}))
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	return app
}

// askedAs is the authority of each call, in order.
func (a *bpGRPCApp) askedAs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.as...)
}

// testCase is a gRPC test of the app recorded somewhere else, as bpTestCase
// is: at another host, on the port the app's gRPC side still has. Its recorded
// answer is the one the app gives.
func (a *bpGRPCApp) testCase(t *testing.T, name string) *models.TestCase {
	t.Helper()
	_, port, err := net.SplitHostPort(a.addr)
	if err != nil {
		t.Fatal(err)
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	newCase := func(authority string) *models.TestCase {
		req := models.GrpcReq{Headers: models.GrpcHeaders{
			PseudoHeaders: map[string]string{
				":authority": authority, ":path": "/orders.Orders/Get", ":method": "POST", ":scheme": "http",
			},
			OrdinaryHeaders: map[string]string{"content-type": "application/grpc"},
		}}
		req.SetMessages([]models.GrpcLengthPrefixedMessage{{}})
		return &models.TestCase{
			Version: models.GetVersion(), Kind: models.GRPC_EXPORT, Name: name,
			AppPort: uint16(portNum), GrpcReq: req,
		}
	}
	recorded, err := pkg.SimulateGRPC(context.Background(), newCase(a.addr), "test-set-0", zap.NewNop(), pkg.SimulationConfig{APITimeout: 5})
	if err != nil {
		t.Fatalf("recording the gRPC test: %v", err)
	}
	a.mu.Lock()
	a.as = nil
	a.mu.Unlock()
	test := newCase(net.JoinHostPort("127.0.0.9", port))
	test.GrpcResp = *recorded
	return test
}

type bpRun struct {
	replayer *Replayer
	tests    *bpTestDB
	agent    *bpNoAgent
	mocks    *bpMockDB
	mappings *bpMappingDB
	report   *bpReportDB
	logs     *observer.ObservedLogs
}

// newBasePathRun builds the replayer `keploy test --base-path basePath` runs,
// over the named recorded tests: no command, so no agent, with the hooks and
// the settings the command ships with.
func newBasePathRun(t *testing.T, basePath string, names ...string) *bpRun {
	t.Helper()
	recordedPort := closedLocalPort(t)
	cases := make([]*models.TestCase, 0, len(names))
	for _, name := range names {
		cases = append(cases, bpTestCase(name, recordedPort))
	}
	return newBasePathRunOf(t, basePath, nil, cases...)
}

// bpTestSetConf is a test set's config.yaml holding template values.
type bpTestSetConf struct {
	prTestSetConf
	template map[string]interface{}
}

func (c bpTestSetConf) Read(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{Template: c.template}, nil
}

// newBasePathRunOf is newBasePathRun over the given test cases, in a test set
// with the given template values.
func newBasePathRunOf(t *testing.T, basePath string, template map[string]interface{}, cases ...*models.TestCase) *bpRun {
	t.Helper()
	cfg := &config.Config{}
	cfg.Path = t.TempDir()
	cfg.Test.BasePath = basePath
	// What config/default.go sets, which a bare config.Config does not.
	cfg.Test.Host = "localhost"
	cfg.Test.APITimeout = 5
	cfg.Test.MaxFlakyChecks = 1

	core, logs := observer.New(zap.InfoLevel)
	run := &bpRun{
		tests:    &bpTestDB{prTestDB: &prTestDB{cases: cases}},
		agent:    &bpNoAgent{},
		mocks:    &bpMockDB{prMockDB: &prMockDB{}},
		mappings: &bpMappingDB{prMappingDB: &prMappingDB{}},
		report:   &bpReportDB{prReportDB: &prReportDB{}},
		logs:     logs,
	}
	svc := NewReplayer(zap.New(core), run.tests, run.mocks, run.report, run.mappings,
		bpTestSetConf{template: template}, prTelemetry{}, run.agent, nil, cfg)
	run.replayer = svc.(*Replayer)
	return run
}

// start runs the replay and returns the exit code the process would end with,
// and Start's error.
func (b *bpRun) start(t *testing.T) (int, error) {
	t.Helper()
	return b.startStoppedBy(t, nil)
}

// startStoppedBy is start for a run its user stops part-way: stop is handed
// what cancels the run's context, as a SIGINT or a SIGTERM does, before the
// run starts.
func (b *bpRun) startStoppedBy(t *testing.T, stop func(interrupt func())) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	if stop != nil {
		stop(cancel)
	}
	return b.startIn(t, ctx)
}

// startIn is start with ctx for the run's root context.
func (b *bpRun) startIn(t *testing.T, ctx context.Context) (int, error) {
	t.Helper()
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0
	err := b.replayer.Start(ctx)
	return utils.ErrCode, err
}

// errorLogs is the run's ERROR lines, which a CI script that greps the log for
// ERROR fails on.
func (b *bpRun) errorLogs() []string {
	var out []string
	for _, e := range b.logs.All() {
		if e.Level >= zap.ErrorLevel {
			out = append(out, e.Message)
		}
	}
	return out
}

// said is the run's lines that contain msg, at the level given.
func (b *bpRun) said(level zapcore.Level, msg string) []observer.LoggedEntry {
	return b.logs.FilterLevelExact(level).FilterMessageSnippet(msg).All()
}

// failureOf is why the named test failed, as its result in the report says:
// the body keploy puts in place of an answer it never got.
func (b *bpRun) failureOf(name string) string {
	b.report.mu.Lock()
	defer b.report.mu.Unlock()
	for _, r := range b.report.results {
		if r.TestCaseID == name && r.Status == models.TestStatusFailed {
			return r.Res.Body
		}
	}
	return ""
}

// The mode itself: every recorded test is sent to the base path and compared,
// the report is written, and the run exits 0. It asks the agent for nothing,
// since there is none, and reads no mocks, since nothing would serve them.
//
// It failed before the first test: RunTestSet stored the set's mocks on the
// agent whether or not the run had one, and the store failed with
// `Post "/storemocks": unsupported protocol scheme ""`.
func TestBasePathReplaysEveryTestWithoutAnAgent(t *testing.T) {
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1", "test-2", "test-3")

	code, err := run.start(t)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := run.agent.made(); len(got) != 0 {
		t.Fatalf("the run has no agent and still called it: %v", got)
	}
	if got, want := app.asked(), []string{"GET /test-1", "GET /test-2", "GET /test-3"}; !slices.Equal(got, want) {
		t.Fatalf("the app at the base path was asked %v, want %v", got, want)
	}
	// By its own name, not the one it was recorded under: a front that routes
	// by Host sends that one somewhere else.
	if got, want := app.askedAs(), []string{app.host(), app.host(), app.host()}; !slices.Equal(got, want) {
		t.Fatalf("the app at the base path was asked as %v, want %v", got, want)
	}
	rep := run.report.report
	if rep == nil {
		t.Fatal("no report was written")
	}
	if rep.Status != string(models.TestSetStatusPassed) || rep.Total != 3 || rep.Success != 3 || rep.Failure != 0 {
		t.Fatalf("report: status %s, total %d, passed %d, failed %d; want PASSED 3/3/0",
			rep.Status, rep.Total, rep.Success, rep.Failure)
	}
	if code != 0 {
		t.Fatalf("a run where every test passed exited %d", code)
	}
	if got := run.mocks.reads.Load() + run.mappings.reads.Load(); got != 0 {
		t.Fatalf("the run mocks nothing and still read the set's mocks or mappings %d time(s)", got)
	}
	// The hooks around the run are not the agent's alone: they still fire.
	if before, after := run.agent.beforeTestRuns.Load(), run.agent.afterTestRuns.Load(); before != 1 || after != 1 {
		t.Fatalf("BeforeTestRun ran %d time(s) and AfterTestRun %d, want once each", before, after)
	}
	if got := run.errorLogs(); len(got) != 0 {
		t.Fatalf("a passing run logged errors: %v", got)
	}
}

// A test the app answers differently fails the run: the report says FAILED
// and the process exits non-zero, with the tests that passed still counted.
func TestBasePathFailedTestFailsTheRun(t *testing.T) {
	app := newBPApp(t)
	app.wrong["/test-2"] = true
	run := newBasePathRun(t, app.URL, "test-1", "test-2", "test-3")

	code, err := run.start(t)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rep := run.report.report
	if rep == nil {
		t.Fatal("no report was written")
	}
	if rep.Status != string(models.TestSetStatusFailed) || rep.Success != 2 || rep.Failure != 1 {
		t.Fatalf("report: status %s, passed %d, failed %d; want FAILED 2/1", rep.Status, rep.Success, rep.Failure)
	}
	if code != 1 {
		t.Fatalf("a run with a failed test exited %d, want 1", code)
	}
	if got := run.agent.made(); len(got) != 0 {
		t.Fatalf("the run has no agent and still called it: %v", got)
	}
}

// Nothing listening at the base path is a failed run too, not an empty one.
func TestBasePathWithNoAppFailsTheRun(t *testing.T) {
	nowhere := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(closedLocalPort(t))))
	run := newBasePathRun(t, nowhere, "test-1")

	code, _ := run.start(t)

	rep := run.report.report
	if rep == nil {
		t.Fatal("no report was written")
	}
	if rep.Status == string(models.TestSetStatusPassed) || rep.Failure != 1 {
		t.Fatalf("report: status %s, failed %d; want a failed set with 1 failure", rep.Status, rep.Failure)
	}
	if code != 1 {
		t.Fatalf("a run that reached no app exited %d, want 1", code)
	}
	if got := run.agent.made(); len(got) != 0 {
		t.Fatalf("the run has no agent and still called it: %v", got)
	}
}

// A run its user stops, stops there. The test whose answer it was waiting for
// gets no verdict, no later test is started, the report says the run was
// stopped, and the process exits 0, as `keploy test -c` does when it is
// interrupted with nothing failed.
//
// It ran on instead. What tells the test loops a cancelled run is over is the
// goroutine watching the app keploy started, and this run starts none. The
// request in flight failed on the cancelled context, every test after it was
// started on that context and failed on it at once, and all of them were
// reported FAILED, with exit code 1. --must-pass then deleted them from the
// test set as its failing tests, so each case here runs with it on.
func TestBasePathRunItsUserStopsGoesNoFurther(t *testing.T) {
	plain := []string{"test-1", "test-2", "test-3-stream"}
	streamed := []string{"test-1", "test-2-stream", "test-3-stream"}
	for _, tc := range []struct {
		name  string
		tests []string
		// When the user stops the run: once the app holds the request for
		// hold, or once the test named has its answer (afterAnswer), which
		// for a streamed test is the start of it, or its result
		// (afterResult). The app never finishes the streamed answer to cut.
		hold, cut, afterAnswer, afterResult string
		// The tests the run got as far as sending, and how many passed.
		wantStarted []string
		wantPassed  int
	}{
		{name: "as a test waits for its answer", tests: plain, hold: "/test-1",
			wantStarted: []string{"test-1"}},
		{name: "between two tests", tests: plain, afterAnswer: "test-1",
			wantStarted: []string{"test-1"}, wantPassed: 1},
		{name: "as a streamed test waits for its answer", tests: streamed, hold: "/test-2-stream",
			wantStarted: []string{"test-1", "test-2-stream"}, wantPassed: 1},
		{name: "as a streamed test's answer comes in", tests: streamed, cut: "/test-2-stream", afterAnswer: "test-2-stream",
			wantStarted: []string{"test-1", "test-2-stream"}, wantPassed: 1},
		{name: "between two streamed tests", tests: streamed, afterResult: "test-2-stream",
			wantStarted: []string{"test-1", "test-2-stream"}, wantPassed: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newBPApp(t)
			app.hold[tc.hold], app.cut[tc.cut] = true, true
			run := newBasePathRun(t, app.URL, tc.tests...)
			run.replayer.config.Test.MustPass = true
			run.replayer.config.Test.MaxFailAttempts = 5 // the default

			code, err := run.startStoppedBy(t, func(interrupt func()) {
				run.agent.afterSimulate = func(test string) {
					if test == tc.afterAnswer {
						interrupt()
					}
				}
				run.report.onResult = func(test string) {
					if test == tc.afterResult {
						interrupt()
					}
				}
				if tc.hold != "" {
					go func() {
						<-app.held
						interrupt()
					}()
				}
			})

			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := run.agent.started(); !slices.Equal(got, tc.wantStarted) {
				t.Fatalf("the run started %v, want it to stop after %v", got, tc.wantStarted)
			}
			rep := run.report.report
			if rep == nil {
				t.Fatal("no report was written")
			}
			if rep.Status != string(models.TestSetStatusUserAbort) || rep.Success != tc.wantPassed || rep.Failure != 0 {
				t.Fatalf("report: status %s, passed %d, failed %d; want USER_ABORT with %d passed and none failed",
					rep.Status, rep.Success, rep.Failure, tc.wantPassed)
			}
			for _, res := range run.report.results {
				if res.Status == models.TestStatusFailed {
					t.Fatalf("%s is reported FAILED (%s); a test the stop cut short did not fail", res.TestCaseID, res.Res.Body)
				}
			}
			if got := run.tests.deleted.Load(); got != 0 {
				t.Fatalf("--must-pass deleted %d test case(s) of a run its user stopped", got)
			}
			if code != 0 {
				t.Fatalf("a run stopped with no test failed exited %d, want 0", code)
			}
			if got := run.agent.made(); len(got) != 0 {
				t.Fatalf("the run has no agent and still called it: %v", got)
			}
			// A stop is not an error, and an app keploy did not start has no
			// logs in the report to point at.
			if got := run.errorLogs(); len(got) != 0 {
				t.Fatalf("a run its user stopped logged errors: %v", got)
			}
			// The first pass says so when it stops short; the streamed pass,
			// after it, does not count what it did not run.
			said := run.said(zap.InfoLevel, "stopped before running every test")
			if wantSaid := len(tc.wantStarted) < 2; (len(said) == 1) != wantSaid ||
				len(said) == 1 && strings.Contains(fmt.Sprint(said[0].ContextMap()["next_step"]), "application logs") {
				t.Fatalf("the run said %+v; want it to say once, at INFO, that it stopped, with a next step that is not the app's logs", said)
			}
		})
	}
}

// bpEndingRoot is a run's root context that the test ends, with the error
// given, as a deadline passing ends one.
type bpEndingRoot struct {
	context.Context
	done chan struct{}
	err  error
}

func newBPEndingRoot() *bpEndingRoot {
	return &bpEndingRoot{Context: context.Background(), done: make(chan struct{})}
}

func (c *bpEndingRoot) Done() <-chan struct{} { return c.done }
func (c *bpEndingRoot) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}
func (c *bpEndingRoot) end(err error) {
	c.err = err
	close(c.done)
}

// A run whose context ends for any other reason than its user's stop goes no
// further either, and fails no test for it, but it is not a stop: the set ends
// INTERNAL_ERR, says why, and the run exits 1. A deadline on the run was read
// as its user's stop: USER_ABORT, exit code 0.
func TestBasePathRunEndedOtherThanByItsUserFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		// root is the run's root context, and end what ends it while the app
		// holds the second test's request.
		root func() (context.Context, func())
		want string
	}{
		{name: "its deadline passes", want: context.DeadlineExceeded.Error(), root: func() (context.Context, func()) {
			root := newBPEndingRoot()
			return root, func() { root.end(context.DeadlineExceeded) }
		}},
		{name: "what runs it ends it for a cause", want: "the CI job was cancelled", root: func() (context.Context, func()) {
			root, cancel := context.WithCancelCause(context.Background())
			return root, func() { cancel(errors.New("the CI job was cancelled")) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newBPApp(t)
			app.hold["/test-2"] = true
			run := newBasePathRun(t, app.URL, "test-1", "test-2", "test-3-stream")
			run.replayer.config.Test.MustPass = true
			run.replayer.config.Test.MaxFailAttempts = 5
			root, end := tc.root()
			go func() {
				<-app.held
				end()
			}()

			code, _ := run.startIn(t, root)

			if got, want := run.agent.started(), []string{"test-1", "test-2"}; !slices.Equal(got, want) {
				t.Fatalf("the run started %v, want it to stop after %v", got, want)
			}
			rep := run.report.report
			if rep == nil {
				t.Fatal("no report was written")
			}
			if rep.Status != string(models.TestSetStatusInternalErr) || rep.Success != 1 || rep.Failure != 0 {
				t.Fatalf("report: status %s, passed %d, failed %d; want INTERNAL_ERR with 1 passed and none failed",
					rep.Status, rep.Success, rep.Failure)
			}
			if got := run.tests.deleted.Load(); got != 0 {
				t.Fatalf("--must-pass deleted %d test case(s) of a run that could not finish", got)
			}
			if code != 1 {
				t.Fatalf("a run that could not finish exited %d, want 1", code)
			}
			said := run.said(zap.ErrorLevel, "stopped before running every test")
			if len(said) != 1 || !strings.Contains(fmt.Sprint(said[0].ContextMap()["cause"]), tc.want) {
				t.Fatalf("the run said %+v; want it to say, once, that it stopped early, with the cause %q", said, tc.want)
			}
		})
	}
}

// A replay that cannot run its test set must not exit 0. `keploy test` drops
// the error Start returns, so the exit code is all a CI job sees: the store
// failure above ran no test, wrote no report, and was a green job.
func TestBasePathRunThatCannotRunItsTestSetExitsNonZero(t *testing.T) {
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1")
	run.report.refuseReport = true

	code, err := run.start(t)

	if err == nil {
		t.Fatal("Start returned nil for a test set that could not run")
	}
	if code != 1 {
		t.Fatalf("a replay that failed with %q exited %d, want 1", err, code)
	}
}

// A base path keploy cannot read runs no test. It was left out of every test's
// URL instead, behind an ERROR line each, and the tests went to the address
// they were recorded at: a green run against something else. A scheme with no
// host was read as a base path that names no host, and the tests went to
// test.host, localhost, on the recorded port: green against the developer's
// own machine.
func TestBasePathThatCannotBeReadRunsNoTest(t *testing.T) {
	for _, basePath := range []string{
		"127.0.0.1:8080", "localhost:8080", "my staging/api", "staging.example.com/api",
		"http:staging:9999", "http://", "http:/staging:9999", "http://:9999",
		"http://staging?x=1",
	} {
		t.Run(basePath, func(t *testing.T) {
			run := newBasePathRun(t, basePath, "test-1")

			code, err := run.start(t)

			if err == nil || !strings.Contains(err.Error(), basePath) {
				t.Fatalf("Start returned %v, want an error naming the base path", err)
			}
			if code != 1 {
				t.Fatalf("exited %d, want 1", code)
			}
			if run.report.report != nil || len(run.report.results) != 0 {
				t.Fatalf("the run went on to its tests: report %+v, %d result(s)", run.report.report, len(run.report.results))
			}
		})
	}

	// A password in a refused base path is not repeated in the log or the
	// error, whether or not the base path parses.
	for _, basePath := range []string{"http://ci:s3cret@staging:8080", "http://ci:s3cret@staging:port"} {
		t.Run("with a password", func(t *testing.T) {
			run := newBasePathRun(t, basePath, "test-1")

			code, err := run.start(t)

			if err == nil || code != 1 {
				t.Fatalf("Start returned %v and exit code %d, want the base path refused", err, code)
			}
			for _, said := range append(run.errorLogs(), err.Error()) {
				if strings.Contains(said, "s3cret") {
					t.Fatalf("the password was repeated: %q", said)
				}
			}
			for _, e := range run.logs.All() {
				for _, v := range e.ContextMap() {
					if strings.Contains(fmt.Sprint(v), "s3cret") {
						t.Fatalf("the password was repeated in %q: %v", e.Message, e.ContextMap())
					}
				}
			}
		})
	}
}

// It runs no test either when keploy starts the app itself, which a config
// holding both a command and a base path asks for. The base path still goes
// in every test's URL then, so one that cannot be read stops the run before
// the app and the agent are started for nothing.
func TestBasePathThatCannotBeReadStopsARunThatStartsTheApp(t *testing.T) {
	cfg := &config.Config{Command: "./app"}
	cfg.Path = t.TempDir()
	cfg.Test.BasePath = "127.0.0.1:8080"
	agent := &bpNoAgent{}
	report := &prReportDB{}
	svc := NewReplayer(zap.NewNop(), &prTestDB{cases: []*models.TestCase{bpTestCase("test-1", closedLocalPort(t))}},
		&prMockDB{}, report, &prMappingDB{}, prTestSetConf{}, prTelemetry{}, agent, nil, cfg)
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()

	err := svc.Start(ctx)

	if err == nil || !strings.Contains(err.Error(), "cannot replay against the base path") {
		t.Fatalf("Start returned %v, want the base path refused", err)
	}
	if got := agent.made(); len(got) != 0 {
		t.Fatalf("the run went on to set up its agent: %v", got)
	}
	if utils.ErrCode != 1 {
		t.Fatalf("exited %d, want 1", utils.ErrCode)
	}
}

// A base path is one of two shapes: an http or https URL with a host, or a
// path prefix. Every other is refused, a schemeless host among them, since
// each reads as one of the two with no host and sends the tests to test.host.
func TestParseBasePath(t *testing.T) {
	for basePath, ok := range map[string]bool{
		"http://staging.example.com":      true,
		"https://127.0.0.1:8443/api":      true,
		"HTTPS://staging.example.com/api": true,
		"http://[::1]:8080":               true,
		"http://[fe80::1%25eth0]:8080":    true,
		"/api":                            true,
		"/":                               true,

		"127.0.0.1:8080":            false, // does not parse
		"localhost:8080":            false, // the scheme "localhost"
		"staging.example.com/api":   false, // a path, with no scheme
		"//staging.example.com/api": false, // a host, with no scheme
		"my staging/api":            false,
		"ftp://staging.example.com": false,
		// A scheme and no host.
		"http:staging:9999":  false, // opaque
		"http://":            false,
		"http:/staging:9999": false, // a path
		"http://:9999":       false, // a port and no host
		// Nowhere to go.
		"http://ci:secret@staging": false,
		"http://staging?x=1":       false,
		"http://staging?":          false,
		"http://staging#top":       false,
		"/api?x=1":                 false,
	} {
		if _, err := parseBasePath(basePath); (err == nil) != ok {
			t.Errorf("parseBasePath(%q) = %v, want ok=%v", basePath, err, ok)
		}
	}
}

// The base path replaces a recorded URL's scheme and host and goes in front of
// its path. Everything after the recorded host is what the app was sent when
// the test was recorded, and the rewrite leaves it as it is.
//
// The rewrite parsed the URL, joined and cleaned the paths, and unescaped the
// whole result: %20 became a space the app refuses the request line for, %23
// cut the query short as a fragment, %2F became a path separator, %26 and %3D
// became a second parameter, and a trailing slash was dropped.
func TestReplaceBaseURL(t *testing.T) {
	const recorded = "http://127.0.0.1:18911"
	for _, tc := range []struct {
		name, base, old, want string
	}{
		{"origin", "http://10.0.0.5:9000", recorded + "/orders/1", "http://10.0.0.5:9000/orders/1"},
		{"scheme", "https://staging.example.com", recorded + "/orders", "https://staging.example.com/orders"},
		{"path prefix", "http://10.0.0.5:9000/api", recorded + "/orders", "http://10.0.0.5:9000/api/orders"},
		{"path prefix ending in a slash", "http://10.0.0.5:9000/api/", recorded + "/orders", "http://10.0.0.5:9000/api/orders"},
		{"IPv6 host", "http://[::1]:9000", recorded + "/orders", "http://[::1]:9000/orders"},
		// Escaped as a URL writes it, so the result parses again.
		{"IPv6 host with a zone", "http://[fe80::1%25eth0]:9000", recorded + "/orders", "http://[fe80::1%25eth0]:9000/orders"},
		// A path prefix names no host: it is left empty, for test.host, and
		// the recorded scheme stays.
		{"path alone names no host", "/api", recorded + "/orders", "http:///api/orders"},
		{"path alone keeps the recorded scheme", "/api", "https://127.0.0.1:8443/orders", "https:///api/orders"},

		{"escaped space in the query", "http://10.0.0.5:9000", recorded + "/orders/9?note=a%20b", "http://10.0.0.5:9000/orders/9?note=a%20b"},
		{"escaped # in the query", "http://10.0.0.5:9000", recorded + "/items?tag=a%23b", "http://10.0.0.5:9000/items?tag=a%23b"},
		{"escaped & and = in a value", "http://10.0.0.5:9000", recorded + "/q?x=a%26b%3Dc", "http://10.0.0.5:9000/q?x=a%26b%3Dc"},
		{"plus in the query", "http://10.0.0.5:9000", recorded + "/q?x=a+b", "http://10.0.0.5:9000/q?x=a+b"},
		{"escaped / in the path", "http://10.0.0.5:9000/api", recorded + "/files/a%2Fb/c", "http://10.0.0.5:9000/api/files/a%2Fb/c"},
		{"escaped space in the path", "http://10.0.0.5:9000", recorded + "/files/a%20b", "http://10.0.0.5:9000/files/a%20b"},
		{"trailing slash", "http://10.0.0.5:9000/api", recorded + "/items/", "http://10.0.0.5:9000/api/items/"},
		{"dot segments and doubled slashes", "http://10.0.0.5:9000", recorded + "/a//b/../c", "http://10.0.0.5:9000/a//b/../c"},
		// Not an escape at all, and what the app was sent all the same.
		{"a % that escapes nothing", "http://10.0.0.5:9000", recorded + "/search?q=100%", "http://10.0.0.5:9000/search?q=100%"},
		{"template values", "http://10.0.0.5:9000/api", recorded + "/orders/{{string .id}}?t={{ .token }}", "http://10.0.0.5:9000/api/orders/{{string .id}}?t={{ .token }}"},
		{"no path", "http://10.0.0.5:9000/api", recorded, "http://10.0.0.5:9000/api"},
		{"query and no path", "http://10.0.0.5:9000/api", recorded + "?page=2", "http://10.0.0.5:9000/api?page=2"},
		{"a URL in the query", "http://10.0.0.5:9000", recorded + "/go?to=http://example.com/a", "http://10.0.0.5:9000/go?to=http://example.com/a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ReplaceBaseURL(tc.base, tc.old)
			if err != nil {
				t.Fatalf("ReplaceBaseURL(%q, %q): %v", tc.base, tc.old, err)
			}
			if got != tc.want {
				t.Fatalf("ReplaceBaseURL(%q, %q) = %q, want %q", tc.base, tc.old, got, tc.want)
			}
			if _, err := url.Parse(got); err != nil {
				t.Fatalf("ReplaceBaseURL(%q, %q) = %q, which does not parse: %v", tc.base, tc.old, got, err)
			}
		})
	}

	// A URL that does not start with a scheme and a host has none to replace,
	// and a base path that cannot be read replaces nothing.
	for _, tc := range []struct{ name, base, old string }{
		{"a path for a URL", "http://10.0.0.5:9000", "/orders"},
		{"a template for the origin", "http://10.0.0.5:9000", "{{string .origin}}/orders"},
		{"a URL only in the query", "http://10.0.0.5:9000", "/go?to=http://example.com/a"},
		{"no URL", "http://10.0.0.5:9000", ""},
		{"a base path that is not a URL", "127.0.0.1:8080", recorded + "/orders"},
		{"a base path with no scheme", "staging.example.com/api", recorded + "/orders"},
		{"a base path with a scheme and no host", "http:staging:9999", recorded + "/orders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ReplaceBaseURL(tc.base, tc.old); err == nil {
				t.Fatalf("ReplaceBaseURL(%q, %q) = %q, want an error", tc.base, tc.old, got)
			}
		})
	}
}

// What the app at the base path is asked is the recorded request, escapes and
// all, as it is when keploy starts the app itself. Start to app, so that
// nothing between the rewrite and the wire undoes it. (A URL with %7B in it is
// not among these: sending decodes such a URL whole, in every mode.)
func TestBasePathSendsTheRecordedRequestAsItWasRecorded(t *testing.T) {
	app := newBPApp(t)
	recordedPort := closedLocalPort(t)
	targets := []string{
		"/orders/9?note=a%20b",
		"/items?tag=a%23b",
		"/q?x=a%26b%3Dc",
		"/files/a%2Fb/c",
		"/items/",
		"/search?q=100%",
	}
	var cases []*models.TestCase
	var want []string
	for i, target := range targets {
		cases = append(cases, bpRequest(fmt.Sprintf("test-%d", i+1), recordedPort, target))
		want = append(want, "GET /v2"+target)
	}
	run := newBasePathRunOf(t, app.URL+"/v2", nil, cases...)

	code, err := run.start(t)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := app.asked(); !slices.Equal(got, want) {
		t.Fatalf("the app at the base path was asked\n  %q\nwant the recorded requests under the base path\n  %q", got, want)
	}
	if rep := run.report.report; rep == nil || rep.Status != string(models.TestSetStatusPassed) || rep.Success != len(targets) {
		t.Fatalf("report %+v; want PASSED with %d passed", rep, len(targets))
	}
	if code != 0 {
		t.Fatalf("a run where every test passed exited %d", code)
	}
	if got := run.errorLogs(); len(got) != 0 {
		t.Fatalf("a passing run logged errors: %v", got)
	}
}

// A test that cannot be pointed at the base path is failed, and is not sent.
// Its URL still names where it was recorded, and a base-path run sends each
// test to the address in its URL: the request went to the recording's
// environment, behind an ERROR line, and its answer passed the test.
//
// The URL here has a template value for its origin, which says nothing of
// where the origin ends until it is filled in, and is filled in after the
// base path is applied. The recorded server is up, as the recording's
// environment usually still is.
func TestBasePathTestThatCannotBePointedAtItIsNotSent(t *testing.T) {
	for _, name := range []string{"orders", "orders-stream"} {
		t.Run(name, func(t *testing.T) {
			app := newBPApp(t)
			recorded := newBPApp(t)
			recordedPort := closedLocalPort(t)
			unpointable := bpTestCase(name, recordedPort)
			unpointable.HTTPReq.URL = "{{string .origin}}/" + name
			run := newBasePathRunOf(t, app.URL, map[string]interface{}{"origin": recorded.URL},
				bpTestCase("health", recordedPort), unpointable)

			code, err := run.start(t)

			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := recorded.asked(); len(got) != 0 {
				t.Fatalf("the server the test was recorded at was sent %v; a base-path run sends it nothing", got)
			}
			if got, want := app.asked(), []string{"GET /health"}; !slices.Equal(got, want) {
				t.Fatalf("the app at the base path was asked %v, want %v", got, want)
			}
			rep := run.report.report
			if rep == nil || rep.Status != string(models.TestSetStatusFailed) || rep.Success != 1 || rep.Failure != 1 {
				t.Fatalf("report %+v; want FAILED with 1 passed and 1 failed", rep)
			}
			if why := run.failureOf(name); !strings.Contains(why, "cannot send the test to the base path") {
				t.Fatalf("the report gives %q as why %s failed; want it to say the test could not be sent to the base path", why, name)
			}
			if code != 1 {
				t.Fatalf("a run with a test it could not send exited %d, want 1", code)
			}
			if got := run.agent.made(); len(got) != 0 {
				t.Fatalf("the run has no agent and still called it: %v", got)
			}
		})
	}
}

// A gRPC test is sent to the host the base path names, as the HTTP tests of
// its set are: the base path is where the app is. It has no URL for the base
// path to go in, so it was left as recorded, and went to test.host (localhost
// unless set) with no word of it, to pass or fail against whatever listens
// there. It keeps the port it was recorded on: the base path's is the app's
// HTTP port.
func TestBasePathSendsAGRPCTestToItsHost(t *testing.T) {
	app := newBPApp(t)
	grpcApp := newBPGRPCApp(t)
	run := newBasePathRunOf(t, app.URL, nil,
		bpTestCase("test-1", closedLocalPort(t)), grpcApp.testCase(t, "grpc-1"))
	// A host that is not the app's, as localhost is not for an app elsewhere.
	run.replayer.config.Test.Host = "127.0.0.2"

	code, err := run.start(t)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := grpcApp.askedAs(), []string{grpcApp.addr}; !slices.Equal(got, want) {
		t.Fatalf("the app's gRPC side was asked as %v, want %v: the base path's host on the recorded port", got, want)
	}
	if got, want := app.asked(), []string{"GET /test-1"}; !slices.Equal(got, want) {
		t.Fatalf("the app at the base path was asked %v, want %v", got, want)
	}
	rep := run.report.report
	if rep == nil || rep.Status != string(models.TestSetStatusPassed) || rep.Success != 2 || rep.Failure != 0 {
		t.Fatalf("report %+v; want PASSED with 2 passed", rep)
	}
	if code != 0 {
		t.Fatalf("a run where every test passed exited %d", code)
	}
	if got := run.agent.made(); len(got) != 0 {
		t.Fatalf("the run has no agent and still called it: %v", got)
	}
	if got := run.errorLogs(); len(got) != 0 {
		t.Fatalf("a passing run logged errors: %v", got)
	}
}

// A streaming test is sent to the base path as the others are. Streaming
// tests run in a phase of their own, after the one that rewrites a test's URL
// for the base path, and were sent to the recorded address.
func TestBasePathReplaysStreamingTests(t *testing.T) {
	app := newBPApp(t)
	run := newBasePathRun(t, app.URL, "test-1", "test-2-stream")

	code, err := run.start(t)

	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := app.asked(), []string{"GET /test-1", "GET /test-2-stream"}; !slices.Equal(got, want) {
		t.Fatalf("the app at the base path was asked %v, want %v", got, want)
	}
	if got, want := app.askedAs(), []string{app.host(), app.host()}; !slices.Equal(got, want) {
		t.Fatalf("the app at the base path was asked as %v, want %v", got, want)
	}
	rep := run.report.report
	if rep == nil || rep.Status != string(models.TestSetStatusPassed) || rep.Success != 2 {
		t.Fatalf("report %+v; want PASSED with 2 passed", rep)
	}
	if code != 0 {
		t.Fatalf("a run where every test passed exited %d", code)
	}
	if got := run.agent.made(); len(got) != 0 {
		t.Fatalf("the run has no agent and still called it: %v", got)
	}
}

// A request the app drops is not sent again. With an agent, one that consumed
// no mock was provably never processed; without one nothing can say so, and
// the app at a base path writes to its real dependencies.
func TestBasePathDroppedRequestIsSentOnce(t *testing.T) {
	host, port, conns := countingListener(t)
	run := newBasePathRun(t, "http://"+net.JoinHostPort(host, port), "test-1")

	code, _ := run.start(t)

	if got := conns.Load(); got != 1 {
		t.Fatalf("the dropped request was sent %d times, want 1", got)
	}
	if got := run.agent.made(); len(got) != 0 {
		t.Fatalf("the run has no agent and still called it: %v", got)
	}
	if rep := run.report.report; rep == nil || rep.Failure != 1 {
		t.Fatalf("report %+v; want 1 failed test", rep)
	}
	if code != 1 {
		t.Fatalf("a run whose only request was dropped exited %d, want 1", code)
	}
}

// The base path is where the request goes: its host and its port, and under
// its name. All three were the recording's instead. The host was replaced by
// test.host (localhost unless set) and the port by the recorded app_port after
// the base path had been put in the URL, so the tests were sent to localhost
// on the recorded port whatever the base path said; and the Host header was
// the one recorded, which a front that routes by name sends somewhere else.
// A port the user set still applies. A base path that is only a path prefix
// names no host, so the app's usual address and name stand under it, and so
// they do for an app keploy starts itself.
func TestSimulateRequestSendsTheTestToTheBasePath(t *testing.T) {
	app := newBPApp(t)
	appHost, appPortStr, err := net.SplitHostPort(app.host())
	if err != nil {
		t.Fatal(err)
	}
	appPort, err := strconv.Atoi(appPortStr)
	if err != nil {
		t.Fatal(err)
	}
	closed := closedLocalPort(t)
	closedBase := "http://" + net.JoinHostPort(appHost, strconv.Itoa(int(closed)))
	// recordedAs is the Host header of a test recorded on that port.
	recordedAs := func(port uint16) string { return net.JoinHostPort("127.0.0.9", strconv.Itoa(int(port))) }

	for _, tc := range []struct {
		name     string
		basePath string
		// command is the app keploy starts, when it starts one.
		command string
		// host and port are test.host and test.port; recordedPort is the port
		// the test was recorded on, which it also keeps as its app_port.
		host         string
		port         uint32
		recordedPort uint16
		wantPath     string
		wantHost     string
	}{
		{name: "the base path's port, not the recorded one", basePath: app.URL,
			host: "localhost", recordedPort: closed, wantPath: "/orders", wantHost: app.host()},
		// 127.0.0.2 is loopback, and the app does not listen on it.
		{name: "the base path's host, not test.host", basePath: app.URL,
			host: "127.0.0.2", recordedPort: uint16(appPort), wantPath: "/orders", wantHost: app.host()},
		{name: "the base path's own path comes first", basePath: app.URL + "/v2",
			host: "localhost", recordedPort: closed, wantPath: "/v2/orders", wantHost: app.host()},
		{name: "test.port still applies", basePath: closedBase,
			host: "localhost", port: uint32(appPort), recordedPort: closed, wantPath: "/orders", wantHost: app.host()},
		{name: "a path prefix leaves the app's address as it was", basePath: "/v2",
			host: appHost, recordedPort: uint16(appPort), wantPath: "/v2/orders", wantHost: recordedAs(uint16(appPort))},
		// The app keploy starts is the one its mocks are for: the tests go
		// to it, whatever a configured base path says of another.
		{name: "an app keploy starts keeps its address", basePath: closedBase, command: "./app",
			host: appHost, recordedPort: uint16(appPort), wantPath: "/orders", wantHost: recordedAs(uint16(appPort))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{Command: tc.command}
			cfg.Test.BasePath = tc.basePath
			cfg.Test.Host = tc.host
			cfg.Test.Port = tc.port
			cfg.Test.APITimeout = 5
			before := len(app.asked())

			test := bpTestCase("orders", tc.recordedPort)
			rewritten, err := ReplaceBaseURL(tc.basePath, test.HTTPReq.URL)
			if err != nil {
				t.Fatalf("ReplaceBaseURL: %v", err)
			}
			test.HTTPReq.URL = rewritten

			resp, err := NewHooks(zap.NewNop(), cfg, &bpNoAgent{}).SimulateRequest(context.Background(), test, "test-set-0")
			if err != nil {
				t.Fatalf("the request did not reach the app: %v", err)
			}
			if httpResp, ok := resp.(*models.HTTPResp); !ok || httpResp.StatusCode != http.StatusOK {
				t.Fatalf("response %+v, want the app's 200", resp)
			}
			asked := app.asked()[before:]
			if want := []string{"GET " + tc.wantPath}; !slices.Equal(asked, want) {
				t.Fatalf("the app was asked %v, want %v", asked, want)
			}
			if askedAs := app.askedAs()[before:]; !slices.Equal(askedAs, []string{tc.wantHost}) {
				t.Fatalf("the app was asked as %v, want as %q", askedAs, tc.wantHost)
			}
		})
	}
}

// bpSimulate sends the tests through the hooks `keploy test` builds over cfg,
// as the replayer does once it has put the base path in the URL of each that
// has one, and returns what the run logged from INFO up.
func bpSimulate(t *testing.T, cfg *config.Config, tests ...*models.TestCase) *observer.ObservedLogs {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	hooks := NewHooks(zap.New(core), cfg, &bpNoAgent{})
	for _, test := range tests {
		if test.Kind == models.HTTP {
			rewritten, err := ReplaceBaseURL(cfg.Test.BasePath, test.HTTPReq.URL)
			if err != nil {
				t.Fatalf("ReplaceBaseURL: %v", err)
			}
			test.HTTPReq.URL = rewritten
		}
		if _, err := hooks.SimulateRequest(context.Background(), test, "test-set-0"); err != nil {
			t.Fatalf("the request did not reach the app: %v", err)
		}
	}
	return logs
}

// A test.host the user set is not where a base-path run sends the tests, and
// the run says so, once, instead of leaving them to find out. It has nothing
// to say of the localhost test.host is by default, nor of a test.host that is
// the base path's own.
func TestBasePathSaysOnceThatTestHostIsNotUsed(t *testing.T) {
	app := newBPApp(t)
	appHost, _, err := net.SplitHostPort(app.host())
	if err != nil {
		t.Fatal(err)
	}
	grpcApp := newBPGRPCApp(t)
	for _, tc := range []struct {
		name, host string
		// grpc makes the run's tests gRPC ones, which are not sent to
		// test.host either.
		grpc bool
		want int
	}{
		{name: "a host of the user's", host: "staging.internal", want: 1},
		{name: "the default", host: "localhost"},
		{name: "the base path's own host", host: appHost},
		{name: "a host of the user's, and gRPC tests", host: "staging.internal", grpc: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Test.BasePath = app.URL
			cfg.Test.Host = tc.host
			cfg.Test.APITimeout = 5
			tests := []*models.TestCase{bpTestCase("test-1", closedLocalPort(t)), bpTestCase("test-2", closedLocalPort(t))}
			if tc.grpc {
				tests = []*models.TestCase{grpcApp.testCase(t, "grpc-1"), grpcApp.testCase(t, "grpc-2")}
			}

			logs := bpSimulate(t, cfg, tests...)

			said := logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("test.host is not used").All()
			if len(said) != tc.want {
				t.Fatalf("the run said %d time(s) that test.host is not used, want %d: %+v", len(said), tc.want, said)
			}
			if tc.want == 1 && (said[0].ContextMap()["host"] != tc.host || said[0].ContextMap()["next_step"] == nil) {
				t.Fatalf("it said %v; want the host %q and a next step", said[0].ContextMap(), tc.host)
			}
		})
	}
}

// A test.port still replaces the port, the one written in the base path
// included, and so does a replaceWith rule; the run says so, once, with what
// to do: a test.port kept in keploy.yml for the app keploy starts moved every
// test of a base-path run to that port without a word. There is nothing to
// say when the base path names no port, or the tests go to the one it names.
func TestBasePathSaysOnceThatThePortInItIsNotUsed(t *testing.T) {
	app := newBPApp(t)
	appHost, appPortStr, err := net.SplitHostPort(app.host())
	if err != nil {
		t.Fatal(err)
	}
	appPort, err := strconv.Atoi(appPortStr)
	if err != nil {
		t.Fatal(err)
	}
	closed := net.JoinHostPort(appHost, strconv.Itoa(int(closedLocalPort(t))))
	for _, tc := range []struct {
		name, basePath string
		port           uint32
		replaceWith    map[string]string
		want           int
	}{
		{name: "test.port over another port", basePath: "http://" + closed, port: uint32(appPort), want: 1},
		{name: "a replaceWith rule over another port", basePath: "http://" + closed, replaceWith: map[string]string{closed: app.host()}, want: 1},
		{name: "test.port the same as the base path's", basePath: app.URL, port: uint32(appPort)},
		// Its port is the scheme's (80): test.port moves every test off it.
		{name: "a base path that names no port", basePath: "http://" + appHost, port: uint32(appPort), want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Test.BasePath = tc.basePath
			cfg.Test.Host = "localhost"
			cfg.Test.Port = tc.port
			cfg.Test.ReplaceWith.Global.URL = tc.replaceWith
			cfg.Test.APITimeout = 5

			logs := bpSimulate(t, cfg, bpTestCase("test-1", closedLocalPort(t)), bpTestCase("test-2", closedLocalPort(t)))

			said := logs.FilterLevelExact(zap.WarnLevel).FilterMessageSnippet("the port in the base path is not used").All()
			if len(said) != tc.want {
				t.Fatalf("the run said %d time(s) that the base path's port is not used, want %d: %+v", len(said), tc.want, said)
			}
			if tc.want == 1 && (said[0].ContextMap()["port"] != appPortStr || said[0].ContextMap()["next_step"] == nil) {
				t.Fatalf("it said %v; want the port the tests went to, %s, and a next step", said[0].ContextMap(), appPortStr)
			}
		})
	}
}

// The app at a base path is asked for under the base path's host, not under
// the Host header a test was recorded with, and the run says so, once, when
// the two differ: an app that checks the name it is asked for under, or
// writes it into its answers, answers differently from its recording, and
// nothing else says why. A test recorded under the base path's own name, or
// with no Host header, gives it nothing to say.
func TestBasePathSaysOnceThatTheRecordedHostIsNotSent(t *testing.T) {
	app := newBPApp(t)
	appHost, appPortStr, err := net.SplitHostPort(app.host())
	if err != nil {
		t.Fatal(err)
	}
	appPort, err := strconv.Atoi(appPortStr)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := "http://" + net.JoinHostPort(appHost, strconv.Itoa(int(closedLocalPort(t))))
	for _, tc := range []struct {
		name string
		// basePath is the app's own URL unless set, port is test.port, and
		// replaceWith a replaceWith rule.
		basePath    string
		port        uint32
		replaceWith map[string]string
		// recordedAs is the Host header of the tests; "" leaves them the one
		// bpTestCase records, which is another address's.
		recordedAs string
		noHost     bool
		want       int
	}{
		{name: "recorded under another name", want: 1},
		{name: "recorded under the base path's own name", recordedAs: app.host()},
		{name: "recorded with no Host header", noHost: true},
		// The name asked for is that of the address the tests go to, which
		// test.port and replaceWith move.
		{name: "recorded under the name test.port makes of the base path's", basePath: elsewhere, port: uint32(appPort), recordedAs: app.host()},
		{name: "recorded under the name a replaceWith rule makes of the base path's", basePath: elsewhere,
			replaceWith: map[string]string{strings.TrimPrefix(elsewhere, "http://"): app.host()}, recordedAs: app.host()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Test.BasePath = app.URL
			if tc.basePath != "" {
				cfg.Test.BasePath = tc.basePath
			}
			cfg.Test.Host = "localhost"
			cfg.Test.Port = tc.port
			cfg.Test.ReplaceWith.Global.URL = tc.replaceWith
			cfg.Test.APITimeout = 5
			tests := []*models.TestCase{bpTestCase("test-1", closedLocalPort(t)), bpTestCase("test-2", closedLocalPort(t))}
			recorded := tests[0].HTTPReq.Header["Host"]
			for _, test := range tests {
				if tc.recordedAs != "" {
					test.HTTPReq.Header["Host"] = tc.recordedAs
				}
				if tc.noHost {
					delete(test.HTTPReq.Header, "Host")
				}
			}

			logs := bpSimulate(t, cfg, tests...)

			said := logs.FilterMessageSnippet("not under the Host header they were recorded with").All()
			if len(said) != tc.want {
				t.Fatalf("the run said %d time(s) that the recorded Host is not sent, want %d: %+v", len(said), tc.want, said)
			}
			if tc.want == 1 && (said[0].ContextMap()["recordedHost"] != recorded || said[0].ContextMap()["host"] != app.host() ||
				said[0].ContextMap()["next_step"] == nil) {
				t.Fatalf("it said %v; want %q recorded, %q asked for, and a next step", said[0].ContextMap(), recorded, app.host())
			}
		})
	}
}

// The base path is where the app is only when it says so and keploy starts no
// app of its own.
func TestAppAtBasePath(t *testing.T) {
	for _, tc := range []struct {
		basePath, command string
		// want is the host the base path names for the app, "" for none.
		want string
	}{
		{basePath: "http://staging.example.com", want: "staging.example.com"},
		{basePath: "https://staging.example.com/api", want: "staging.example.com"},
		{basePath: "http://127.0.0.1:8080", want: "127.0.0.1:8080"},
		{basePath: "http://[::1]:8080/api", want: "[::1]:8080"},
		// A path prefix says nothing of where the app is.
		{basePath: "/api"},
		{basePath: ""},
		// Nor does a base path keploy cannot read.
		{basePath: "127.0.0.1:8080"},
		{basePath: "staging.example.com/api"},
		{basePath: "http:staging:9999"},
		// The app keploy starts is where its command puts it.
		{basePath: "http://staging.example.com", command: "./app"},
	} {
		cfg := &config.Config{Command: tc.command}
		cfg.Test.BasePath = tc.basePath
		at := NewHooks(zap.NewNop(), cfg, &bpNoAgent{}).(*Hooks).appAt
		switch {
		case tc.want == "" && at != nil:
			t.Errorf("base path %q, command %q: the app is at %q, want the base path to say nothing of it", tc.basePath, tc.command, at.Host)
		case tc.want != "" && (at == nil || at.Host != tc.want):
			t.Errorf("base path %q, command %q: the app is at %v, want at %q", tc.basePath, tc.command, at, tc.want)
		}
	}
}

// Whether a run has an agent is decided once, when the replayer is made, and
// its hooks go by the same decision whatever the command is later. Enterprise
// cloud replay builds the replayer under a placeholder command and then puts
// the user's back, which may be none: the replayer ran with an agent while its
// hooks, reading the command afresh, sent the tests to the base path with
// test.host, app_port and the recorded Host left out. The other way round,
// the hooks sent them to localhost on the recorded port.
func TestTheRunHasAnAgentOrNotForTheReplayerAndItsHooksAlike(t *testing.T) {
	for _, tc := range []struct {
		name           string
		built, then    string
		wantNoAgent    bool
		wantSentToBase bool
	}{
		{name: "built with a command that is then taken away", built: "docker compose up", then: ""},
		{name: "built with no command, and one set later", built: "", then: "./app", wantNoAgent: true, wantSentToBase: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newBPApp(t)
			cfg := &config.Config{Command: tc.built}
			cfg.Test.BasePath = app.URL
			cfg.Test.Host = "localhost"
			cfg.Test.APITimeout = 5
			r := NewReplayer(zap.NewNop(), &prTestDB{}, &prMockDB{}, &prReportDB{}, &prMappingDB{},
				prTestSetConf{}, prTelemetry{}, &bpNoAgent{}, nil, cfg).(*Replayer)
			cfg.Command = tc.then

			if r.noAgent != tc.wantNoAgent {
				t.Fatalf("the replayer has noAgent=%v, want %v", r.noAgent, tc.wantNoAgent)
			}
			// The test was recorded on the port the app now has, under another
			// name: a run with an agent asks there under that name, one with
			// none asks the base path under its own.
			_, appPortStr, err := net.SplitHostPort(app.host())
			if err != nil {
				t.Fatal(err)
			}
			appPort, err := strconv.Atoi(appPortStr)
			if err != nil {
				t.Fatal(err)
			}
			test := bpTestCase("test-1", uint16(appPort))
			if test.HTTPReq.URL, err = ReplaceBaseURL(cfg.Test.BasePath, test.HTTPReq.URL); err != nil {
				t.Fatal(err)
			}
			if _, err := r.hookImpl.SimulateRequest(context.Background(), test, "test-set-0"); err != nil {
				t.Fatalf("SimulateRequest: %v", err)
			}
			want := test.HTTPReq.Header["Host"]
			if tc.wantSentToBase {
				want = app.host()
			}
			if got := app.askedAs(); !slices.Equal(got, []string{want}) {
				t.Fatalf("the app was asked as %v, want %q: the hooks did not go by the replayer's decision", got, want)
			}
		})
	}
}
