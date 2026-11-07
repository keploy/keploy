package manager

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// addMockTo makes a Kafka mock asked now, of connection conn, of the session's
// lifetime when session is set (else per test). Not a Generic one: every
// Generic mock derives as the session's, which memory pressure keeps.
func addMockTo(conn string, session bool) *models.Mock {
	m := &models.Mock{Kind: models.Kind("Kafka"), Name: "fetch", ConnectionID: conn}
	if session {
		m.Spec.Metadata = map[string]string{"type": "config"}
	}
	m.Spec.ReqTimestampMock = time.Now()
	m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(time.Millisecond)
	return m
}

// namedDrop reports whether logs hold an entry at level that names mock (its
// kind and connection) and says why, as want.
func namedDrop(logs *observer.ObservedLogs, level zapcore.Level, mock *models.Mock, want MockDrop) bool {
	for _, e := range logs.All() {
		f := e.ContextMap()
		if e.Level == level && f["mock_kind"] == string(mock.Kind) && f["connID"] == mock.ConnectionID && f["why"] == string(want) {
			return true
		}
	}
	return false
}

// AddMock returns what it did with each mock: MockTaken for one it buffered
// or sent on, else why it dropped it, having counted it (the record summary's
// mocks_dropped_by_pressure, mocks_dropped_capacity) and said which mock and
// why. Before, it returned nothing, and a parser that said it kept a mock
// could not tell a drop.
func TestAddMockSaysWhatItDidWithEachMock(t *testing.T) {
	t.Run("memory pressure", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		ch := make(chan *models.Mock, 4)
		m.SetOutputChannel(ch)
		m.SetMemoryPressure(true)
		perTest := addMockTo("c1", false)
		if got := m.AddMock(perTest); got != MockDroppedUnderPressure {
			t.Fatalf("a per-test mock asked under pressure: %q, want %q", got, MockDroppedUnderPressure)
		}
		if _, dropped, _, _ := m.GetDropStats(); dropped != 1 {
			t.Fatalf("pressure drops counted %d, want 1", dropped)
		}
		if !namedDrop(logs, zapcore.WarnLevel, perTest, MockDroppedUnderPressure) {
			t.Fatalf("no WARN names the mock dropped under pressure: %v", logs.All())
		}
		// A mock of the session's lifetime is kept whenever it was asked.
		session := addMockTo("c1", true)
		if got := m.AddMock(session); got != MockTaken {
			t.Fatalf("a session mock asked under pressure: %q, want it taken", got)
		}
		if got := <-ch; got != session {
			t.Fatalf("the output got %v, want the session mock", got)
		}
	})
	t.Run("output closed", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		m.SetOutputChannel(make(chan *models.Mock, 4))
		m.CloseOutChan()
		mock := addMockTo("c2", false)
		if got := m.AddMock(mock); got != MockDroppedOutputClosed {
			t.Fatalf("a mock after the close: %q, want %q", got, MockDroppedOutputClosed)
		}
		if got := m.DropCount(); got != 1 {
			t.Fatalf("capacity drops counted %d, want 1", got)
		}
		if !namedDrop(logs, zapcore.WarnLevel, mock, MockDroppedOutputClosed) {
			t.Fatalf("no WARN names the mock dropped after the close: %v", logs.All())
		}
		// A stop may log no ERROR: a recording's end is no fault of the agent.
		if n := logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
			t.Fatalf("a mock dropped after the close logged %d ERRORs: %v", n, logs.All())
		}
	})
	t.Run("taken", func(t *testing.T) {
		m := New(zap.NewNop())
		ch := make(chan *models.Mock, 4)
		m.SetOutputChannel(ch)
		first := addMockTo("c3", false)
		if got := m.AddMock(first); got != MockTaken { // before the first request: sent on
			t.Fatalf("a mock before the first request: %q, want it taken", got)
		}
		if got := <-ch; got != first {
			t.Fatalf("the output got %v, want the mock", got)
		}
		m.SetFirstRequestSignaled()
		if got := m.AddMock(addMockTo("c3", false)); got != MockTaken { // buffered for its window
			t.Fatalf("a mock after the first request: %q, want it taken", got)
		}
		if _, dropped, _, buffered := m.GetDropStats(); dropped != 0 || buffered != 1 || m.DropCount() != 0 {
			t.Fatalf("pressure drops %d, buffered %d, capacity drops %d; want the mock buffered alone", dropped, buffered, m.DropCount())
		}
	})
}

