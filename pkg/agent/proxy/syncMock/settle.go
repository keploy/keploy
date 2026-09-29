package manager

import (
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// settle.go holds a test case until its verdict under the rule for unrecorded
// traffic (unrecorded.go) is final.
//
// The rule leaves out a test case whose window overlaps a span in which one of
// its connections could not be recorded. Some of those spans are only known
// late. A parser that fails stops its connection from where it had got to,
// which it learns only when it fails, and it runs behind the traffic (in RCA
// #2, by ten minutes). And a capture reader sharded by NUMA node can hand the
// response of a test case over before another shard's refused push of that
// test case's query. A test case checked as it completes is then saved without
// its mocks.
//
// So a capture that can tell when a verdict is final (a Watermark) has its test
// cases held until it is, and checked then. The hold is bounded, in time and in
// bytes: a parser minutes behind must not cost the agent its memory, nor hold
// the recording back for minutes. What the bound releases is checked against
// what is known by then, and counted (Held.Unsettled).

// Watermark tells when the rule's verdict for a test case is final: every span
// that could still overlap its window is known.
type Watermark interface {
	// Settled reports whether the verdict for a test case of pod (namespace
	// and pod; either "" when not known) recorded over [start, end] is final.
	Settled(namespace, pod string, start, end time.Time) bool
}

// Fenced is a Watermark whose settled verdict needs one more barrier: the
// mocks handed on before it settled must have got to where a drop of theirs is
// told (a DaemonSet agent streams its mocks on through a channel, and the
// stream can still drop one). The hold opens a fence for a test case once
// Settled says so, and releases it when the fence has been passed.
type Fenced interface {
	// Fence opens a barrier behind every mock handed on so far, and returns
	// its token (never 0).
	Fence() uint64
	// Passed reports whether every mock handed on before the token's Fence
	// has got through.
	Passed(token uint64) bool
}

// Explainer is a Watermark that can say what keeps a verdict from being final,
// for a test case the hold's bound lets go (Held.HeldBy).
type Explainer interface {
	// HeldBy says what keeps the verdict for a test case of pod recorded over
	// [start, end] from being final, once it waits on the fence with token
	// fence (Fenced; 0 when none was opened): nil when nothing does. What it
	// returns is formatted only when it is told.
	HeldBy(namespace, pod string, start, end time.Time, fence uint64) fmt.Stringer
}

// Backlog is a Watermark that can also tell whether the capture may still
// emit a test case or a mock captured before a time: a reader or a parser has
// not got past it. A recording's stop drains the agent's streams until it
// cannot (the agent's /record/pending, the CLI's drain).
type Backlog interface {
	PendingBefore(before time.Time) bool
}

const (
	// TestCaseHoldMax bounds how long a test case is held for its verdict.
	TestCaseHoldMax = 30 * time.Second
	// TestCaseHoldBytes bounds the bytes of test cases held at once: past
	// it, the oldest are released first.
	TestCaseHoldBytes int64 = 32 << 20
	// TestCaseHoldTick is how often held test cases are looked at again.
	TestCaseHoldTick = 20 * time.Millisecond
)

// Held is a test case the hold releases.
type Held struct {
	TC             *models.TestCase
	Namespace, Pod string
	// Unsettled: released by the hold's bound before its verdict was final.
	// It is checked against what is known, and may lack mocks.
	Unsettled bool
	// HeldBy is, for one Unsettled, what kept its verdict from being final
	// when the bound let it go (Explainer): nil when the watermark cannot say.
	HeldBy fmt.Stringer
}

type heldEntry struct {
	Held
	at   time.Time
	size int64
	// fence is the token of the barrier opened once its verdict settled
	// (Fenced); 0 until then.
	fence uint64
}

// TestCaseHold holds test cases until their verdict is final (Watermark), in
// the order they came per pod. Not safe for concurrent use: the one loop that
// takes the test cases owns it.
type TestCaseHold struct {
	w        Watermark
	maxAge   time.Duration
	maxBytes int64
	q        []heldEntry
	bytes    int64
	now      func() time.Time
}

// NewTestCaseHold holds test cases for w, for at most maxAge each and maxBytes
// in all (TestCaseHoldMax and TestCaseHoldBytes in production).
func NewTestCaseHold(w Watermark, maxAge time.Duration, maxBytes int64) *TestCaseHold {
	return &TestCaseHold{w: w, maxAge: maxAge, maxBytes: maxBytes, now: time.Now}
}

// Add holds tc, of pod (namespace, pod; either "" when not known). Release
// hands it back, at once when its verdict is final already.
func (h *TestCaseHold) Add(tc *models.TestCase, namespace, pod string) {
	size := testCaseSize(tc)
	h.q = append(h.q, heldEntry{Held: Held{TC: tc, Namespace: namespace, Pod: pod}, at: h.now(), size: size})
	h.bytes += size
}

// Len is how many test cases are held.
func (h *TestCaseHold) Len() int { return len(h.q) }

// EarliestStarts calls fn once for each pod it holds test cases of, with the
// earliest request time among them: every test case of the pod still to be
// checked that has come to the hold starts no earlier (Spans.CheckedBefore).
func (h *TestCaseHold) EarliestStarts(fn func(namespace, pod string, start time.Time)) {
	type podKey struct{ ns, pod string }
	earliest := make(map[podKey]time.Time, 1)
	for _, e := range h.q {
		k := podKey{e.Namespace, e.Pod}
		if at, ok := earliest[k]; !ok || e.TC.HTTPReq.Timestamp.Before(at) {
			earliest[k] = e.TC.HTTPReq.Timestamp
		}
	}
	for k, at := range earliest {
		fn(k.ns, k.pod, at)
	}
}

// Bytes is about how many bytes the held test cases take.
func (h *TestCaseHold) Bytes() int64 { return h.bytes }

// Oldest is the earliest end of a test case held, zero when none is.
func (h *TestCaseHold) Oldest() time.Time {
	var oldest time.Time
	for _, e := range h.q {
		if end := e.TC.HTTPResp.Timestamp; oldest.IsZero() || end.Before(oldest) {
			oldest = end
		}
	}
	return oldest
}

// Release returns the test cases whose verdict is final, and those the bounds
// release, in the order they came per pod: one waits behind an earlier one of
// its pod that is still held (a pod's test cases are named in that order).
func (h *TestCaseHold) Release() []Held {
	if len(h.q) == 0 {
		return nil
	}
	now := h.now()
	over := h.bytes - h.maxBytes
	type podKey struct{ ns, pod string }
	var blocked map[podKey]bool
	var out []Held
	keep := h.q[:0]
	for _, e := range h.q {
		k := podKey{e.Namespace, e.Pod}
		release := false
		switch {
		case over > 0:
			// Past the byte budget: the oldest go first, settled or not.
			release = true
			e.Unsettled = !h.final(&e)
		case blocked[k]:
		case h.final(&e):
			release = true
		case now.Sub(e.at) >= h.maxAge:
			release, e.Unsettled = true, true
		default:
			if blocked == nil {
				blocked = make(map[podKey]bool)
			}
			blocked[k] = true
		}
		if release {
			if e.Unsettled {
				e.HeldBy = h.heldBy(&e)
			}
			out = append(out, e.Held)
			h.bytes -= e.size
			over -= e.size
			continue
		}
		keep = append(keep, e)
	}
	clear(h.q[len(keep):])
	h.q = keep
	if len(h.q) == 0 {
		h.q = nil // do not keep a burst's backing array
	}
	return out
}

// final reports whether e's verdict is final: settled, and past its fence
// when the watermark has one (Fenced), which it opens the first time.
func (h *TestCaseHold) final(e *heldEntry) bool {
	if !h.w.Settled(e.Namespace, e.Pod, e.TC.HTTPReq.Timestamp, e.TC.HTTPResp.Timestamp) {
		return false
	}
	f, ok := h.w.(Fenced)
	if !ok {
		return true
	}
	if e.fence == 0 {
		e.fence = f.Fence()
	}
	return f.Passed(e.fence)
}

// heldBy is what keeps e's verdict from being final, as the watermark tells
// it (Explainer): asked only for a test case the bound lets go.
func (h *TestCaseHold) heldBy(e *heldEntry) fmt.Stringer {
	x, ok := h.w.(Explainer)
	if !ok {
		return nil
	}
	return x.HeldBy(e.Namespace, e.Pod, e.TC.HTTPReq.Timestamp, e.TC.HTTPResp.Timestamp, e.fence)
}

// testCaseSize is about how many bytes a test case holds.
func testCaseSize(tc *models.TestCase) int64 {
	if tc == nil {
		return 0
	}
	n := int64(512 + len(tc.HTTPReq.URL) + len(tc.HTTPReq.Body) + len(tc.HTTPResp.Body))
	for k, v := range tc.HTTPReq.Header {
		n += int64(len(k) + len(v))
	}
	for k, v := range tc.HTTPResp.Header {
		n += int64(len(k) + len(v))
	}
	return n
}

// watermarkBox lets a Watermark sit in an atomic.Pointer.
type watermarkBox struct{ w Watermark }

// SetWatermark says when the verdict for a test case of this recording is
// final (see Watermark); nil: at once, as when no capture can tell.
func (m *SyncMockManager) SetWatermark(w Watermark) {
	if m == nil {
		return
	}
	if w == nil {
		m.watermark.Store(nil)
		return
	}
	m.watermark.Store(&watermarkBox{w})
}

// NoteHeld says the earliest end of a test case the recorder's stream still
// holds (TestCaseHold.Oldest), zero when it holds none.
func (m *SyncMockManager) NoteHeld(oldest time.Time) {
	if m == nil {
		return
	}
	var ns int64
	if !oldest.IsZero() {
		ns = oldest.UnixNano()
	}
	m.heldOldest.Store(ns)
}

// PendingBefore reports whether this recording may still hand over a test case
// or a mock captured before `before`, and whether that can be told at all (a
// capture with a Backlog watermark): a test case held for its verdict, or a
// capture reader or parser that has not got past it.
func (m *SyncMockManager) PendingBefore(before time.Time) (pending, known bool) {
	if m == nil {
		return false, false
	}
	b, ok := m.Watermark().(Backlog)
	if !ok {
		return false, false
	}
	if h := m.heldOldest.Load(); h != 0 && h <= before.UnixNano() {
		return true, true
	}
	return b.PendingBefore(before), true
}

// Watermark is what SetWatermark set, nil when none.
func (m *SyncMockManager) Watermark() Watermark {
	if m == nil {
		return nil
	}
	if b := m.watermark.Load(); b != nil {
		return b.w
	}
	return nil
}
