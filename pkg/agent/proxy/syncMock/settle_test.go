package manager

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/stall"
)

// fakeWatermark settles a test case once its pod's position has passed its
// end.
type fakeWatermark struct{ at map[string]time.Time }

func (f *fakeWatermark) Settled(_, pod string, _, end time.Time) bool {
	return !f.at[pod].Before(end)
}

func heldTC(name string, end time.Time) *models.TestCase {
	return &models.TestCase{Name: name, HTTPReq: models.HTTPReq{Timestamp: end.Add(-time.Millisecond)}, HTTPResp: models.HTTPResp{Timestamp: end}}
}

func names(hs []Held) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.TC.Name)
	}
	return out
}

// A test case is released once its verdict is final, not before; one whose
// verdict is final already goes at once.
func TestTestCaseHold_ReleasesWhenSettled(t *testing.T) {
	t0 := time.Unix(1000, 0)
	w := &fakeWatermark{at: map[string]time.Time{"a": t0}}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	h.Add(heldTC("settled", t0), "", "a")
	if got := names(h.Release()); len(got) != 1 || got[0] != "settled" {
		t.Fatalf("released %v, want the settled test case at once", got)
	}
	h.Add(heldTC("later", t0.Add(time.Second)), "", "a")
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v before its verdict was final", names(got))
	}
	w.at["a"] = t0.Add(time.Second)
	if got := h.Release(); len(got) != 1 || got[0].Unsettled {
		t.Fatalf("released %+v, want the test case, settled", got)
	}
}

// A pod's test cases leave in the order they came, and a pod held back does
// not hold another's.
func TestTestCaseHold_KeepsEachPodsOrder(t *testing.T) {
	t0 := time.Unix(1000, 0)
	w := &fakeWatermark{at: map[string]time.Time{"a": t0, "b": t0.Add(time.Hour)}}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	h.Add(heldTC("a1", t0.Add(2*time.Second)), "", "a") // not yet
	h.Add(heldTC("a2", t0), "", "a")                    // settled, behind a1
	h.Add(heldTC("b1", t0.Add(time.Second)), "", "b")   // settled
	if got := names(h.Release()); len(got) != 1 || got[0] != "b1" {
		t.Fatalf("released %v, want only b1: a2 waits behind a1", got)
	}
	w.at["a"] = t0.Add(2 * time.Second)
	if got := names(h.Release()); len(got) != 2 || got[0] != "a1" || got[1] != "a2" {
		t.Fatalf("released %v, want [a1 a2]", got)
	}
}

// The bounds release a test case whose verdict is not final, marked: in time,
// and, past the byte budget, the oldest first.
func TestTestCaseHold_BoundsReleaseAndSaySo(t *testing.T) {
	t0 := time.Unix(1000, 0)
	w := &fakeWatermark{at: map[string]time.Time{}}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	clk := &stall.ManualClock{}
	h.clk, h.hold = clk, nil
	h.Add(heldTC("old", t0), "", "a")
	clk.Advance(59 * time.Second)
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v inside the time bound", names(got))
	}
	clk.Advance(time.Second)
	got := h.Release()
	if len(got) != 1 || !got[0].Unsettled {
		t.Fatalf("released %+v at the time bound, want it marked unsettled", got)
	}

	h = NewTestCaseHold(w, time.Minute, 2*testCaseSize(heldTC("x", t0)))
	for _, n := range []string{"first", "second", "third"} {
		h.Add(heldTC(n, t0), "", "a")
	}
	got = h.Release()
	if len(got) != 1 || got[0].TC.Name != "first" || !got[0].Unsettled {
		t.Fatalf("released %+v past the byte budget, want the oldest, marked unsettled", got)
	}
	if h.Len() != 2 {
		t.Fatalf("holds %d, want 2", h.Len())
	}
}

// The manager keeps its recording's watermark.
func TestSyncMockManager_Watermark(t *testing.T) {
	m := New(nil)
	if m.Watermark() != nil {
		t.Fatal("a new manager has a watermark")
	}
	w := &fakeWatermark{}
	m.SetWatermark(w)
	if m.Watermark() != w {
		t.Fatal("the watermark set is not the one returned")
	}
	m.SetWatermark(nil)
	if m.Watermark() != nil {
		t.Fatal("the watermark was not cleared")
	}
}

