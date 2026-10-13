package manager

import (
	"fmt"
	"sync"
	"sync/atomic"
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

// Drain hands back every test case still held, settled or not, and holds none
// after: what its stream had taken when it ended without sending them.
func (h *TestCaseHold) Drain() []*models.TestCase {
	out := make([]*models.TestCase, 0, len(h.q))
	for _, e := range h.q {
		out = append(out, e.TC)
	}
	h.q, h.bytes = nil, 0
	return out
}

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
// final (see Watermark); nil: at once, as when no capture can tell. It replaces
// the one it set before; the watermarks AddWatermark added stay.
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

// AddWatermark adds a capture's watermark beside the one SetWatermark set: the
// verdict for a test case is final once every one of them says so, and the
// recording may still hand over something from before a time while any one of
// them says it may. A capture that runs beside another one, and does not see
// that one's connections, adds its own here: proxy mode's relays (the proxy
// adds theirs as it starts) beside an embedder's capture, which it replaces
// with SetWatermark as its recordings come and go. It returns the func that
// removes w; the func is idempotent. A nil w adds nothing.
func (m *SyncMockManager) AddWatermark(w Watermark) (remove func()) {
	if m == nil || w == nil {
		return func() {}
	}
	// Each addition is its own box, and is removed by it: a watermark need
	// not be comparable, and one added twice is two.
	box := &watermarkBox{w}
	m.addedMu.Lock()
	var next []*watermarkBox
	if cur := m.added.Load(); cur != nil {
		next = append(next, *cur...)
	}
	next = append(next, box)
	m.added.Store(&next)
	m.addedMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.addedMu.Lock()
			defer m.addedMu.Unlock()
			cur := m.added.Load()
			if cur == nil {
				return
			}
			next := make([]*watermarkBox, 0, len(*cur))
			for _, o := range *cur {
				if o != box {
					next = append(next, o)
				}
			}
			m.added.Store(&next)
		})
	}
}

