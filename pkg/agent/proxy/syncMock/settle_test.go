package manager

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
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
	now := t0
	h.now = func() time.Time { return now }
	h.Add(heldTC("old", t0), "", "a")
	now = now.Add(59 * time.Second)
	if got := h.Release(); len(got) != 0 {
		t.Fatalf("released %v inside the time bound", names(got))
	}
	now = now.Add(time.Second)
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
	out := make(chan *models.Mock) // nothing reads it: a send waits out its budget
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
	<-done
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
	now := t0
	h.now = func() time.Time { return now }
	h.Add(heldTC("behind", t0), "", "a")
	now = now.Add(time.Minute)
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
	now = now.Add(time.Minute)
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
