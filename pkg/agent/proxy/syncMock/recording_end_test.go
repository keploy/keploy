package manager

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// perTestMock is a mock of a call the app made at at, one a test's window owns.
func perTestMock(at time.Time) *models.Mock {
	return &models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}}
}

// endedRecording is a manager recording through a mapping stream that nothing
// reads past its marker and a mock stream, with its log observed: a sidecar
// agent whose recording's client has read all it was owed.
func endedRecording(t *testing.T) (m *SyncMockManager, out chan *models.Mock, mappings chan models.TestMockMapping, endStream context.CancelFunc, logs *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	m = New(nil)
	m.SetLogger(zap.New(core))
	out = make(chan *models.Mock, 64)
	m.SetOutputChannel(out)
	m.SetFirstRequestSignaled()
	streamCtx, endStream := context.WithCancel(context.Background())
	t.Cleanup(endStream)
	mappings = make(chan models.TestMockMapping) // its client reads none
	m.SetMappingChannel(streamCtx, mappings)
	return m, out, mappings, endStream, logs
}

// Once its recording has ended, the agent captures nothing of the app's
// traffic: a call's mock is refused, as a DNS answer's is, and a request it
// resolves makes no mapping. Before, the agent captured on until it exited:
// each mapping queued for a stream whose client had gone, and was lost with
// one ERROR each ("mapping stream closed with mappings still queued"), 129 of
// them in the 120 s a sidecar's app served after its recording's stop.
func TestAnEndedRecordingTakesNothingTheAppDoesAfterIt(t *testing.T) {
	m, out, mappings, endStream, logs := endedRecording(t)
	m.EndRecording()
	endStream() // its stream's client read the marker and went

	if m.Recording() {
		t.Fatal("the manager still records once its recording has ended")
	}
	for i := 0; i < 5; i++ {
		at := time.Now()
		if d := m.AddMock(perTestMock(at.Add(time.Millisecond))); d != MockDroppedNoRecording {
			t.Fatalf("a call made after the recording ended was %q, want refused: %q", d, MockDroppedNoRecording)
		}
		m.ResolveRange(at, at.Add(5*time.Millisecond), "test-after", true, true)
	}
	if d := m.SendConfigMock(&models.Mock{Kind: models.DNS}); d != MockDroppedNoRecording {
		t.Fatalf("a DNS answer after the recording ended was %q, want refused", d)
	}
	time.Sleep(50 * time.Millisecond) // a drainer that would say its loss has had its time
	select {
	case got := <-mappings:
		t.Fatalf("a request served after the recording ended was mapped: %+v", got)
	default:
	}
	if len(out) != 0 {
		t.Fatalf("%d mocks reached the ended recording's mock stream", len(out))
	}
	for _, e := range logs.All() {
		if e.Level >= zapcore.WarnLevel {
			t.Fatalf("a refused capture was said at %s: %q", e.Level, e.Message)
		}
	}
	if n := m.ended.calls.Load(); n != 6 {
		t.Fatalf("%d calls counted as not captured, want the 6 refused", n)
	}
}