// A mock the capacity path drops is told to OnDrop, outside the manager's
// locks; one delivered is not.
func TestSyncMockManager_OnDropIsToldOfEveryDrop(t *testing.T) {
	m := New(nil)
	stalledOutClock(m)
	var dropped []string
	m.OnDrop(func(mk *models.Mock) {
		// Outside outChanMu: the hook can use the manager.
		m.SetMemoryPressure(false)
		dropped = append(dropped, mk.Name)
	})
	m.sendToOutChan(&models.Mock{Name: "no-channel"})
	ch := make(chan *models.Mock, 1)
	m.SetOutputChannel(ch)
	m.sendToOutChan(&models.Mock{Name: "delivered"})
	m.sendToOutChan(&models.Mock{Name: "over-budget"})
	waitOutQueue(t, m)
	if len(dropped) != 2 || dropped[0] != "no-channel" || dropped[1] != "over-budget" {
		t.Fatalf("told of %v, want [no-channel over-budget]", dropped)
	}
	// One that arrives after the channel closed.
	m.CloseOutChan()
	m.AddMock(&models.Mock{Name: "after-close"})
	if len(dropped) != 3 || dropped[2] != "after-close" {
		t.Fatalf("told of %v, want the mock dropped after the channel closed too", dropped)
	}
}

// fencedWatermark settles everything, and passes a fence once told to.
type fencedWatermark struct {
	opened, passed uint64
	asked          int
}

func (f *fencedWatermark) Settled(string, string, time.Time, time.Time) bool { return true }
func (f *fencedWatermark) Fence() uint64                                     { f.opened++; return f.opened }
func (f *fencedWatermark) Passed(t uint64) bool                              { f.asked++; return f.passed >= t }

// A settled test case waits for the fence opened when it settled: the mocks
// handed on before then must have got to where a drop of theirs is told. The
// fence is opened once, and the pod's order holds behind it.
func TestTestCaseHold_WaitsForItsFence(t *testing.T) {
	t0 := time.Unix(1000, 0)
	w := &fencedWatermark{}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	h.Add(heldTC("first", t0), "", "a")
	h.Add(heldTC("second", t0), "", "a")
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v before its fence was passed", names(got))
	}
	if w.opened != 1 {
		t.Fatalf("opened %d fences, want 1: the second waits behind the first", w.opened)
	}
	h.Release()
	if w.opened != 1 {
		t.Fatalf("opened %d fences over two passes, want the first one kept", w.opened)
	}
	w.passed = 1
	got := h.Release()
	if len(got) != 1 || got[0].TC.Name != "first" || got[0].Unsettled {
		t.Fatalf("released %+v, want first, settled", got)
	}
	w.passed = 2
	if got := names(h.Release()); len(got) != 1 || got[0] != "second" {
		t.Fatalf("released %v, want second behind its own fence", got)
	}
}

// PendingIn reports a mock the manager still holds or is handing on, within
// the window asked about, and not one outside it.
func TestSyncMockManager_PendingIn(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock) // nothing reads it: a send is queued, then given up
	clk := &stall.ManualClock{}
	m.outClock, m.outStall = clk, time.Second
	m.SetOutputChannel(out)
	m.SetFirstRequestSignaled() // mocks are buffered for their window
	t0 := time.Unix(2000, 0)
	mk := &models.Mock{Name: "held"}
	mk.Spec.ReqTimestampMock = t0
	m.AddMock(mk)
	if !m.PendingIn(t0.Add(-time.Second), t0.Add(time.Second)) {
		t.Fatal("a buffered mock inside the window is not pending")
	}
	if m.PendingIn(t0.Add(time.Second), t0.Add(2*time.Second)) {
		t.Fatal("a buffered mock outside the window is pending")
	}
	// Claimed by its window: on its way out, until its send ends.
	var dropped atomic.Bool
	m.OnDrop(func(*models.Mock) { dropped.Store(true) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.ResolveRange(t0.Add(-time.Second), t0.Add(time.Second), "test-1", true, false)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		taken := len(m.buffer) == 0
		m.mu.Unlock()
		if taken {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the mock never left the buffer")
		}
		time.Sleep(time.Millisecond)
	}
	if !m.PendingIn(t0.Add(-time.Second), t0.Add(time.Second)) {
		t.Fatal("a mock on its way out is not pending in its window")
	}
	if m.PendingIn(t0.Add(time.Hour), t0.Add(2*time.Hour)) {
		t.Fatal("a mock on its way out holds another window: a full channel would hold back every test case")
	}
	<-done // the resolve does not wait on the consumer
	if !m.PendingIn(t0.Add(-time.Second), t0.Add(time.Second)) {
		t.Fatal("a mock queued for a consumer that has not taken it is not pending in its window")
	}
	waitClockPending(t, clk)
	clk.Advance(time.Second) // the consumer took nothing for the bound: given up
	waitOutQueue(t, m)
	if m.PendingIn(t0.Add(-time.Second), t0.Add(time.Second)) {
		t.Fatal("pending after its send ended")
	}
	if !dropped.Load() {
		t.Fatal("the drop was not told before the mock stopped being pending")
	}
}