// watermarks is every watermark of the recording: the one SetWatermark set,
// then those AddWatermark added. Nil when there is none.
func (m *SyncMockManager) watermarks() []Watermark {
	if m == nil {
		return nil
	}
	var out []Watermark
	if b := m.watermark.Load(); b != nil {
		out = append(out, b.w)
	}
	if a := m.added.Load(); a != nil {
		for _, b := range *a {
			out = append(out, b.w)
		}
	}
	return out
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
// capture with a Backlog watermark): a test case held for its verdict, or
// still being built or queued for the stream, or a capture reader or parser
// that has not got past it. See BacklogBefore.
func (m *SyncMockManager) PendingBefore(before time.Time) (pending, known bool) {
	st := m.BacklogBefore(before)
	return st.Pending, st.Known
}

// BacklogState is what a recording's stop learns of what the agent may still
// hand over from before a time (BacklogBefore).
type BacklogState struct {
	// Known: a capture can tell (a Backlog watermark is set). When it is
	// false the rest says nothing.
	Known bool
	// Pending: the agent may still hand over a test case or a mock captured
	// before the time.
	Pending bool
	// Progress is a count that moves whenever the capture makes progress
	// (Progressor): a stop that waits on a pending backlog can tell one that
	// is being worked through from one that is not moving.
	Progress uint64
	// WaitingOn says what is pending, when Pending and it can be told.
	WaitingOn string
}

// Horizon is a Watermark that can also tell how far the capture has got:
// every chunk stamped up to the time it returns has been read and noted. The
// recording's open orphan spans extend no further (SyncMockManager.CapturedTo).
type Horizon interface {
	CapturedTo() time.Time
}

// Progressor is a Backlog that can also count its progress: a count that
// moves whenever it gets further (a parser consumes bytes, or returns).
type Progressor interface {
	Progress() uint64
}

// PendingExplainer is a Backlog that can say what it may still hand over from
// before a time: nil when nothing.
type PendingExplainer interface {
	PendingOn(before time.Time) fmt.Stringer
}

// BacklogBefore is what this recording may still hand over from before
// `before` (BacklogState): a test case the recorder's stream holds for its
// verdict that ended by then, one the ingress is still building or has queued
// for the stream, or, as each Backlog watermark tells, a capture reader or
// parser that has not got past it. Known only when a Backlog watermark is set:
// without one nothing can tell whether a parser is behind.
func (m *SyncMockManager) BacklogBefore(before time.Time) BacklogState {
	var st BacklogState
	if m == nil {
		return st
	}
	var backlogs []Backlog
	for _, w := range m.watermarks() {
		if b, ok := w.(Backlog); ok {
			backlogs = append(backlogs, b)
		}
	}
	if len(backlogs) == 0 {
		return st
	}
	st.Known = true
	st.Progress = m.incoming.progress()
	for _, b := range backlogs {
		if p, ok := b.(Progressor); ok {
			st.Progress += p.Progress()
		}
	}
	pending := func(why string) {
		if !st.Pending {
			st.Pending, st.WaitingOn = true, why
		}
	}
	if h := m.heldOldest.Load(); h != 0 && h <= before.UnixNano() {
		pending("a test case held for its verdict ended before it")
	}
	if why := m.incoming.pendingBefore(before); why != "" {
		pending(why)
	}
	for _, b := range backlogs {
		if st.Pending {
			break
		}
		if x, ok := b.(PendingExplainer); ok {
			if on := x.PendingOn(before); on != nil {
				pending(on.String())
			}
			continue
		}
		if b.PendingBefore(before) {
			pending("the capture has not got past it")
		}
	}
	return st
}

// Watermark is what tells when the verdict for a test case of this recording
// is final: the one SetWatermark set, the ones AddWatermark added, or, when
// there are several, all of them together (each must say so). Nil when none.
func (m *SyncMockManager) Watermark() Watermark {
	ws := m.watermarks()
	switch len(ws) {
	case 0:
		return nil
	case 1:
		return ws[0]
	}
	return allWatermarks(ws)
}

// allWatermarks is several watermarks as one: a verdict is final when each
// says so (Settled); the first that can say what keeps one from being final
// says it (Explainer). It is not Fenced: a watermark with a fence holds its own
// test cases (a DaemonSet agent's), and is never added beside another.
type allWatermarks []Watermark

var _ Explainer = allWatermarks(nil)

func (a allWatermarks) Settled(namespace, pod string, start, end time.Time) bool {
	for _, w := range a {
		if !w.Settled(namespace, pod, start, end) {
			return false
		}
	}
	return true
}

func (a allWatermarks) HeldBy(namespace, pod string, start, end time.Time, fence uint64) fmt.Stringer {
	for _, w := range a {
		if w.Settled(namespace, pod, start, end) {
			continue
		}
		if x, ok := w.(Explainer); ok {
			if why := x.HeldBy(namespace, pod, start, end, fence); why != nil {
				return why
			}
		}
	}
	return nil
}

// incomingBacklog is what the ingress has yet to hand the recorder's stream:
// the test cases it is building, by the time their response came, and the
// queues built test cases wait in until the stream takes them.
type incomingBacklog struct {
	mu       sync.Mutex
	next     uint64
	building map[uint64]time.Time
	queues   map[uint64]func() int
	// built counts the test cases built (or given up): progress.
	built atomic.Uint64
}

// pendingBefore says why a test case from before `before` may still come from
// the ingress: one whose response came by then is still being built, or a
// queue holds test cases (whose times it cannot tell). "" when none may.
func (b *incomingBacklog) pendingBefore(before time.Time) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, resp := range b.building {
		if !resp.After(before) {
			return "a test case whose response came before it is still being built"
		}
	}
	for _, n := range b.queues {
		if n() > 0 {
			return "test cases wait in the ingress queue for the recorder's stream"
		}
	}
	return ""
}

func (b *incomingBacklog) progress() uint64 { return b.built.Load() }

// BuildingTestCase notes a test case whose response came at resp being built
// by the ingress, and returns the func that says it is done: handed to the
// stream's queue, or given up. A recording's stop waits on it
// (BacklogBefore). The func is idempotent.
func (m *SyncMockManager) BuildingTestCase(resp time.Time) (done func()) {
	if m == nil {
		return func() {}
	}
	b := &m.incoming
	b.mu.Lock()
	if b.building == nil {
		b.building = make(map[uint64]time.Time)
	}
	b.next++
	id := b.next
	b.building[id] = resp
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.building, id)
			b.mu.Unlock()
			b.built.Add(1)
		})
	}
}

// WatchTestCaseQueue notes a queue built test cases wait in for the
// recorder's stream, by its length, and returns the func that stops watching
// it. While it holds any, a recording's stop takes it that the agent may still
// hand over a test case from before it (BacklogBefore).
func (m *SyncMockManager) WatchTestCaseQueue(length func() int) (stop func()) {
	if m == nil || length == nil {
		return func() {}
	}
	b := &m.incoming
	b.mu.Lock()
	if b.queues == nil {
		b.queues = make(map[uint64]func() int)
	}
	b.next++
	id := b.next
	b.queues[id] = length
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.queues, id)
			b.mu.Unlock()
		})
	}
}