// The end lets go of what the agent still held for the ended recording, and
// says nothing per item: the requests in flight are given up (none is the
// ended recording's, nor a later one's), the mocks buffered for a request
// that may still claim them are let go, and so are the mappings queued for
// its stream, whose debts no stop waits on any more.
func TestAnEndedRecordingLetsGoOfWhatTheAgentHeldForIt(t *testing.T) {
	m, _, _, endStream, logs := endedRecording(t)
	inFlight := m.OpenWindow(time.Now(), nil)
	start := time.Now().Add(-2 * time.Second)
	// Two tests' mappings for a client that takes none: the drainer holds
	// one, waiting on the stream, and the other waits in the queue. Owed.
	m.AddMock(perTestMock(start.Add(10 * time.Millisecond)))
	m.ResolveRange(start, start.Add(100*time.Millisecond), "test-1", true, true)
	m.AddMock(perTestMock(start.Add(110 * time.Millisecond)))
	m.ResolveRange(start.Add(105*time.Millisecond), start.Add(150*time.Millisecond), "test-2", true, true)
	// Past the startup window (its first tests' calls are sent on as they
	// come), a call no window owns yet is buffered for a request that may
	// still claim it.
	for i := 0; i < models.StartupMockTestCaseWindow; i++ {
		at := start.Add(time.Duration(200+10*i) * time.Millisecond)
		m.ResolveRange(at, at.Add(time.Millisecond), fmt.Sprintf("startup-%d", i), true, false)
	}
	m.AddMock(perTestMock(time.Now().Add(time.Hour)))
	m.mu.Lock()
	buffered := len(m.buffer)
	m.mu.Unlock()
	if buffered == 0 {
		t.Fatal("setup: the call no window owns yet is not buffered")
	}
	m.mappingOverflowMu.Lock()
	waiting := len(m.mappingOverflow)
	m.mappingOverflowMu.Unlock()
	if waiting == 0 || !m.mappingOwedBefore(time.Now()) {
		t.Fatalf("setup: %d mappings wait in the queue, owed %v; want one waiting and owed", waiting, m.mappingOwedBefore(time.Now()))
	}

	m.EndRecording()
	endStream()

	if n := m.OpenWindows(); n != 0 {
		t.Fatalf("%d requests still in flight after the end, want none", n)
	}
	if inFlight.Keep() {
		t.Fatal("a request in flight at the end is kept as a test case of a recording that has ended")
	}
	m.mu.Lock()
	buffered = len(m.buffer)
	m.mu.Unlock()
	if buffered != 0 {
		t.Fatalf("%d mocks still buffered after the end", buffered)
	}
	m.mappingOverflowMu.Lock()
	queued := len(m.mappingOverflow)
	m.mappingOverflowMu.Unlock()
	if queued != 0 || m.mappingOwedBefore(time.Now()) {
		t.Fatalf("after the end %d mappings are queued and owed=%v, want none", queued, m.mappingOwedBefore(time.Now()))
	}
	m.mu.Lock()
	m.mappingOverflowMu.Lock()
	held := m.mappingChan != nil || m.mappingStreamCh != nil
	m.mappingOverflowMu.Unlock()
	m.mu.Unlock()
	if held {
		t.Fatal("the ended recording's mapping stream is still held, and what its buffer holds with it")
	}
	time.Sleep(50 * time.Millisecond) // a drainer still blocked on the ended stream retires
	for _, e := range logs.All() {
		if e.Level >= zapcore.WarnLevel {
			t.Fatalf("the end said %s: %q", e.Level, e.Message)
		}
	}
	if got := logs.FilterMessageSnippet("the agent captures none of the application's traffic").Len(); got != 1 {
		t.Fatalf("the end was said %d times, want once", got)
	}
	m.EndRecording() // idempotent
	if got := logs.FilterMessageSnippet("the agent captures none of the application's traffic").Len(); got != 1 {
		t.Fatalf("a second end was said again (%d lines)", got)
	}
}

// A recording that starts after an ended one captures from its start, as any
// does, and what the agent left uncaptured meanwhile is said once, with its
// counts: as the next recording starts, or as the agent stops.
func TestARecordingAfterAnEndedOneCaptures(t *testing.T) {
	m, _, _, endStream, logs := endedRecording(t)
	m.EndRecording()
	endStream()
	m.NoteNotCaptured()
	m.AddMock(perTestMock(time.Now()))

	m.StartRecordSession() // the next recording's mock stream binds
	if !m.Recording() {
		t.Fatal("a recording that started after an ended one does not record")
	}
	out := make(chan *models.Mock, 4)
	m.SetOutputChannel(out)
	if d := m.AddMock(perTestMock(time.Now())); d.Dropped() {
		t.Fatalf("the next recording's call was dropped: %q", d)
	}
	said := logs.FilterMessageSnippet("no recording took the application's traffic").All()
	if len(said) != 1 {
		t.Fatalf("what the agent left uncaptured was said %d times, want once", len(said))
	}
	f := said[0].ContextMap()
	if f["requests_not_captured"] != uint64(1) || f["calls_not_captured"] != uint64(1) || f["until"] != "a recording started" {
		t.Fatalf("the uncaptured traffic was said as %v, want 1 request and 1 call until a recording started", f)
	}
	m.CloseOutChan() // the agent stops: the next recording has not ended
	if n := logs.FilterMessageSnippet("no recording took the application's traffic").Len(); n != 1 {
		t.Fatalf("said %d times once the agent stopped, want still once", n)
	}
}