// A mock AddMock sends at once (before the first request) that finds the
// process's queue at its byte bound behind another is dropped at once, and
// AddMock says so. Not parallel: the process's bound (processOutQueued) is
// every manager's.
func TestAddMockSaysItDroppedAMockAtTheByteBound(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	ch := make(chan *models.Mock, 1)
	ch <- &models.Mock{Name: "filler"}
	clk := &stall.ManualClock{}
	m := New(zap.New(core))
	m.outClock, m.outStall = clk, time.Hour
	m.SetOutputChannel(ch)
	processOutQueued.Add(outQueueMaxBytes)
	defer processOutQueued.Add(-outQueueMaxBytes)
	inHand := addMockTo("c4", false)
	if got := m.AddMock(inHand); got != MockTaken { // nothing before it: queued
		t.Fatalf("the first mock behind a full channel: %q, want it taken (queued)", got)
	}
	waitClockPending(t, clk) // its drainer has it in hand
	behind := addMockTo("c4", false)
	if got := m.AddMock(behind); got != MockDroppedOutputFull {
		t.Fatalf("a mock behind it past the byte bound: %q, want %q", got, MockDroppedOutputFull)
	}
	if !namedDrop(logs, zapcore.ErrorLevel, behind, MockDroppedOutputFull) {
		t.Fatalf("no ERROR names the mock dropped at the byte bound: %v", logs.All())
	}
	<-ch // the filler, then the mock in hand
	if got := <-ch; got != inHand {
		t.Fatalf("the output got %v, want the mock in the drainer's hand", got)
	}
	waitOutQueue(t, m)
}

// Every drop is said, naming its mock: at WARN (memory pressure, a closed
// output) or ERROR (an overflow) for the manager's first and every
// sendDropSampleRate-th of its kind, at Debug for the rest; a closed output's
// is said as one, not as an overflow, and the first says the recording is
// lossy.
func TestEachDropIsSaidAndSampled(t *testing.T) {
	t.Run("memory pressure", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		for _, n := range []int64{1, 2, int64(sendDropSampleRate)} {
			mock := addMockTo("p", false)
			m.reportPressureDrop(mock, n, string(MockDroppedUnderPressure))
			want := zapcore.DebugLevel
			if n != 2 {
				want = zapcore.WarnLevel
			}
			last := logs.All()[logs.Len()-1]
			if last.Level != want || last.ContextMap()["why"] != string(MockDroppedUnderPressure) || last.ContextMap()["connID"] != "p" {
				t.Fatalf("pressure drop %d said at %v with %v; want %v naming the mock and why", n, last.Level, last.ContextMap(), want)
			}
		}
	})
	t.Run("output closed", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		for i := uint64(1); i <= sendDropSampleRate; i++ {
			m.dropOut(addMockTo("o", false), "", 0, string(MockDroppedOutputClosed))
		}
		warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
		if len(warns) != 2 || logs.FilterLevelExact(zapcore.DebugLevel).Len() != int(sendDropSampleRate)-2 || logs.FilterLevelExact(zapcore.ErrorLevel).Len() != 0 {
			t.Fatalf("%d drops said %d at WARN, %d at Debug, %d at ERROR; want the first and the %dth at WARN, none at ERROR", sendDropSampleRate, len(warns), logs.FilterLevelExact(zapcore.DebugLevel).Len(), logs.FilterLevelExact(zapcore.ErrorLevel).Len(), sendDropSampleRate)
		}
		for i, e := range warns {
			if !strings.Contains(e.Message, "mock output had closed, or while none was set") || strings.Contains(e.Message, "overflow") {
				t.Fatalf("closed drop %d said %q; want it said as a closed output's", i, e.Message)
			}
			if strings.Contains(e.Message, "LOSSY") != (i == 0) {
				t.Fatalf("closed drop %d said %q; want LOSSY on the first alone", i, e.Message)
			}
		}
	})
}

