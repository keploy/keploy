package record

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/errgroup"
)

// mappingDrain runs a docker compose recording's frame forwarders (the app
// serves on through the stop, as a workload k8s-proxy records does), reads
// every stream as a consumer would, and reports when the mapping stream
// closed, and what it handed on.
type mappingDrain struct {
	h      *stopDrainHarness
	mu     sync.Mutex
	mapped []string
	closed chan struct{}
}

func newMappingDrain(t *testing.T) *mappingDrain {
	t.Helper()
	d := &mappingDrain{h: newStopDrainHarnessFor(t, utils.DockerCompose), closed: make(chan struct{})}
	go func() {
		for range d.h.frames.Incoming { //nolint:revive // draining
		}
	}()
	go func() {
		for range d.h.frames.Outgoing { //nolint:revive // draining
		}
	}()
	go func() {
		defer close(d.closed)
		for m := range d.h.frames.Mappings {
			d.mu.Lock()
			d.mapped = append(d.mapped, m.TestName)
			d.mu.Unlock()
		}
	}()
	return d
}

func (d *mappingDrain) send(t *testing.T, m models.TestMockMapping) {
	t.Helper()
	select {
	case d.h.f.mappings <- m:
	case <-time.After(5 * time.Second):
		t.Fatalf("the mapping of %q was not taken", m.TestName)
	}
}

// serveOn sends the mapping each new request makes until the mapping stream
// closes or for lasts, as an app that goes on serving after the stop does,
// and reports whether the stream closed while it still served.
func (d *mappingDrain) serveOn(t *testing.T, for_ time.Duration, mapping func(i int) models.TestMockMapping) bool {
	t.Helper()
	until := time.Now().Add(for_)
	for i := 0; time.Now().Before(until); i++ {
		select {
		case <-d.closed:
			return true
		case d.h.f.mappings <- mapping(i):
		case <-time.After(5 * time.Second):
			t.Fatal("a mapping of the traffic after the stop was neither taken nor the stream closed")
		}
		select {
		case <-d.closed:
			return true
		case <-time.After(mappingIdleGrace / 10):
		}
	}
	return false
}

func (d *mappingDrain) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.mapped...)
}

// An app that goes on serving after the stop must not hold the recording's
// save open. Its requests are async captures with no test name, as k8s-proxy's
// sidecar agent sends them, and the agent sends a mapping for each one, about
// two a second: the recorder took every unnamed mapping for one of a test case
// it recorded (an unnamed test case is keyed ""), so each one restarted the
// mapping drain's quiet bound, and the drain did not end while the app served.
// The undated mappings of an older agent included.
func TestGetTestAndMockChans_AnAppServingOnAfterTheStopDoesNotHoldTheMappingDrain(t *testing.T) {
	t.Parallel()
	d := newMappingDrain(t)
	before := time.Now()
	select {
	case d.h.f.incoming <- &models.TestCase{HTTPReq: models.HTTPReq{Timestamp: before}}: // unnamed, as an async capture
	case <-time.After(5 * time.Second):
		t.Fatal("the test case was not taken")
	}
	d.send(t, models.TestMockMapping{MockIDs: []string{"m-before"}})
	d.h.stop()
	stopped := time.Now()
	ended := d.serveOn(t, 4*mappingIdleGrace, func(i int) models.TestMockMapping {
		return models.TestMockMapping{MockIDs: []string{fmt.Sprintf("m-after-%d", i)}}
	})
	if !ended {
		t.Fatalf("the mapping drain was still open %v after the stop, while the app served on: the traffic after the stop held the mapping drain open", time.Since(stopped).Round(time.Second))
	}
	// The test-case and mock drains end on their own quiet, and the mapping
	// drain on its own after them.
	if took := time.Since(stopped); took > 3*mappingIdleGrace {
		t.Fatalf("the mapping drain took %v to end after the stop", took)
	}
}