// explainedWatermark settles nothing, passes no fence, and says what holds a
// test case: its pod, and the fence it waits on.
type explainedWatermark struct {
	settled, passed bool
	asked           int
}

type heldByText string

func (s heldByText) String() string { return string(s) }

func (e *explainedWatermark) Settled(string, string, time.Time, time.Time) bool { return e.settled }
func (e *explainedWatermark) Fence() uint64                                     { return 7 }
func (e *explainedWatermark) Passed(uint64) bool                                { return e.passed }
func (e *explainedWatermark) HeldBy(_, pod string, _, _ time.Time, fence uint64) fmt.Stringer {
	e.asked++
	if !e.settled {
		return heldByText("pod " + pod + " is behind")
	}
	return heldByText(fmt.Sprintf("fence %d", fence))
}

// A test case the bound lets go says what held it, as found when it was let
// go (later, what held it may have moved on), and only then: the watermark is
// not asked about one released settled.
func TestTestCaseHold_SaysWhatHeldWhatItLetGo(t *testing.T) {
	t0 := time.Unix(1000, 0)
	w := &explainedWatermark{}
	h := NewTestCaseHold(w, time.Minute, 1<<20)
	clk := &stall.ManualClock{}
	h.clk, h.hold = clk, nil
	h.Add(heldTC("behind", t0), "", "a")
	clk.Advance(time.Minute)
	got := h.Release()
	if len(got) != 1 || !got[0].Unsettled || got[0].HeldBy == nil || got[0].HeldBy.String() != "pod a is behind" {
		t.Fatalf("released %+v, want it unsettled and held by its pod", got)
	}
	// Settled, but its fence is not passed: what held it is the fence it
	// waited on, by its token.
	w.settled = true
	h.Add(heldTC("fenced", t0), "", "b")
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v before its fence was passed", names(got))
	}
	clk.Advance(time.Minute)
	got = h.Release()
	if len(got) != 1 || got[0].HeldBy == nil || got[0].HeldBy.String() != "fence 7" {
		t.Fatalf("released %+v, want it held by fence 7", got)
	}
	asked := w.asked
	w.passed = true
	h.Add(heldTC("settled", t0), "", "c")
	if got := h.Release(); len(got) != 1 || got[0].Unsettled || got[0].HeldBy != nil {
		t.Fatalf("released %+v, want it settled, held by nothing", got)
	}
	if w.asked != asked {
		t.Fatal("the watermark was asked what held a test case the hold did not let go unsettled")
	}
}

// EarliestStarts says, per pod, where the test cases the hold still holds
// start: the earliest request time among them, whatever order they came in.
func TestTestCaseHold_EarliestStarts(t *testing.T) {
	t0 := time.Unix(1000, 0)
	h := NewTestCaseHold(&fakeWatermark{at: map[string]time.Time{}}, time.Minute, 1<<20)
	h.Add(heldTC("a-late", t0.Add(3*time.Second)), "shop", "a")
	h.Add(heldTC("a-early", t0.Add(time.Second)), "shop", "a")
	h.Add(heldTC("b", t0.Add(2*time.Second)), "shop", "b")
	got := map[string]time.Time{}
	h.EarliestStarts(func(ns, pod string, start time.Time) { got[ns+"/"+pod] = start })
	want := map[string]time.Time{"shop/a": t0.Add(time.Second - time.Millisecond), "shop/b": t0.Add(2*time.Second - time.Millisecond)}
	if len(got) != len(want) || !got["shop/a"].Equal(want["shop/a"]) || !got["shop/b"].Equal(want["shop/b"]) {
		t.Fatalf("EarliestStarts = %v, want %v", got, want)
	}
	empty := NewTestCaseHold(&fakeWatermark{}, time.Minute, 1<<20)
	empty.EarliestStarts(func(string, string, time.Time) { t.Fatal("an empty hold reported a start") })
}