// An agent that stops while its last recording has ended says once what it
// left uncaptured: the app served on after the stop until the agent did.
func TestAnAgentStoppingAfterItsRecordingEndedSaysWhatItLeftUncaptured(t *testing.T) {
	m, _, _, endStream, logs := endedRecording(t)
	m.EndRecording()
	endStream()
	for i := 0; i < 3; i++ {
		m.NoteNotCaptured()
	}
	m.CloseOutChan()
	m.CloseOutChan()
	said := logs.FilterMessageSnippet("no recording took the application's traffic").All()
	if len(said) != 1 || said[0].ContextMap()["requests_not_captured"] != uint64(3) || !strings.Contains(said[0].ContextMap()["until"].(string), "stopped") {
		t.Fatalf("an agent stopping after its recording ended said %v, want once: 3 requests until it stopped", said)
	}
}

// A stop's ask is what makes the closing of the current recording's streams
// its end (StopWasAsked); a recording that starts after forgets it.
func TestAStopsAskIsTheCurrentRecordingsOwn(t *testing.T) {
	m := New(nil)
	if m.StopWasAsked() {
		t.Fatal("a stop was asked of a recording no stop asked of")
	}
	m.BacklogBefore(time.Now())
	if !m.StopWasAsked() {
		t.Fatal("the stop's ask was not noted")
	}
	m.StartRecordSession()
	if m.StopWasAsked() {
		t.Fatal("the next recording carries the last one's stop")
	}
	m.BacklogBefore(time.Now())
	m.ResumeRecording() // a new recording's stream attached
	if m.StopWasAsked() {
		t.Fatal("a recording whose stream attached after the last stop carries that stop")
	}
}

// Another manager's recording (a DaemonSet app's, each with its own manager)
// is not ended by this one's end.
func TestAnEndedRecordingIsItsManagersAlone(t *testing.T) {
	m, _, _, endStream, _ := endedRecording(t)
	other := New(nil)
	otherOut := make(chan *models.Mock, 4)
	other.SetOutputChannel(otherOut)
	other.SetFirstRequestSignaled()
	m.EndRecording()
	endStream()
	if !other.Recording() {
		t.Fatal("another manager stopped recording with this one's end")
	}
	if d := other.AddMock(perTestMock(time.Now())); d == MockDroppedNoRecording {
		t.Fatal("another manager's call was refused by this one's end")
	}
}

// An agent that serves a recording after an ended one, and then sees that
// one end too, says each ended recording's own count once: the first as the
// next recording starts, the second as the agent stops.
func TestEachEndedRecordingSaysItsOwnCountOnce(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	m := New(nil)
	m.SetLogger(zap.New(core))
	m.EndRecording()
	m.NoteNotCaptured()
	m.NoteNotCaptured()
	m.ResumeRecording() // the next recording starts
	m.EndRecording()    // and ends
	for i := 0; i < 3; i++ {
		m.NoteNotCaptured()
	}
	m.CloseOutChan() // the agent stops
	said := logs.FilterMessageSnippet("no recording took the application's traffic").All()
	if len(said) != 2 {
		t.Fatalf("two ended recordings were said %d times, want once each", len(said))
	}
	for i, want := range []struct {
		n     uint64
		until string
	}{{2, "a recording started"}, {3, "the agent stopped"}} {
		f := said[i].ContextMap()
		if f["requests_not_captured"] != want.n || f["until"] != want.until {
			t.Fatalf("ended recording %d was said as %v, want %d requests until %s", i+1, f, want.n, want.until)
		}
	}
}