// The agent dates each mapping by the request it maps (RequestedAt): one of
// a request made after the stop is left out at once, whatever test it names
// -- one a test case recorded before the stop also has, as a request whose
// test name is its own header does -- and never holds the drain; one of a
// request made before the stop is recorded.
func TestGetTestAndMockChans_AMappingDatedAfterTheStopIsLeftOut(t *testing.T) {
	t.Parallel()
	d := newMappingDrain(t)
	before := time.Now()
	select {
	case d.h.f.incoming <- tcAt("named-by-header", before):
	case <-time.After(5 * time.Second):
		t.Fatal("the test case was not taken")
	}
	d.h.stop()
	stopped := time.Now()
	d.send(t, models.TestMockMapping{TestName: "named-by-header", MockIDs: []string{"m-before"}, RequestedAt: before})
	ended := d.serveOn(t, 4*mappingIdleGrace, func(i int) models.TestMockMapping {
		return models.TestMockMapping{TestName: "named-by-header", MockIDs: []string{fmt.Sprintf("m-after-%d", i)},
			RequestedAt: time.Now()}
	})
	if !ended {
		t.Fatalf("the mapping drain was still open %v after the stop: requests after it held the mapping drain open", time.Since(stopped).Round(time.Second))
	}
	assert.Equal(t, []string{"named-by-header"}, d.got(), "only the mapping of the request before the stop is recorded")
}

// The stop judges test cases, mocks and mappings by one cut-off: the first of
// the three drains to see the stop fixes it, and the others read the same.
func TestStopCut_OneCutOffForEveryDrain(t *testing.T) {
	t.Parallel()
	var c stopCut
	first := c.at()
	time.Sleep(time.Millisecond)
	assert.Equal(t, first, c.at(), "a later look moved the stop's cut-off")
	var none *stopCut
	assert.False(t, none.at().Before(first), "a nil cut is now")
	assert.True(t, mappingAfterStop(models.TestMockMapping{RequestedAt: first.Add(time.Nanosecond)}, first))
	assert.False(t, mappingAfterStop(models.TestMockMapping{RequestedAt: first}, first), "a request at the cut-off is before the stop, as a test case completed at it is")
	assert.False(t, mappingAfterStop(models.TestMockMapping{}, first), "an undated mapping (an older agent) is judged by its test, not as after the stop")
}

// A mapping the agent names mocks in that the recording does not hold (the
// mock stream ended without them) is said once, at the stop, counted by why:
// not one ERROR with a stack per mock (71,061 log lines for 6,457 entries in
// 38 s pushed everything else out of k8s-proxy's log), and with no test name
// (the agent's can be a request's own header).
func TestStart_MocksAMappingNamesAndTheStreamNeverDeliveredAreSaidOnce(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}}, BaseRecordHooks{}, zap.New(core))
	run.test(t, "test-1")
	ids := make([]string, 96)
	for i := range ids {
		ids[i] = fmt.Sprintf("never-sent-%d", i)
	}
	run.mapping(t, "test-1", ids...)
	run.until(t, "test-1's entries held", heldIs(len(ids)))
	run.stop(t)

	for _, e := range logs.All() {
		if _, perEntry := e.ContextMap()["tempMockID"]; perEntry && e.Level > zapcore.DebugLevel {
			t.Fatalf("a mapping entry was logged one by one above debug: %s %q", e.Level, e.Message)
		}
		if _, named := e.ContextMap()["test"]; named && e.ContextMap()["tempMockID"] != nil {
			t.Fatalf("an entry's line names its test: %q", e.Message)
		}
	}
	summary := logs.FilterMessage(UnmappedSummaryLog).All()
	require.Len(t, summary, 1, "the unmapped mocks must be said once, at the stop")
	assert.Equal(t, zapcore.WarnLevel, summary[0].Level)
	fields := summary[0].ContextMap()
	assert.EqualValues(t, len(ids), fields["mappingEntries"])
	assert.EqualValues(t, len(ids), fields[lossReasonName(reasonEnded)])
	assert.NotContains(t, fields, "test")
	assert.Len(t, logs.FilterMessage(unmappedEntryLog).All(), len(ids), "each entry is still at debug, with why")
}

