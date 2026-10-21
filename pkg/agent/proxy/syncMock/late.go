package manager

import (
	"slices"
	"sort"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// late.go is, under the proven switch, what a record stream's test cases sent
// with nothing lost turn out to have lost after they went.
//
// A parser runs behind the traffic, and reports what it could not record when
// it gets there (RecordOrphanWindowWhy): a test case its span overlaps may
// have been streamed already, checked against the spans known then. A
// LateWatch watches each such test case, and each span recorded while it does
// charges, exactly as it is recorded, the watched test cases it overlaps: the
// stream's end names them (Late), for keploy record to charge as it charges a
// test case streamed with its loss.
//
// The watch is exact where the spans are not: past their cap Spans joins
// spans, over gaps and, once their test cases have been checked, all before a
// point into one (CheckedBefore). Those joins only ever widen what a test
// case checked later is checked against; a test case checked again against
// them at the stream's end would be charged with a span it never overlapped.
// A LateWatch never reads the spans: each span it is told of is the span as
// it was recorded.

// MaxLateWatched bounds how many test cases one stream watches: about 32 MiB
// of them. One streamed past it is not watched, and counted (Unwatched).
const MaxLateWatched = 1 << 18

// LateWatch watches one record stream's test cases sent with nothing lost
// (Watch) for a span recorded after they went (late.go). Nil-safe: a nil
// watch watches nothing.
type LateWatch struct {
	m  *SyncMockManager
	mu sync.Mutex
	// byStart is the test cases watched, sorted by start (then by seq).
	byStart []*Watched
	// widest is the widest window watched: a span can overlap no watched
	// test case that starts more than that before it.
	widest time.Duration
	seq    int
	// unwatched is how many test cases Watch was given past MaxLateWatched.
	unwatched int
}

// Watched is one test case a LateWatch watches (Watch), its handle there.
type Watched struct {
	seq  int
	name string
	// start and end are its window as the stream was given it, which names
	// it (models.LateLoss); stop is end, or start for an end before it: the
	// window a span is checked against.
	start, end, stop time.Time
	lost             models.CaptureLosses
	gone             bool
}

// WatchLate opens a watch of the test cases a record stream sends with
// nothing lost (LateWatch). Close it as the stream ends. Nil for a nil
// manager. Under the proven switch only: the stream opens it only for a
// client that charges what a test case lost (models.IncomingOptions.ChargesLosses).
func (m *SyncMockManager) WatchLate() *LateWatch {
	if m == nil {
		return nil
	}
	w := &LateWatch{m: m}
	m.lateMu.Lock()
	m.late = append(m.late, w)
	m.lateMu.Unlock()
	return w
}

// Close ends the watch: no span is told to it any more. Late keeps reporting
// what it found. Idempotent, and a no-op on a nil watch.
func (w *LateWatch) Close() {
	if w == nil {
		return
	}
	w.m.lateMu.Lock()
	defer w.m.lateMu.Unlock()
	if i := slices.Index(w.m.late, w); i >= 0 {
		w.m.late = slices.Delete(w.m.late, i, i+1)
	}
}

// Watch watches test case name (empty for one the agent streams unnamed),
// recorded over [start, end], from now on: every span recorded from here
// charges it if it overlaps it. The stream calls it before it checks the test
// case against the spans known (TestCaseLosses): a span recorded in between is
// then either one the check sees, and the test case is streamed with it
// (Forget), or one the watch is told of. Nil (watching nothing) for a nil
// watch, a zero bound, and past MaxLateWatched, which Unwatched counts.
func (w *LateWatch) Watch(name string, start, end time.Time) *Watched {
	if w == nil || start.IsZero() || end.IsZero() {
		return nil
	}
	start, end = wallTime(start), wallTime(end)
	stop := end
	if stop.Before(start) {
		stop = start
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.byStart) >= MaxLateWatched {
		w.unwatched++
		return nil
	}
	w.seq++
	t := &Watched{seq: w.seq, name: name, start: start, end: end, stop: stop}
	i := sort.Search(len(w.byStart), func(k int) bool { return w.byStart[k].start.After(start) })
	w.byStart = slices.Insert(w.byStart, i, t)
	if d := stop.Sub(start); d > w.widest {
		w.widest = d
	}
	return t
}

// Forget stops watching t: it was streamed with what it lost (the check that
// follows Watch found it), or not streamed at all. What the watch charged it
// with meanwhile is dropped: the check read every span recorded before it.
// A no-op on a nil t.
func (w *LateWatch) Forget(t *Watched) {
	if w == nil || t == nil {
		return
	}
	w.mu.Lock()
	t.gone, t.lost = true, nil
	w.mu.Unlock()
}

// span charges every watched test case that [start, end] overlaps with a
// loss of kind, What why; a zero end is a span still open, to now and on.
// Overlap is Spans.Overlaps': [a, b] and [c, d] overlap iff a <= d and c <= b.
func (w *LateWatch) span(kind models.CaptureLossKind, start, end time.Time, why string) {
	open := end.IsZero()
	start = wallTime(start)
	if !open {
		end = wallTime(end)
		if end.Before(start) {
			end = start
		}
	}
	lost := models.CaptureLosses{{Kind: kind, Count: 1, What: why}}
	w.mu.Lock()
	defer w.mu.Unlock()
	// A test case that starts before start-widest ends before start.
	from := sort.Search(len(w.byStart), func(k int) bool { return !w.byStart[k].start.Before(start.Add(-w.widest)) })
	to := len(w.byStart)
	if !open {
		to = sort.Search(len(w.byStart), func(k int) bool { return w.byStart[k].start.After(end) })
	}
	for _, t := range w.byStart[from:to] {
		if t.gone || t.stop.Before(start) {
			continue
		}
		t.lost = t.lost.PlusNaming(lost)
	}
}

// Late is what each test case watched and streamed with nothing lost has lost
// since it went, in the order they were watched: each span recorded after it
// went that overlaps it, and, for one the agent named, a mock of its own
// dropped on its way to keploy record (WasMockDroppedForTC). Each is named as
// the agent streamed it (models.LateLoss). Nil for none.
func (w *LateWatch) Late() []models.LateLoss {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	type late struct {
		seq  int
		loss models.LateLoss
	}
	var out []late
	var named []*Watched
	for _, t := range w.byStart {
		if t.gone {
			continue
		}
		if t.name != "" {
			named = append(named, t)
		}
		if len(t.lost) > 0 {
			out = append(out, late{t.seq, models.LateLoss{Name: t.name, Request: t.start, Response: t.end, Lost: slices.Clone(t.lost)}})
		}
	}
	w.mu.Unlock()
	// By name, outside w.mu: droppedMu is a leaf of its own.
	dropped := models.CaptureLosses{{Kind: models.CaptureLossMockDropped, Count: 1, What: MockDroppedOnItsWay}}
	for _, t := range named {
		if !w.m.WasMockDroppedForTC(t.name) {
			continue
		}
		if i := slices.IndexFunc(out, func(l late) bool { return l.seq == t.seq }); i >= 0 {
			out[i].loss.Lost = out[i].loss.Lost.PlusNaming(dropped)
			continue
		}
		out = append(out, late{t.seq, models.LateLoss{Name: t.name, Request: t.start, Response: t.end, Lost: dropped}})
	}
	if len(out) == 0 {
		return nil
	}
	slices.SortFunc(out, func(a, b late) int { return a.seq - b.seq })
	losses := make([]models.LateLoss, len(out))
	for i, l := range out {
		losses[i] = l.loss
	}
	return losses
}

// Unwatched is how many test cases Watch was given past MaxLateWatched: each
// streamed with nothing lost, and saved without what a span recorded after it
// went says it lost.
func (w *LateWatch) Unwatched() int {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unwatched
}

// noteLate tells every open LateWatch of a span recorded now: [start, end]
// (a zero end for one still open), lost as kind, What why. Called with no
// lock of m's held; a no-op with no watch open, which is every recording but
// one under the proven switch whose client charges what a test case lost.
func (m *SyncMockManager) noteLate(kind models.CaptureLossKind, start, end time.Time, why string) {
	if m == nil || start.IsZero() {
		return
	}
	m.lateMu.Lock()
	watches := slices.Clone(m.late)
	m.lateMu.Unlock()
	for _, w := range watches {
		w.span(kind, start, end, why)
	}
}