// A mock AddMock took and memory pressure then lets go is the loss a parser's
// claim to have kept it cannot see, so it is counted (the record summary's
// mocks_dropped_by_pressure) and said, naming it, like one AddMock refused.
// Before, the wipe of the buffer and of the hold told only an aggregate at
// Debug, and the summary counted neither. A calm mock survives uncounted.
func TestPressureLettingGoOfATakenMockIsCountedAndSaid(t *testing.T) {
	t.Run("buffered", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		m.SetOutputChannel(make(chan *models.Mock, 4))
		// Past the startup window, whose mocks every reaper keeps.
		m.SetFirstRequestSignaled()
		m.resolvedTestCount = models.StartupMockTestCaseWindow
		calm := addMockTo("calm", false)
		calm.Spec.ReqTimestampMock = time.Now().Add(-time.Minute)
		if got := m.AddMock(calm); got != MockTaken {
			t.Fatalf("a calm mock: %q, want it taken", got)
		}
		m.SetMemoryPressure(true)
		m.SetMemoryPressure(false)
		late := addMockTo("late", false)
		// Taken once, in the pause's span: as a mock buffered just before the
		// next tick of a pause it fell in.
		late.Spec.ReqTimestampMock = time.Now()
		m.mu.Lock()
		late.DeriveLifetime()
		m.buffer = append(m.buffer, late)
		m.pressure.Record(late.Spec.ReqTimestampMock, late.Spec.ReqTimestampMock)
		m.mu.Unlock()
		_, _, addedBefore, _ := m.GetDropStats()
		m.SetMemoryPressure(true)
		_, dropped, added, buffered := m.GetDropStats()
		if dropped != 1 || buffered != 1 {
			t.Fatalf("pressure drops %d, buffered %d; want the mock of the pause counted, the calm one kept", dropped, buffered)
		}
		if added != addedBefore-1 { // the summary has each mock in one field
			t.Fatalf("added %d, was %d: a mock let go is still counted added", added, addedBefore)
		}
		if !namedDrop(logs, zapcore.WarnLevel, late, MockDroppedUnderPressure) {
			t.Fatalf("no WARN names the buffered mock pressure let go: %v", logs.All())
		}
	})
	t.Run("held", func(t *testing.T) {
		core, logs := observer.New(zapcore.DebugLevel)
		m := New(zap.New(core))
		held := addMockTo("held", false)
		owed := addMockTo("owed", false)
		m.mu.Lock()
		m.holdLocked(heldMock{mock: held})
		m.holdLocked(heldMock{mock: owed, owner: &resolvedWindow{}})
		m.mu.Unlock()
		m.SetMemoryPressure(true)
		if _, dropped, _, _ := m.GetDropStats(); dropped != 1 {
			t.Fatalf("pressure drops %d; want the held mock counted, the one owed to its window not", dropped)
		}
		if !namedDrop(logs, zapcore.WarnLevel, held, MockDrop(heldLetGo)) {
			t.Fatalf("no WARN names the held mock pressure let go: %v", logs.All())
		}
		if namedDrop(logs, zapcore.WarnLevel, owed, MockDrop(heldLetGo)) {
			t.Fatal("a mock owed to its window was said dropped")
		}
	})
}

// send says at once what it dropped at once: AddMock's own look at the output
// can find it open, and the close come before the send does, which leaves this
// the only one to tell. Queued or delivered is taken.
func TestSendSaysItDroppedAMockForAClosedOutput(t *testing.T) {
	m := New(zap.NewNop())
	ch := make(chan *models.Mock, 1)
	m.SetOutputChannel(ch)
	if got := m.send(addMockTo("s", false), "", nil); got != MockTaken {
		t.Fatalf("a mock the channel took: %q, want it taken", got)
	}
	<-ch
	m.CloseOutChan()
	if got := m.send(addMockTo("s", false), "", nil); got != MockDroppedOutputClosed {
		t.Fatalf("a mock sent to a closed output: %q, want %q", got, MockDroppedOutputClosed)
	}
}