// A frame drain judges by the stop's one cut-off: when the mapping drain saw
// the stop first, a test case completed after that instant is after the stop
// for the test-case drain too, though that drain saw the stop later. Judged
// by each drain's own look, it would be recorded while its mapping, dated
// after the cut-off, was left out: a test case saved with no mocks.
func TestForwardUntilDrained_JudgesByTheStopsOneCutOff(t *testing.T) {
	t.Parallel()
	cut := &stopCut{}
	first := cut.at() // another drain saw the stop first
	time.Sleep(20 * time.Millisecond)
	r := &Recorder{logger: zap.NewNop(), frameQuiet: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	stream := make(chan *models.TestCase, 1)
	stream <- tcAt("between", first.Add(10*time.Millisecond))
	cancel() // this drain sees the stop now, after the cut-off
	out := make(chan *models.TestCase, 1)
	var afterStop []string
	forwardUntilDrained(ctx, r, stream, out, make(chan struct{}), drainOpts[*models.TestCase]{
		capturedAt:  testCaseCapturedAt,
		dropped:     func(*models.TestCase) {},
		closeStream: func() { close(stream) },
		afterStop:   func(tc *models.TestCase) { afterStop = append(afterStop, tc.Name) },
		kind:        "test cases",
		cut:         cut,
	})
	if len(out) > 0 || fmt.Sprint(afterStop) != "[between]" {
		t.Fatalf("a test case completed after the stop's cut-off was forwarded (%d) or not judged after it (%v)", len(out), afterStop)
	}
}

// The counts a stop says are its recording's own: said once, then forgotten,
// so a recorder started again counts only its own.
func TestUnmappedMocks_CountByWhyAndForgetOnceSaid(t *testing.T) {
	var u unmappedMocks
	u.add(reasonEnded)
	u.add(reasonEnded)
	u.add(reasonTaken)
	total, by := u.take()
	assert.Equal(t, 3, total)
	assert.Equal(t, map[string]int{lossReasonName(reasonEnded): 2, lossReasonName(reasonTaken): 1}, by)
	total, _ = u.take()
	assert.Zero(t, total, "a stop's counts were said twice")
}

// The mapping drain judges a mapping by the cut-off the frame drains fixed as
// the stop came, not by when the first mapping after the stop reached it: a
// request made between the stop and that first mapping is after the stop.
func TestGetTestAndMockChans_AMappingDatedBeforeTheDrainLookedIsStillAfterTheStop(t *testing.T) {
	t.Parallel()
	d := newMappingDrain(t)
	before := time.Now()
	select {
	case d.h.f.incoming <- tcAt("t-1", before):
	case <-time.After(5 * time.Second):
		t.Fatal("the test case was not taken")
	}
	d.h.stop()
	stopped := time.Now()
	time.Sleep(100 * time.Millisecond) // the mapping drain has not looked yet
	d.send(t, models.TestMockMapping{TestName: "t-1", MockIDs: []string{"m-after"}, RequestedAt: stopped.Add(50 * time.Millisecond)})
	d.send(t, models.TestMockMapping{TestName: "t-1", MockIDs: []string{"m-before"}, RequestedAt: before})
	select {
	case <-d.closed:
	case <-time.After(4 * mappingIdleGrace):
		t.Fatal("the mapping drain did not end")
	}
	assert.Equal(t, []string{"t-1"}, d.got(), "a mapping of a request made after the stop was recorded, or the one before it was not")
}

// A frame drain busy handing a frame to a consumer still saving the last one
// learns of the stop only once it is free: it judges what it takes next by
// the stop's cut-off, which another drain fixed as the stop came, not by when
// it looked. Judged by its own late look, a test case completed after the
// stop would be recorded while its mapping, dated after the cut-off, was left
// out: a test case saved with no mocks.
func TestGetTestAndMockChans_ADrainThatSeesTheStopLateJudgesByItsCutOff(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarnessFor(t, utils.DockerCompose)
	go func() {
		for range h.frames.Outgoing { //nolint:revive // draining
		}
	}()
	go func() {
		for range h.frames.Mappings { //nolint:revive // draining
		}
	}()
	send := func(tc *models.TestCase) {
		select {
		case h.f.incoming <- tc:
		case <-time.After(5 * time.Second):
			t.Errorf("%s was not taken", tc.Name)
		}
	}
	before := time.Now()
	send(tcAt("t-0", before))         // fills the hand-over's one slot
	send(tcAt("t-1", before))         // the drain now waits for the consumer with it
	time.Sleep(50 * time.Millisecond) // and is in that wait when the stop comes
	h.stop()
	stopped := time.Now()
	time.Sleep(100 * time.Millisecond) // the other drains have seen the stop
	late := make(chan struct{})
	go func() { defer close(late); send(tcAt("t-after", stopped.Add(50*time.Millisecond))) }()
	time.Sleep(50 * time.Millisecond)
	var got []string
	for tc := range h.frames.Incoming {
		got = append(got, tc.Name)
	}
	<-late
	assert.Equal(t, []string{"t-0", "t-1"}, got, "a test case completed after the stop was recorded")
}

// The stop's cut-off is the instant the stop came, even when every drain is
// busy handing a frame to a consumer that is still persisting the last one:
// before, the cut was fixed by the first drain to look at the stop, which
// such a drain did only once its consumer took the frame, so a request made
// in the meantime, after the stop, was recorded (a test case, and the mapping
// of one).
func TestGetTestAndMockChans_TheCutOffIsTheStopsWhenEveryDrainIsBusy(t *testing.T) {
	t.Parallel()
	h := newStopDrainHarnessFor(t, utils.DockerCompose)
	before := time.Now()
	hand := func(what string, send func() bool) {
		t.Helper()
		if !send() {
			t.Fatalf("%s was not taken", what)
		}
	}
	within := func() <-chan time.Time { return time.After(5 * time.Second) }
	// Each drain holds one frame in its hand-over and waits for its consumer
	// with a second when the stop comes.
	for i := 0; i < 2; i++ {
		hand("a test case", func() bool {
			select {
			case h.f.incoming <- tcAt(fmt.Sprintf("t-%d", i), before):
				return true
			case <-within():
				return false
			}
		})
		hand("a mock", func() bool {
			select {
			case h.f.outgoing <- mockAt(fmt.Sprintf("m-%d", i), before):
				return true
			case <-within():
				return false
			}
		})
	}
	// The mapping drain hands its one frame straight to its consumer, and
	// waits for it there.
	hand("a mapping", func() bool {
		select {
		case h.f.mappings <- models.TestMockMapping{TestName: "t-0", MockIDs: []string{"m-0"}, RequestedAt: before}:
			return true
		case <-within():
			return false
		}
	})
	time.Sleep(50 * time.Millisecond) // every drain is in its wait
	h.stop()
	stopped := time.Now()
	time.Sleep(150 * time.Millisecond) // the consumers are still persisting
	after := stopped.Add(50 * time.Millisecond)
	var tests, maps []string
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for tc := range h.frames.Incoming {
			tests = append(tests, tc.Name)
		}
	}()
	go func() {
		defer wg.Done()
		for range h.frames.Outgoing { //nolint:revive // draining
		}
	}()
	go func() {
		defer wg.Done()
		for m := range h.frames.Mappings {
			maps = append(maps, m.TestName)
		}
	}()
	// A request served after the stop, made while the drains were busy.
	select {
	case h.f.incoming <- tcAt("t-after", after):
	case <-within():
	}
	select {
	case h.f.mappings <- models.TestMockMapping{TestName: "t-after", MockIDs: []string{"m-after"}, RequestedAt: after}:
	case <-within():
	}
	wg.Wait()
	assert.Equal(t, []string{"t-0", "t-1"}, tests, "a test case of a request made after the stop was recorded")
	assert.Equal(t, []string{"t-0"}, maps, "the mapping of a request made after the stop was recorded")
}