// A gRPC test case held beside an HTTP one ends when its response did: the
// hold's oldest end is the earlier of the two, or a stop between them would
// hear nothing is held while the earlier one waits (it is dropped unsent when
// the stream ends). One with no wire time at all hides neither.
func TestTestCaseHold_OldestCountsEveryHeldTestCasesEnd(t *testing.T) {
	end := time.Unix(1000, 0)
	grpcAt := func(name string, at time.Time) *models.TestCase {
		return &models.TestCase{Name: name, Kind: models.GRPC_EXPORT,
			GrpcReq: models.GrpcReq{Timestamp: at.Add(-time.Millisecond)}, GrpcResp: models.GrpcResp{Timestamp: at}}
	}

	h := NewTestCaseHold(&fakeWatermark{}, time.Hour, 1<<20) // nothing settles
	h.Add(heldTC("http", end), "", "")
	h.Add(grpcAt("grpc-later", end.Add(time.Second)), "", "")
	h.Add(&models.TestCase{Name: "no-wire-time"}, "", "")
	if got := h.Oldest(); !got.Equal(end) {
		t.Fatalf("Oldest = %v, want the HTTP test case's end %v", got, end)
	}

	h = NewTestCaseHold(&fakeWatermark{}, time.Hour, 1<<20)
	h.Add(heldTC("http", end), "", "")
	h.Add(grpcAt("grpc-earlier", end.Add(-time.Second)), "", "") // held behind it
	if got := h.Oldest(); !got.Equal(end.Add(-time.Second)) {
		t.Fatalf("Oldest = %v, want the gRPC test case's end %v", got, end.Add(-time.Second))
	}
}

// backlogWatermark is a Backlog watermark whose answers the test sets.
type backlogWatermark struct {
	settled, pending atomic.Bool
	progress         atomic.Uint64
	on               string
}

func (b *backlogWatermark) Settled(_, _ string, _, _ time.Time) bool { return b.settled.Load() }
func (b *backlogWatermark) PendingBefore(time.Time) bool             { return b.pending.Load() }
func (b *backlogWatermark) Progress() uint64                         { return b.progress.Load() }
func (b *backlogWatermark) PendingOn(time.Time) fmt.Stringer {
	if !b.pending.Load() {
		return nil
	}
	return stringerFunc(b.on)
}

type stringerFunc string

func (s stringerFunc) String() string { return string(s) }

// A watermark added beside the one SetWatermark sets: a verdict is final only
// once both say so; the embedder replacing or clearing its own leaves the
// added one; removing the added one leaves the embedder's.
func TestAddWatermark_SitsBesideSetWatermark(t *testing.T) {
	m := New(nil)
	if m.Watermark() != nil {
		t.Fatal("a manager with no watermark has one")
	}
	proxy := &backlogWatermark{}
	remove := m.AddWatermark(proxy)
	if m.Watermark() != Watermark(proxy) {
		t.Fatal("the only watermark is not returned as it is")
	}
	embedder := &backlogWatermark{}
	m.SetWatermark(embedder)
	w := m.Watermark()
	t0 := time.Unix(1000, 0)
	for _, c := range []struct{ embedder, proxy, want bool }{
		{false, false, false}, {true, false, false}, {false, true, false}, {true, true, true},
	} {
		embedder.settled.Store(c.embedder)
		proxy.settled.Store(c.proxy)
		if got := w.Settled("", "", t0, t0); got != c.want {
			t.Fatalf("embedder settled %v, proxy settled %v: Settled %v, want %v", c.embedder, c.proxy, got, c.want)
		}
	}
	m.SetWatermark(nil)
	if m.Watermark() != Watermark(proxy) {
		t.Fatal("clearing the embedder's watermark took the added one with it")
	}
	m.SetWatermark(embedder)
	remove()
	remove() // idempotent
	if m.Watermark() != Watermark(embedder) {
		t.Fatal("removing the added watermark did not leave the embedder's")
	}
}

