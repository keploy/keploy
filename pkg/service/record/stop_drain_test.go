package record

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// stopDrainHarness wires GetTestAndMockChans to a fake agent whose streams the
// test drives, and stops the recording (cancels reqCtx) on demand.
type stopDrainHarness struct {
	f      *fakeInstr
	frames FrameChan
	stop   context.CancelFunc
	g      *errgroup.Group
}

func newStopDrainHarness(t *testing.T) *stopDrainHarness {
	t.Helper()
	return newStopDrainHarnessFor(t, "")
}

// newStopDrainHarnessFor is newStopDrainHarness for a command type: under
// docker compose the app serves on through the drain.
func newStopDrainHarnessFor(t *testing.T, commandType utils.CmdType) *stopDrainHarness {
	t.Helper()
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	cfg := &config.Config{}
	cfg.CommandType = string(commandType)
	r := &Recorder{logger: zap.NewNop(), instrumentation: f, config: cfg}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); frames.Abandon() })
	return &stopDrainHarness{f: f, frames: frames, stop: cancel, g: g}
}

func mockAt(name string, at time.Time) *models.Mock {
	m := &models.Mock{Name: name}
	m.Spec.ReqTimestampMock = at
	return m
}

func tcAt(name string, at time.Time) *models.TestCase {
	return &models.TestCase{Name: name, HTTPReq: models.HTTPReq{Timestamp: at}}
}

// The agent's parsers run behind the traffic: at the stop, a busy recording
// still has mocks queued there (1,655 of 3,300 tests lost them at 110 req/s,
// when the CLI stopped reading at the stop and gave its stream 30 s). They
// must all reach the sink, however long that takes, as long as they keep
// coming.
func TestGetTestAndMockChans_DrainsQueuedMocksAtStop(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarness(t)
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range h.frames.Outgoing {
			got = append(got, m.Name)
		}
	}()

	captured := time.Now()
	h.stop()
	// Arriving for longer than a stream may stay quiet at the stop, spaced
	// well inside it: a sink still draining.
	const n = 40
	for i := 0; i < n; i++ {
		select {
		case h.f.outgoing <- mockAt(fmt.Sprintf("mock-%d", i), captured):
		case <-time.After(5 * time.Second):
			t.Fatalf("mock %d, captured before the stop, was not taken: the sink was cut off while it was still draining", i)
		}
		time.Sleep(mappingIdleGrace / 20)
	}
	select {
	case <-done:
	case <-time.After(mappingIdleGrace + 5*time.Second):
		t.Fatal("the mock drain did not end once nothing queued at the stop was left")
	}
	if len(got) != n {
		t.Fatalf("persisted %d of %d mocks queued at the stop", len(got), n)
	}
}

// The same for test cases.
func TestGetTestAndMockChans_DrainsQueuedTestCasesAtStop(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarness(t)
	var got int
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range h.frames.Incoming {
			got++
		}
	}()
	captured := time.Now()
	h.stop()
	const n = 40
	for i := 0; i < n; i++ {
		select {
		case h.f.incoming <- tcAt(fmt.Sprintf("test-%d", i), captured):
		case <-time.After(5 * time.Second):
			t.Fatalf("test case %d, captured before the stop, was not taken", i)
		}
		time.Sleep(mappingIdleGrace / 20)
	}
	select {
	case <-done:
	case <-time.After(mappingIdleGrace + 5*time.Second):
		t.Fatal("the test-case drain did not end")
	}
	if got != n {
		t.Fatalf("persisted %d of %d test cases queued at the stop", got, n)
	}
}

// Traffic the app keeps sending after the stop is not what was queued at it:
// it is persisted while the drain lasts, but cannot keep the drain going, or a
// busy app would never let the recording stop.
func TestGetTestAndMockChans_TrafficAfterTheStopDoesNotHoldTheDrain(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarness(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range h.frames.Outgoing { //nolint:revive // draining
		}
	}()
	h.stop()
	stopped := time.Now()
	for i := 0; ; i++ {
		select {
		case <-done:
			if took := time.Since(stopped); took > 3*mappingIdleGrace {
				t.Fatalf("the drain took %v with nothing queued at the stop", took)
			}
			return
		case h.f.outgoing <- mockAt(fmt.Sprintf("late-%d", i), time.Now().Add(time.Millisecond)):
		case <-time.After(10 * mappingIdleGrace):
			t.Fatal("the drain never ended while traffic after the stop kept coming")
		}
		time.Sleep(mappingIdleGrace / 10)
	}
}