// A recording whose every mapping entry found its mock says nothing of
// unmapped mocks at its stop.
func TestStart_NoUnmappedMocksNoSummary(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}}, BaseRecordHooks{}, zap.New(core))
	run.test(t, "test-1")
	run.mock(t, "m-1")
	run.mapping(t, "test-1", "m-1")
	run.persisted(t, "test-1")
	run.stop(t)
	assert.Empty(t, logs.FilterMessage(UnmappedSummaryLog).All(), "a summary of unmapped mocks with none")
}

// Each reason the stop's summary counts by has a name of its own: two that
// shared one would be added together under it.
func TestLossReasonName_EachReasonHasItsOwnName(t *testing.T) {
	seen := map[string]lossReason{}
	for _, why := range []lossReason{reasonEnded, reasonTaken, reasonUntrackedDrop, reasonCapFull, reasonRevoked, reasonAbandoned, reasonConsumerGivenUp} {
		name := lossReasonName(why)
		if prev, dup := seen[name]; dup {
			t.Fatalf("reasons %d and %d share the name %q", prev, why, name)
		}
		seen[name] = why
	}
}

// The mapping drain says, at debug, how many mappings of requests made after
// the stop it left out: a count, with no test name.
func TestGetTestAndMockChans_SaysHowManyMappingsAfterTheStopItLeftOut(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.DebugLevel)
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose)
	r := &Recorder{logger: zap.New(core), instrumentation: f, config: cfg}
	g, gctx := errgroup.WithContext(context.Background())
	ctx, cancel := context.WithCancel(gctx)
	frames, err := r.GetTestAndMockChans(context.WithValue(ctx, models.ErrGroupKey, g))
	require.NoError(t, err)
	t.Cleanup(func() { cancel(); frames.Abandon() })
	go func() {
		for range frames.Incoming { //nolint:revive // draining
		}
	}()
	go func() {
		for range frames.Outgoing { //nolint:revive // draining
		}
	}()
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range frames.Mappings { //nolint:revive // draining
		}
	}()
	cancel()
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 3; i++ {
		select {
		case f.mappings <- models.TestMockMapping{TestName: "by-header", MockIDs: []string{"m"}, RequestedAt: time.Now()}:
		case <-time.After(5 * time.Second):
			t.Fatal("a mapping was not taken")
		}
	}
	select {
	case <-closed:
	case <-time.After(4 * mappingIdleGrace):
		t.Fatal("the mapping drain did not end")
	}
	said := logs.FilterMessage("left out the mappings of requests the app served after the stop").All()
	require.Len(t, said, 1)
	assert.EqualValues(t, 3, said[0].ContextMap()["mappings"])
	assert.NotContains(t, said[0].ContextMap(), "test")
}