// What a stop learns of the agent's backlog: known only with a Backlog
// watermark; pending while any watermark, a held test case, one being built,
// or a queue of built ones says so; the progress of every watermark that
// counts it, and of the ingress.
func TestBacklogBefore(t *testing.T) {
	m := New(nil)
	stop := time.Unix(2000, 0)
	if st := m.BacklogBefore(stop); st.Known {
		t.Fatal("known with no watermark")
	}
	a, b := &backlogWatermark{on: "a's parser"}, &backlogWatermark{on: "b's parser"}
	m.SetWatermark(a)
	defer m.AddWatermark(b)()
	if st := m.BacklogBefore(stop); !st.Known || st.Pending {
		t.Fatalf("%+v, want known and clear", st)
	}
	b.pending.Store(true)
	if st := m.BacklogBefore(stop); !st.Pending || st.WaitingOn != "b's parser" {
		t.Fatalf("%+v, want pending on b", st)
	}
	b.pending.Store(false)
	a.progress.Store(3)
	b.progress.Store(4)
	if st := m.BacklogBefore(stop); st.Progress != 7 {
		t.Fatalf("progress %d, want the sum 7", st.Progress)
	}

	// A test case being built whose response came before the stop.
	done := m.BuildingTestCase(stop.Add(-time.Second))
	if st := m.BacklogBefore(stop); !st.Pending {
		t.Fatal("not pending while a test case from before the stop was being built")
	}
	if st := m.BacklogBefore(stop.Add(-2 * time.Second)); st.Pending {
		t.Fatal("pending for a time before the test case being built")
	}
	before := m.BacklogBefore(stop).Progress
	done()
	done()
	if st := m.BacklogBefore(stop); st.Pending || st.Progress == before {
		t.Fatalf("%+v: a built test case still pending, or no progress counted", st)
	}

	// Once a stop has asked, a test case built from after the time it asked
	// about is no progress on its backlog: steady traffic after the stop
	// must not keep a stop waiting on a backlog that does not move.
	before = m.BacklogBefore(stop).Progress
	m.BuildingTestCase(stop.Add(time.Second))()
	if st := m.BacklogBefore(stop); st.Progress != before {
		t.Fatalf("progress moved from %d to %d for a test case from after the stop", before, st.Progress)
	}
	m.BuildingTestCase(stop.Add(-time.Second))()
	if st := m.BacklogBefore(stop); st.Progress == before {
		t.Fatal("no progress counted for a test case from before the stop")
	}

	// A queue of built test cases the stream has not taken.
	var queued atomic.Int64
	stopWatch := m.WatchTestCaseQueue(func() int { return int(queued.Load()) })
	queued.Store(1)
	if st := m.BacklogBefore(stop); !st.Pending {
		t.Fatal("not pending while built test cases waited in the queue")
	}
	queued.Store(0)
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatal("pending with the queue empty")
	}
	queued.Store(1)
	stopWatch()
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatal("a queue no longer watched still counts")
	}

	// A test case the stream holds for its verdict.
	m.NoteHeld(stop.Add(-time.Millisecond))
	if st := m.BacklogBefore(stop); !st.Pending {
		t.Fatal("not pending while a held test case ended before the stop")
	}
	m.NoteHeld(time.Time{})

	// PendingBefore is BacklogBefore's verdict.
	if pending, known := m.PendingBefore(stop); pending || !known {
		t.Fatalf("PendingBefore (%v, %v), want (false, true)", pending, known)
	}
}

// A mock a parser that ran behind its traffic hands over after its test's
// window resolved waits in the manager's buffer for the next flush, a ticker a
// starved agent runs late. Until that flush has handed it on, a stop that asks
// must hear it is pending: told "clear", it ends the mock stream and the mock
// is lost, with the recording reported complete.
func TestBacklogBefore_AMockTheNextFlushHandsOnIsPending(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	m.SetOutputChannel(out)
	defer m.AddWatermark(&backlogWatermark{})() // every parser caught up
	m.SetFirstRequestSignaled()

	start := time.Now().Add(-2 * time.Second)
	end := start.Add(100 * time.Millisecond)
	m.ResolveRange(start, end, "test-1", true, true)
	// Per-test (a kind no fallback makes reusable): test-1's window owns it.
	m.AddMock(&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: start.Add(10 * time.Millisecond),
		ResTimestampMock: start.Add(20 * time.Millisecond),
	}})
	if len(out) != 0 {
		t.Fatal("setup: the late mock was sent at once, not buffered for the flush")
	}

	stop := time.Now()
	if st := m.BacklogBefore(stop); !st.Pending || st.WaitingOn == "" {
		t.Fatalf("%+v: a mock from before the stop that the next flush hands on is not pending", st)
	}
	if st := m.BacklogBefore(start); st.Pending {
		t.Fatalf("%+v: pending for a time before the buffered mock was requested", st)
	}
	m.FlushOwnedWindows()
	if len(out) != 1 {
		t.Fatalf("setup: the flush handed on %d mocks, want the late one", len(out))
	}
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatalf("%+v: still pending once the flush handed the mock on", st)
	}
}