// Mappings come last: the agent emits a test's mapping once its mocks are in.
// While the mocks queued at the stop are still draining, the mapping stream
// must stay open however quiet it is, or the mappings of the tests whose mocks
// were drained are lost (replay then finds no mocks for them).
func TestGetTestAndMockChans_MappingDrainWaitsForTheMockDrain(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarness(t)
	var mapped []string
	done := make(chan struct{})
	go func() {
		for range h.frames.Outgoing { //nolint:revive // draining
		}
	}()
	go func() {
		defer close(done)
		for m := range h.frames.Mappings {
			mapped = append(mapped, m.TestName)
		}
	}()
	captured := time.Now()
	h.stop()
	// Mocks drain for longer than the mapping stream's own idle bound.
	for deadline := time.Now().Add(mappingIdleGrace + time.Second); time.Now().Before(deadline); {
		select {
		case h.f.outgoing <- mockAt("m", captured):
		case <-time.After(5 * time.Second):
			t.Fatal("a mock queued at the stop was not taken")
		}
		time.Sleep(50 * time.Millisecond)
	}
	select {
	case h.f.mappings <- models.TestMockMapping{TestName: "test-1", MockIDs: []string{"m"}}:
	case <-time.After(5 * time.Second):
		t.Fatal("the mapping of a test whose mocks were still draining was not taken")
	}
	select {
	case <-done:
	case <-time.After(3*mappingIdleGrace + 5*time.Second):
		t.Fatal("the mapping drain did not end")
	}
	if len(mapped) != 1 {
		t.Fatalf("mappings = %v", mapped)
	}
}

// composeInstr runs the "app" under docker compose: the agent lives in the
// app's stack, so Run's context ending takes the agent down, and the fake agent
// stops handing over frames from then on.
type composeInstr struct {
	*fakeInstr
	stackDown chan struct{}
}

func (c *composeInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	close(c.stackDown)
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

// Under docker compose the agent is a service of the app's stack. The stop must
// drain what it holds before taking the stack down: taking it down first lost
// the mocks of 1,655 of 3,300 tests at 110 req/s.
func TestStart_ComposeDrainsTheAgentBeforeTakingItsStackDown(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &composeInstr{fakeInstr: f, stackDown: make(chan struct{})}
	mockDB := &recMockDB{unencodable: map[string]bool{}}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // the compose agent's health check
	}))
	defer agent.Close()
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	cfg.Agent.AgentURI = agent.URL
	r := &Recorder{
		logger:          zap.NewNop(),
		testDB:          &recTestDB{},
		mockDB:          mockDB,
		mappingDb:       &recMappingDB{},
		telemetry:       &recTelemetry{},
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          cfg,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	captured := time.Now()
	send := func(name string) bool {
		select {
		case f.outgoing <- mockAt(name, captured):
			return true
		case <-instr.stackDown:
			return false
		case <-time.After(30 * time.Second):
			t.Fatalf("mock %s was never taken", name)
			return false
		}
	}
	if !send("before-stop") {
		t.Fatal("the stack went down before the stop")
	}
	cancel()
	// The agent still holds mocks captured before the stop: they must reach the
	// sink while the stack, and the agent in it, is still up.
	const queued = 20
	for i := 0; i < queued; i++ {
		if !send(fmt.Sprintf("queued-%d", i)) {
			t.Fatalf("the stack (and the agent) went down with %d of %d queued mocks still to hand over", queued-i, queued)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Start did not return")
	}
	mockDB.mu.Lock()
	defer mockDB.mu.Unlock()
	if len(mockDB.inserted) != queued+1 {
		t.Fatalf("persisted %d of %d mocks", len(mockDB.inserted), queued+1)
	}
}

// A test case the app served after the stop is not recorded: the drain may end
// before its mocks arrive, and saved without them it would fail replay.
func TestGetTestAndMockChans_TestCasesServedAfterTheStopAreNotRecorded(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarnessFor(t, utils.DockerCompose)
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for tc := range h.frames.Incoming {
			got = append(got, tc.Name)
		}
	}()
	before := time.Now()
	h.stop()
	for _, tc := range []*models.TestCase{tcAt("before", before), tcAt("after", time.Now().Add(time.Second))} {
		select {
		case h.f.incoming <- tc:
		case <-time.After(5 * time.Second):
			t.Fatalf("test case %q was not taken", tc.Name)
		}
	}
	select {
	case <-done:
	case <-time.After(mappingIdleGrace + 5*time.Second):
		t.Fatal("the drain did not end")
	}
	if len(got) != 1 || got[0] != "before" {
		t.Fatalf("recorded %v, want only the test case served before the stop", got)
	}
}

// Undated frames (a gRPC test case carries no HTTP timestamps) say nothing
// about a backlog: undated traffic after the stop must not hold the drain open.
func TestGetTestAndMockChans_UndatedTrafficDoesNotHoldTheDrain(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarness(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range h.frames.Incoming { //nolint:revive // draining
		}
	}()
	h.stop()
	stopped := time.Now()
	for i := 0; ; i++ {
		select {
		case <-done:
			if took := time.Since(stopped); took > 3*mappingIdleGrace {
				t.Fatalf("the drain took %v", took)
			}
			return
		case h.f.incoming <- &models.TestCase{Name: fmt.Sprintf("grpc-%d", i)}:
		case <-time.After(10 * mappingIdleGrace):
			t.Fatal("the drain never ended while undated traffic kept coming")
		}
		time.Sleep(mappingIdleGrace / 10)
	}
}