// liveWorkloadInstr is the agent of a workload that goes on serving after the
// recording's stop, as a k8s-proxy sidecar recording's pod does: its "app"
// runs until the recorder lets it go, and nothing it does takes the agent down.
// It hands the test the context agentStopWatcher is given, the parent of the
// one the recorder opens its frame streams under: its end is the stop as the
// drains see it (they read it as they take each frame).
type liveWorkloadInstr struct {
	*fakeInstr
	streams chan context.Context
}

func (l *liveWorkloadInstr) Run(ctx context.Context, _ models.RunOptions) models.AppError {
	<-ctx.Done()
	return models.AppError{AppErrorType: models.ErrCtxCanceled}
}

func (l *liveWorkloadInstr) WatchAgentStop(ctx context.Context) <-chan struct{} {
	select {
	case l.streams <- ctx:
	default: // the first one is the recording's
	}
	return make(chan struct{}) // the agent never says it is stopping
}

// A workload that serves on after the stop (a k8s-proxy sidecar recording,
// which records as docker compose) has its agent go on sending each request's
// mocks and mapping. The stop's drains leave out those mocks and the request's
// test case. An agent that does not date its mappings (RequestedAt) sends the
// mapping undated, and for an async capture unnamed, so the mapping drain
// hands it on. Each of its entries then waited for a mock the recording had
// left out itself and was settled at the end as a lost mock: a "Failed to
// correlate mock mapping" ERROR per Redis or MySQL call of every request after
// the stop on older releases, and a stop summary saying those tests are saved
// without their mocks on newer ones. No test is: the requests are not
// recorded. A mock the agent never sent at all is still a loss, and is still
// said.
func TestStart_AMappingOfARequestServedAfterTheStopNamesNoLostMock(t *testing.T) {
	f := &fakeInstr{
		mappings: make(chan models.TestMockMapping),
		incoming: make(chan *models.TestCase),
		outgoing: make(chan *models.Mock),
	}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // the sidecar agent's health check
	}))
	defer agent.Close()
	cfg := &config.Config{}
	cfg.CommandType = string(utils.DockerCompose) // as k8s-proxy records a sidecar
	cfg.Agent.AgentURI = agent.URL
	core, logs := observer.New(zapcore.DebugLevel)
	instr := &liveWorkloadInstr{fakeInstr: f, streams: make(chan context.Context, 1)}
	waitsMade := make(chan *mappingWaits, 1)
	testDB := &recTestDB{}
	mockDB := &recMockDB{unencodable: map[string]bool{}}
	mappingDB := &recMappingDB{}
	tele := &recTelemetry{}
	r := &Recorder{
		logger:          zap.New(core),
		testDB:          testDB,
		mockDB:          mockDB,
		mappingDb:       mappingDB,
		telemetry:       tele,
		instrumentation: instr,
		testSetConf:     recTestSetConf{},
		hooks:           BaseRecordHooks{},
		config:          cfg,
		// The stop's drains stay open this long after their last frame from
		// before the stop, however loaded the host: every frame this row
		// sends after the stop is taken well inside it.
		frameQuiet:       5 * time.Second,
		mappingWaitsMade: func(w *mappingWaits) { waitsMade <- w },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Start(ctx) }()

	give := func(what string, send func() bool) {
		t.Helper()
		if !send() {
			t.Fatalf("%s was not taken", what)
		}
	}
	mock := func(name string, at time.Time) func() bool {
		return func() bool {
			select {
			case f.outgoing <- mockAt(name, at):
				return true
			case <-time.After(30 * time.Second):
				return false
			}
		}
	}
	mapping := func(m models.TestMockMapping) func() bool {
		return func() bool {
			select {
			case f.mappings <- m:
				return true
			case <-time.After(30 * time.Second):
				return false
			}
		}
	}
	testCase := func(tc *models.TestCase) func() bool {
		return func() bool {
			select {
			case f.incoming <- tc:
				return true
			case <-time.After(30 * time.Second):
				return false
			}
		}
	}

	// Before the stop: a recorded request, its Redis and MySQL calls, and
	// its mapping, which also names a mock the agent never sends.
	before := time.Now()
	give("redis-pre", mock("redis-pre", before))
	give("mysql-pre", mock("mysql-pre", before))
	give("test-1", testCase(tcAt("test-1", before)))
	give("test-1's mapping", mapping(models.TestMockMapping{TestName: "test-1", MockIDs: []string{"redis-pre", "mysql-pre", "never-sent"}}))
	waitForPersistedTestCase(t, testDB, "test-1")

	var waits *mappingWaits
	select {
	case waits = <-waitsMade:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the recorder never made its mapping waits")
	}
	var streamCtx context.Context
	select {
	case streamCtx = <-instr.streams:
	case <-time.After(time.Minute): // only turns a hang into a failure
		t.Fatal("the recorder never opened its frame streams")
	}
	held := func(want int, what string) {
		t.Helper()
		deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
		for waits.heldCount() != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d mapping entries held, want %d", what, waits.heldCount(), want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// test-1's mocks are saved, so the one entry held from here on is its
	// never-sent mock's.
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for {
		mockDB.mu.Lock()
		n := len(mockDB.inserted)
		mockDB.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test-1's mocks were never saved")
		}
		time.Sleep(time.Millisecond)
	}
	held(1, "test-1's never-sent mock")

	cancel() // the stop; the workload serves on

	// Traffic after the stop is judged so only once the drains have seen the
	// stop: one they take before that is the recording's, as it should be. So
	// wait for the stop to reach them (the end of their context's parent), and
	// for the test-case drain to leave out a test case served after it.
	<-streamCtx.Done()
	// Dated well past the stop's cut-off, which is fixed at that end.
	after := time.Now().Add(time.Minute)
	give("request a's test case", testCase(&models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: after}}))
	waitForLog(t, logs, "not recording a test case served after the stop")

	// A request after the stop, as the agent sends it: its mocks, then its
	// undated, unnamed mapping.
	give("redis-post-a", mock("redis-post-a", after))
	give("mysql-post-a", mock("mysql-post-a", after))
	give("request a's mapping", mapping(models.TestMockMapping{MockIDs: []string{"redis-post-a", "mysql-post-a"}}))
	// Another, whose mapping reaches the recording before its mock (the two
	// come on separate streams): its entry waits, and is handed back as soon
	// as the drain leaves the mock out, long before the mock stream ends, so
	// the traffic after the stop does not fill the waits.
	give("request b's mapping", mapping(models.TestMockMapping{MockIDs: []string{"redis-post-b"}}))
	held(2, "request b's entry")
	give("redis-post-b", mock("redis-post-b", after))
	held(1, "request b's entry once its mock was left out")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Minute):
		t.Fatal("Start did not return")
	}

	mockDB.mu.Lock()
	saved := append([]string(nil), mockDB.inserted...)
	mockDB.mu.Unlock()
	assert.ElementsMatch(t, []string{"redis-pre", "mysql-pre"}, saved, "a mock of a request after the stop was saved")

	var unmapped []string
	for _, e := range logs.FilterMessage(unmappedEntryLog).All() {
		unmapped = append(unmapped, e.ContextMap()["tempMockID"].(string))
	}
	assert.Equal(t, []string{"never-sent"}, unmapped,
		"only the mock the agent never sent is missing from a recorded test; the mocks of requests served after the stop are left out with them")
	summary := logs.FilterMessage(UnmappedSummaryLog).All()
	require.Len(t, summary, 1, "the mock the agent never sent is still said at the stop")
	assert.EqualValues(t, 1, summary[0].ContextMap()["mappingEntries"], "%v", summary[0].ContextMap())

	// test-1 keeps both of its mocks.
	var got []string
	mappingDB.mu.Lock()
	for _, b := range mappingDB.batches {
		for _, e := range b["test-1"] {
			got = append(got, e.Name)
		}
	}
	mappingDB.mu.Unlock()
	assert.ElementsMatch(t, []string{"redis-pre", "mysql-pre"}, got)

	tele.mu.Lock()
	suite := tele.suite
	tele.mu.Unlock()
	require.NotNil(t, suite)
	assert.EqualValues(t, 1, suite["mocks-uncorrelated"], "%v", suite)
	assert.EqualValues(t, 1, suite["tests-short-pool"], "%v", suite)
}