// A drop at a recording's stop takes nothing from a real overflow: the two are
// sampled apart, so the first overflow is still the loud ERROR, on a manager
// that outlives the session whose stop dropped a mock.
func TestAClosedDropTakesNoSampleFromAnOverflow(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	m.dropOut(addMockTo("a", false), "", 0, string(MockDroppedOutputClosed))
	m.dropOut(addMockTo("a", false), "", 0, string(MockDroppedOutputFull))
	errs := logs.FilterLevelExact(zapcore.ErrorLevel).All()
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "FIRST drop") {
		t.Fatalf("the first overflow after a closed drop said %v; want its first ERROR", errs)
	}
	if got := m.DropCount(); got != 2 {
		t.Fatalf("capacity drops %d, want both", got)
	}
}

// An answer the client never read is counted when it is dropped, but its loss
// costs no test case: said at Debug, with no word of a lossy recording, and
// taking no sample, so the next real drop is still said aloud.
func TestAnUnreadAnswerDroppedIsCountedAndNotSaidLossy(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	m := New(zap.New(core))
	m.SetOutputChannel(make(chan *models.Mock, 1))
	m.CloseOutChan()
	unread := addMockTo("u", false)
	unread.MarkUndelivered()
	if got := m.AddMock(unread); got != MockDroppedOutputClosed {
		t.Fatalf("an unread answer after the close: %q, want %q", got, MockDroppedOutputClosed)
	}
	if got := m.DropCount(); got != 1 {
		t.Fatalf("capacity drops %d, want it counted", got)
	}
	if n := logs.FilterLevelExact(zapcore.WarnLevel).Len() + logs.FilterLevelExact(zapcore.ErrorLevel).Len(); n != 0 {
		t.Fatalf("an unread answer's drop was said aloud: %v", logs.All())
	}
	real := addMockTo("u", false)
	m.AddMock(real)
	if !namedDrop(logs, zapcore.WarnLevel, real, MockDroppedOutputClosed) {
		t.Fatalf("the unread answer took the first drop's sample: %v", logs.All())
	}
}

// A drop is said with outChanMu released, every path of it: a log write blocked
// on I/O under that lock would hold up every parser. The hook tries the write
// lock as the line is written, which any reader still holding it refuses.
func TestADropIsSaidWithTheOutputLockReleased(t *testing.T) {
	for _, closed := range []bool{true, false} {
		var m *SyncMockManager
		said, held := 0, 0
		core, _ := observer.New(zapcore.DebugLevel)
		logger := zap.New(zapcore.RegisterHooks(core, func(zapcore.Entry) error {
			said++
			if !m.outChanMu.TryLock() {
				held++
				return nil
			}
			m.outChanMu.Unlock()
			return nil
		}))
		m = New(logger)
		if closed {
			m.SetOutputChannel(make(chan *models.Mock, 1))
			m.CloseOutChan()
			m.send(addMockTo("l", false), "", nil)
		} else {
			func() {
				ch := make(chan *models.Mock, 1)
				ch <- &models.Mock{Name: "filler"}
				clk := &stall.ManualClock{}
				m.outClock, m.outStall = clk, time.Hour
				m.SetOutputChannel(ch)
				processOutQueued.Add(outQueueMaxBytes)
				defer processOutQueued.Add(-outQueueMaxBytes)
				m.send(addMockTo("l", false), "", nil)
				waitClockPending(t, clk)
				m.send(addMockTo("l", false), "", nil)
				<-ch
				<-ch
				waitOutQueue(t, m)
			}()
		}
		if said == 0 || held != 0 {
			t.Fatalf("closed %v: %d lines said, %d of them under outChanMu", closed, said, held)
		}
	}
}