// What the app does after the stop is not recorded: neither its test cases,
// nor their mocks, nor their mappings (orphans of test cases that do not
// exist).
func TestGetTestAndMockChans_NothingOfTrafficAfterTheStopIsRecorded(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarnessFor(t, utils.DockerCompose)
	var tcs, mocks, maps []string
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for tc := range h.frames.Incoming {
			tcs = append(tcs, tc.Name)
		}
	}()
	go func() {
		defer wg.Done()
		for m := range h.frames.Outgoing {
			mocks = append(mocks, m.Name)
		}
	}()
	go func() {
		defer wg.Done()
		for m := range h.frames.Mappings {
			maps = append(maps, m.TestName)
		}
	}()
	before := time.Now()
	h.stop()
	after := time.Now().Add(time.Second)
	send := func(ch any, v any) {
		t.Helper()
		var ok bool
		switch c := ch.(type) {
		case chan *models.TestCase:
			select {
			case c <- v.(*models.TestCase):
				ok = true
			case <-time.After(5 * time.Second):
			}
		case chan *models.Mock:
			select {
			case c <- v.(*models.Mock):
				ok = true
			case <-time.After(5 * time.Second):
			}
		case chan models.TestMockMapping:
			select {
			case c <- v.(models.TestMockMapping):
				ok = true
			case <-time.After(5 * time.Second):
			}
		}
		if !ok {
			t.Fatalf("%v was not taken", v)
		}
	}
	send(h.f.incoming, tcAt("t-before", before))
	send(h.f.outgoing, mockAt("m-before", before))
	send(h.f.incoming, tcAt("t-after", after))
	send(h.f.incoming, &models.TestCase{Name: "t-undated"}) // gRPC: cannot be told
	send(h.f.outgoing, mockAt("m-after", after))
	// The frame drains have taken what was queued; the mappings trail them.
	time.Sleep(100 * time.Millisecond)
	send(h.f.mappings, models.TestMockMapping{TestName: "t-before", MockIDs: []string{"m-before"}})
	send(h.f.mappings, models.TestMockMapping{TestName: "t-after", MockIDs: []string{"m-after"}})
	// The test-case drain ends; the agent then emits the mapping of a test
	// case the drain never read.
	time.Sleep(mappingIdleGrace + 500*time.Millisecond)
	send(h.f.mappings, models.TestMockMapping{TestName: "t-never-read", MockIDs: []string{"m-x"}})
	wg.Wait()
	if fmt.Sprint(tcs, mocks, maps) != "[t-before] [m-before] [t-before]" {
		t.Fatalf("recorded test cases %v, mocks %v, mappings %v; want only what was served before the stop", tcs, mocks, maps)
	}
}

func TestTestCaseCapturedAtIsWhenItWasComplete(t *testing.T) {
	t.Parallel()
	req, resp := time.Unix(100, 0), time.Unix(101, 0)
	if got := testCaseCapturedAt(&models.TestCase{HTTPReq: models.HTTPReq{Timestamp: req}, HTTPResp: models.HTTPResp{Timestamp: resp}}); !got.Equal(resp) {
		t.Fatalf("got %v, want the response %v", got, resp)
	}
	// Created is when the agent built it, possibly long after it ran.
	if got := testCaseCapturedAt(&models.TestCase{Created: 100}); !got.IsZero() {
		t.Fatalf("got %v for a gRPC test case, want it undated", got)
	}
}

// ctxInstr hands out the context its incoming stream was opened on.
type ctxInstr struct {
	*fakeInstr
	incomingCtx chan context.Context
}

func (c *ctxInstr) GetIncoming(ctx context.Context, _ models.IncomingOptions) (<-chan *models.TestCase, error) {
	c.incomingCtx <- ctx
	return c.incoming, nil
}

// What the stream's reader hands over after the drain has ended it is filtered
// like anything else: a test case served after the stop is not recorded.
func TestGetTestAndMockChans_TrafficAfterTheStopIsFilteredToTheEnd(t *testing.T) {
	t.Parallel()
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	instr := &ctxInstr{fakeInstr: f, incomingCtx: make(chan context.Context, 1)}
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	r := &Recorder{logger: zap.NewNop(), instrumentation: instr, config: cfg, frameQuiet: 50 * time.Millisecond}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	ctx = context.WithValue(ctx, models.ErrGroupKey, g)
	frames, err := r.GetTestAndMockChans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer frames.Abandon()
	streamCtx := <-instr.incomingCtx
	var got []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		for tc := range frames.Incoming {
			got = append(got, tc.Name)
		}
	}()
	cancel()
	<-streamCtx.Done() // the drain has ended the stream
	select {
	case f.incoming <- tcAt("served-after", time.Now().Add(time.Second)):
	case <-time.After(5 * time.Second):
		t.Fatal("the reader's last test case was not taken")
	}
	close(f.incoming)
	<-done
	if len(got) != 0 {
		t.Fatalf("recorded %v after the drain ended the stream", got)
	}
}