// waitForLog waits for a line with msg (logs only grow).
func waitForLog(t *testing.T, logs *observer.ObservedLogs, msg string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute) // only turns a hang into a failure
	for logs.FilterMessage(msg).Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no %q line", msg)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A mock the stop's drain leaves out settles the entries that name it, the
// one already waiting at once, and its mark is freed when an entry consumes
// it. The marks alive at once are capped: past the cap a mock is not marked,
// so an entry naming it counts as lost, as before, and the cap is said once.
// A recording that maps nothing marks nothing.
func TestLeftOutMarker_SettlesItsEntriesUnderACapOfLiveMarks(t *testing.T) {
	old := maxLeftOutAtStopMarks
	maxLeftOutAtStopMarks = 1
	t.Cleanup(func() { maxLeftOutAtStopMarks = old })

	core, logs := observer.New(zapcore.DebugLevel)
	var asyncMockIDs, correlationMap sync.Map
	var dropped droppedMockSet
	waits := newMappingWaits()
	l := &leftOutMarker{asyncMockIDs: &asyncMockIDs, waits: waits, logger: zap.New(core)}
	settle := func(id string) mockFate {
		_, fate := settleMock(id, &correlationMap, &asyncMockIDs, &dropped)
		return fate
	}
	after := time.Now()

	// An entry already waiting for "a" is handed back as soon as it is left
	// out, and settles as no loss; its mark is then freed.
	c := waits.claim("", "a", func() (models.MockEntry, mockFate) {
		_, fate := settleMock("a", &correlationMap, &asyncMockIDs, &dropped)
		return models.MockEntry{}, fate
	})
	require.True(t, c.held)
	l.mark(mockAt("a", after))
	assert.Equal(t, 0, waits.heldCount(), "the waiting entry was not handed back")
	ready := waits.takeReady()
	require.Len(t, ready, 1)
	assert.Equal(t, fateExcluded, settle(ready[0].tempID))
	assert.EqualValues(t, 0, l.live.Load(), "a consumed mark is still counted")

	// "b" takes the one place; "c" and "d" are past the cap, said once.
	l.mark(mockAt("b", after))
	l.mark(mockAt("c", after))
	l.mark(mockAt("d", after))
	assert.Equal(t, 1, logs.FilterMessageSnippet("too many mocks of traffic after the stop").Len())
	assert.Equal(t, fateUnknown, settle("c"), "a mock past the cap must count as lost, as before")
	assert.Equal(t, fateExcluded, settle("b"))
	assert.Equal(t, fateUnknown, settle("b"), "a mark is consumed once")
	assert.EqualValues(t, 0, l.live.Load())
	// Its place is free again.
	l.mark(mockAt("e", after))
	assert.Equal(t, fateExcluded, settle("e"))

	off := &leftOutMarker{asyncMockIDs: &asyncMockIDs, waits: waits, logger: zap.NewNop(), off: true}
	off.mark(mockAt("f", after))
	_, marked := asyncMockIDs.Load("f")
	assert.False(t, marked, "a recording that maps nothing marked a mock no entry will consume")
}