// A buffered mock that no window can take yet stays in the buffer for a
// request that may still claim it, and a flush leaves it there: it is not
// pending, or a stop would wait on it until it gave up and failed a recording
// that had lost nothing.
func TestBacklogBefore_AMockNoWindowCanTakeIsNotPending(t *testing.T) {
	m := New(nil)
	out := make(chan *models.Mock, 16)
	m.SetOutputChannel(out)
	defer m.AddWatermark(&backlogWatermark{})()
	m.SetFirstRequestSignaled()
	m.mu.Lock()
	m.resolvedTestCount = models.StartupMockTestCaseWindow // past the startup window
	m.mu.Unlock()

	at := time.Now().Add(-time.Second)
	// Per-test: a kind no fallback makes reusable, tagged as a test's mock.
	m.AddMock(&models.Mock{Kind: models.Mongo, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: at,
		ResTimestampMock: at,
	}})
	stop := time.Now()
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatalf("%+v: a mock no window can take is pending", st)
	}
	m.FlushOwnedWindows()
	m.mu.Lock()
	buffered := len(m.buffer)
	m.mu.Unlock()
	if buffered != 1 || len(out) != 0 {
		t.Fatalf("setup: the flush moved a mock no window can take (buffered %d, sent %d)", buffered, len(out))
	}
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatalf("%+v: a mock no window can take is pending after the flush", st)
	}
}

// A mock on its way to the stream (its send can wait on a full output
// channel), or owed back to its test at the next flush, is still the agent's:
// pending until it has been handed on.
func TestBacklogBefore_AMockBeingSentOrOwedIsPending(t *testing.T) {
	m := New(nil)
	defer m.AddWatermark(&backlogWatermark{})()
	stop := time.Now()
	mk := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: stop.Add(-time.Second)}}

	m.noteSending(mk)
	if st := m.BacklogBefore(stop); !st.Pending {
		t.Fatalf("%+v: a mock from before the stop whose send is under way is not pending", st)
	}
	if st := m.BacklogBefore(stop.Add(-2 * time.Second)); st.Pending {
		t.Fatalf("%+v: pending for a time before the mock being sent was requested", st)
	}
	m.sent(mk)
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatalf("%+v: still pending once the send finished", st)
	}

	m.mu.Lock()
	m.owed = append(m.owed, owedMock{mock: mk})
	m.mu.Unlock()
	if st := m.BacklogBefore(stop); !st.Pending {
		t.Fatalf("%+v: a mock from before the stop owed back to its test is not pending", st)
	}
	m.mu.Lock()
	m.owed = nil
	m.mu.Unlock()
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatalf("%+v: still pending once nothing is owed", st)
	}
}

// A session or connection mock in the buffer (an untagged HTTP mock is one)
// goes at the next flush whatever window it falls in: pending until then.
func TestBacklogBefore_AReusableMockInTheBufferIsPendingUntilTheFlush(t *testing.T) {
	t.Setenv(provenswitch.Env, "0") // this tests the paths with the proven switch off
	m := New(nil)
	out := make(chan *models.Mock, 16)
	m.SetOutputChannel(out)
	defer m.AddWatermark(&backlogWatermark{})()
	m.SetFirstRequestSignaled()
	m.mu.Lock()
	m.resolvedTestCount = models.StartupMockTestCaseWindow // past the startup window
	m.mu.Unlock()

	at := time.Now().Add(-time.Second)
	m.AddMock(&models.Mock{Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at}})
	stop := time.Now()
	if st := m.BacklogBefore(stop); !st.Pending {
		t.Fatalf("%+v: a reusable mock from before the stop that the next flush hands on is not pending", st)
	}
	m.FlushOwnedWindows()
	if len(out) != 1 {
		t.Fatalf("setup: the flush handed on %d mocks, want the reusable one", len(out))
	}
	if st := m.BacklogBefore(stop); st.Pending {
		t.Fatalf("%+v: still pending once the flush handed the mock on", st)
	}
}