// The cut-off is the instant the stop was made, however late it is first
// asked for: a request made after the stop, before the goroutine that fixes
// the cut runs, is dated after it.
func TestStopCut_IsTheInstantOfTheStop(t *testing.T) {
	ctx, stop := utils.WithStop(context.Background())
	cut := &stopCut{ctx: ctx}
	stop()
	stoppedBy := time.Now()
	time.Sleep(10 * time.Millisecond)
	requested := time.Now() // a request made after the stop, before the cut is asked for
	at := cut.at()
	if at.After(stoppedBy) {
		t.Fatalf("the cut-off is %v after the stop", at.Sub(stoppedBy))
	}
	if !mappingAfterStop(models.TestMockMapping{RequestedAt: requested}, at) {
		t.Fatal("a mapping of a request made after the stop is dated before its cut-off")
	}
	if again := cut.at(); !again.Equal(at) {
		t.Fatalf("the cut-off moved from %v to %v", at, again)
	}
	// A context cancelled with no stop's instant: the first ask fixes it.
	plain, cancel := context.WithCancel(context.Background())
	cancel()
	before := time.Now()
	if c := (&stopCut{ctx: plain}).at(); c.Before(before) {
		t.Fatalf("a cut-off of a context cancelled with no instant is %v before it was asked for", before.Sub(c))
	}
}
