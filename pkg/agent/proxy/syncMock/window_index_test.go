package manager

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// linearOwnerOrShared is ownerOrSharedLocked as it was before the ring had an
// index, verbatim: a walk of every window in the ring, in its order. The rows
// here hold the index to it. Caller holds mu.
func linearOwnerOrShared(m *SyncMockManager, t time.Time) (owner resolvedWindow, ok bool, shared resolvedWindow, behind bool) {
	var sharedOwner resolvedWindow
	found, foundShared := false, false
	for _, w := range m.recentWindows {
		if t.Before(w.start) || t.After(w.end) {
			continue
		}
		if w.keep {
			if !t.After(w.exclusiveEnd()) {
				return w, true, resolvedWindow{}, false
			}
			if !foundShared {
				sharedOwner, foundShared = w, true
			}
			continue
		}
		if !found {
			owner, found = w, true
		}
	}
	if foundShared {
		if !m.openClaimLocked(t, nil) {
			return sharedOwner, true, resolvedWindow{}, false
		}
		return owner, found, sharedOwner, true
	}
	return owner, found, resolvedWindow{}, false
}

// linearClaims is claimsLocked as it was before the ring had an index,
// verbatim. Caller holds mu.
func linearClaims(m *SyncMockManager, self *Window, after, end time.Time) claims {
	var c claims
	for _, o := range m.open {
		if o == self || o.start.After(end) || (o.yieldAt != 0 && o.yieldAt <= after.UnixNano()) {
			continue
		}
		var to time.Time
		if o.yieldAt != 0 {
			to = time.Unix(0, o.yieldAt)
		}
		c = append(c, claim{from: o.start, to: to})
	}
	for _, r := range m.recentWindows {
		if !r.keep || r.start.After(end) || !r.end.After(after) {
			continue
		}
		c = append(c, claim{from: r.start, to: r.end})
	}
	return mergeClaims(c)
}

// clockStepped is t, which has a monotonic reading, as a time.Now() taken
// after the wall clock was stepped by d reads: its wall clock on by d from its
// monotonic reading's. time.Time keeps the monotonic reading in its second
// word when it has one; the row fails if that moved anything else.
func clockStepped(tb testing.TB, t time.Time, d time.Duration) time.Time {
	tb.Helper()
	if !hasMonotonic(t) {
		tb.Fatalf("clockStepped: %v has no monotonic reading", t)
	}
	type timeWords struct {
		wall uint64
		ext  int64
		loc  *time.Location
	}
	s := t
	(*timeWords)(unsafe.Pointer(&s)).ext -= int64(d)
	if s.Sub(t) != -d || s.UnixNano() != t.UnixNano() || !hasMonotonic(s) {
		tb.Fatalf("clockStepped: time.Time is not laid out as this row assumes (moved its monotonic reading by %v, its wall clock by %dns)", s.Sub(t), s.UnixNano()-t.UnixNano())
	}
	return s
}

// instants draws the times the differential rows put in windows and ask
// about: few enough of them on a coarse grid that windows share their starts
// and ends and times fall on their bounds; a nanosecond off them; with and
// without a monotonic reading, and with one taken after the wall clock was
// stepped (by less than the grid, so the two clocks order the same times
// differently, and by an hour); and the zero time and times past the int64
// nanosecond range on either side.
type instants struct {
	tb   testing.TB
	r    *rand.Rand
	base time.Time
}

func (g instants) grid() time.Time {
	t := g.base.Add(time.Duration(g.r.IntN(48)) * time.Millisecond)
	if g.r.IntN(6) == 0 {
		t = t.Add(time.Duration(g.r.IntN(3) - 1))
	}
	return t
}

func (g instants) next() time.Time {
	switch g.r.IntN(24) {
	case 0:
		return time.Time{}
	case 1:
		return time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)
	case 2:
		return time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	t := g.grid()
	switch g.r.IntN(8) {
	case 0, 1:
		t = t.Round(0)
	case 2:
		steps := []time.Duration{-3 * time.Millisecond, -time.Millisecond, -1, 1, time.Millisecond, 3 * time.Millisecond, time.Hour, -time.Hour}
		t = clockStepped(g.tb, t, steps[g.r.IntN(len(steps))])
	}
	return t
}

// window is a resolved window as the ring may hold one: overlapping others,
// ending before it starts at times, kept or a duplicate's, yielded before,
// inside or after its span, of one of two recordings.
func (g instants) window() resolvedWindow {
	w := resolvedWindow{start: g.next(), keep: g.r.IntN(3) > 0, recording: uint32(g.r.IntN(2)), mapping: g.r.IntN(2) == 0}
	switch g.r.IntN(5) {
	case 0:
		w.end = w.start
	case 1:
		w.end = g.next()
	default:
		w.end = w.start.Add(time.Duration(g.r.IntN(12)) * time.Millisecond)
	}
	if w.keep && g.r.IntN(2) == 0 {
		w.sharedFrom = g.grid().UnixNano()
	}
	w.testName = fmt.Sprintf("test-%d", g.r.IntN(1000))
	return w
}

// probes are the times a check asks about: drawn ones, and the bounds of
// windows the ring holds, a nanosecond either side, and their yields.
func (g instants) probes(ring []resolvedWindow, n int) []time.Time {
	ps := []time.Time{{}, time.Date(1500, 1, 1, 0, 0, 0, 0, time.UTC)}
	for range n {
		ps = append(ps, g.next())
	}
	for range n / 2 {
		if len(ring) == 0 {
			break
		}
		w := ring[g.r.IntN(len(ring))]
		ps = append(ps, w.start, w.end, w.start.Add(-1), w.end.Add(1), w.exclusiveEnd(), w.exclusiveEnd().Add(1))
	}
	return ps
}

// checkLookups holds the index to the walk of the ring at every probe, for the
// owner lookup and for the claims of a yielded resolve: the same windows, the
// same answers.
func checkLookups(t *testing.T, m *SyncMockManager, g instants, n int, open []*Window) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	// In step first: a rebuild numbers the ring's windows again, and the
	// answers must name the same windows of the ring, places and all.
	m.windowsLocked()
	for _, p := range g.probes(m.recentWindows, n) {
		wo, wok, ws, wb := linearOwnerOrShared(m, p)
		o, ok, s, b := m.ownerOrSharedLocked(p)
		if o != wo || ok != wok || s != ws || b != wb {
			t.Fatalf("ownerOrSharedLocked(%v) over a ring of %d = (%+v, %v, %+v, %v), the walk of the ring gives (%+v, %v, %+v, %v)", p, len(m.recentWindows), o, ok, s, b, wo, wok, ws, wb)
		}
	}
	for range n / 4 {
		after, end := g.next(), g.next()
		if g.r.IntN(2) == 0 && !after.After(end) {
			after, end = end, after
		}
		var self *Window
		if len(open) > 0 && g.r.IntN(2) == 0 {
			self = open[g.r.IntN(len(open))]
		}
		want := linearClaims(m, self, after, end)
		if got := m.claimsLocked(self, after, end); !reflect.DeepEqual(got, want) {
			t.Fatalf("claimsLocked(%v, %v) over a ring of %d = %v, the walk of the ring gives %v", after, end, len(m.recentWindows), got, want)
		}
	}
}

// TestWindowIndexAgreesWithTheWalkOfTheRing: over rings of randomized windows
// (overlapping, sharing bounds, inverted, kept and duplicates', yielded,
// zero and out-of-range times, times with and without a monotonic reading and
// with one taken across a wall-clock step), with requests in flight that
// claim times after the kept windows' yields, the index's owner lookup and
// claims give what the walk of every window in the ring gave, at every
// probe. Some rings pass their bound several times, so the oldest windows are
// let go in order and the ring wraps; some are written directly, as rows do
// (added to, cut at either end, or written over at the same length), which the
// index must notice and rebuild for.
func TestWindowIndexAgreesWithTheWalkOfTheRing(t *testing.T) {
	for seed := range uint64(6) {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, 0x5eed))
			g := instants{tb: t, r: r, base: time.Now().Add(-time.Minute)}
			m := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
			var open []*Window
			t.Cleanup(func() {
				for _, w := range open {
					w.Close()
				}
			})
			for range 4 {
				w := m.OpenWindow(g.grid(), nil)
				if r.IntN(2) == 0 {
					w.Yield(g.grid())
				}
				open = append(open, w)
			}
			// Seeds 0 and 1 fill the ring past its bound twice over; the rest
			// stay small, where ties and edges are dense.
			adds, every := 600, 15
			if seed < 2 {
				adds, every = 2*maxRecentWindows+500, 900
			}
			for i := range adds {
				w := g.window()
				m.mu.Lock()
				// A direct write is checked at once, before an add could
				// bring the index back in step by itself.
				// The small rings are written directly one add in six; the
				// full ones, which wrap and let go in order, not at all.
				direct := true
				pick := 4
				if seed >= 2 {
					pick = r.IntN(24)
				}
				switch pick {
				case 0:
					// A row's own window, appended as rows append one.
					m.recentWindows = append(m.recentWindows, w)
				case 1:
					// The ring's front cut away directly.
					if k := len(m.recentWindows); k > 4 {
						m.recentWindows = m.recentWindows[1+r.IntN(3):]
					}
				case 2:
					// Its end cut away directly.
					if k := len(m.recentWindows); k > 4 {
						m.recentWindows = m.recentWindows[:k-1-r.IntN(3)]
					}
				case 3:
					// Written over in place, its length kept, as a row that
					// puts back a ring it saved does: every window moved down
					// one, w last.
					if k := len(m.recentWindows); k > 1 {
						copy(m.recentWindows, m.recentWindows[1:])
						m.recentWindows[k-1] = w
					}
				default:
					direct = false
					m.addRecentWindowLocked(w)
				}
				m.mu.Unlock()
				if direct || i%every == 0 || i == adds-1 {
					checkLookups(t, m, g, 24, open)
				}
			}
			m.mu.Lock()
			n := len(m.recentWindows)
			m.mu.Unlock()
			if seed < 2 && n != maxRecentWindows {
				t.Fatalf("the ring holds %d windows, want it at its bound, %d", n, maxRecentWindows)
			}
		})
	}
}

// twinSide is one of the two managers a pass differential makes the same
// calls of, and what it handed on.
type twinSide struct {
	m      *SyncMockManager
	out    chan *models.Mock
	maps   chan models.TestMockMapping
	toldMu sync.Mutex
	told   []string
	open   []*Window
	// rows is the row of each mock by the name it was handed on under.
	rows map[string]string
	// gone is what the side handed on to an output it then closed
	// (closeOutput), said by the next state.
	gone strings.Builder
}

// handedOn is what ch holds, read until it is empty or closed.
func handedOn(ch chan *models.Mock) []*models.Mock {
	var got []*models.Mock
	for {
		select {
		case mk, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, mk)
		default:
			return got
		}
	}
}

// closeOutput closes the side's output, as the end of a recording in the same
// agent does, and keeps what was handed on to it for the next state. The next
// output bound is a new channel, as a new recording's is.
func (s *twinSide) closeOutput() {
	s.m.CloseOutChan()
	for _, mk := range handedOn(s.out) {
		s.rows[mk.Name] = rowOf(mk)
		fmt.Fprintf(&s.gone, "out %s; ", rowOf(mk))
	}
	s.out = make(chan *models.Mock, 1<<16)
}

func newTwinSide(bound bool) *twinSide {
	s := &twinSide{out: make(chan *models.Mock, 1<<16), maps: make(chan models.TestMockMapping, 1<<16), rows: map[string]string{}}
	m := &SyncMockManager{buffer: make([]*models.Mock, 0, defaultMockBufferCapacity)}
	if bound {
		m.SetOutputChannel(s.out)
	}
	m.SetMappingChannel(context.Background(), s.maps)
	m.SetFirstRequestSignaled()
	// Two kept resolves still inside the startup window: the startup
	// rescues run, then stop.
	m.resolvedTestCount = models.StartupMockTestCaseWindow - 2
	m.OnDrop(func(mk *models.Mock) {
		s.toldMu.Lock()
		s.told = append(s.told, rowOf(mk))
		s.toldMu.Unlock()
	})
	s.m = m
	return s
}

func rowOf(mk *models.Mock) string { return mk.Spec.Metadata["row"] }

// rowsOf names the mocks a mapping lists by their rows.
func (s *twinSide) rowsOf(names []string) []string {
	rows := make([]string, len(names))
	for i, n := range names {
		if r, ok := s.rows[n]; ok {
			rows[i] = r
		} else {
			rows[i] = "unsent:" + strconv.Itoa(len(n))
		}
	}
	return rows
}

// state is what the side handed on since it was last asked, and what it holds
// now: each mock by its row, in order, with the reasons, owners and counts
// that decide what happens to it next.
func (s *twinSide) state() string {
	var b strings.Builder
	b.WriteString(s.gone.String())
	s.gone.Reset()
	for _, mk := range drained(s.out) {
		s.rows[mk.Name] = rowOf(mk)
		fmt.Fprintf(&b, "out %s; ", rowOf(mk))
	}
	// The mappings a call sends, in no order: a resolve sends its late
	// mappings by ranging over a map.
	var maps []string
	for more := true; more; {
		select {
		case e := <-s.maps:
			maps = append(maps, fmt.Sprintf("map %s %v; ", e.TestName, s.rowsOf(e.MockIDs)))
		default:
			more = false
		}
	}
	slices.Sort(maps)
	b.WriteString(strings.Join(maps, ""))
	owner := func(w *resolvedWindow) string {
		if w == nil {
			return "-"
		}
		return fmt.Sprintf("%s/%v/%v", w.testName, w.keep, w.mapping)
	}
	m := s.m
	m.mu.Lock()
	for _, mk := range m.buffer {
		fmt.Fprintf(&b, "buf %s; ", rowOf(mk))
	}
	for _, h := range m.held {
		fmt.Fprintf(&b, "held %s %d %s; ", rowOf(h.mock), h.why, owner(h.owner))
	}
	for _, o := range m.owed {
		fmt.Fprintf(&b, "owed %s %s; ", rowOf(o.mock), owner(o.to))
	}
	for _, c := range m.sentClaimed {
		fmt.Fprintf(&b, "claim %s %d %s %d %s; ", s.rowsOf([]string{c.name}), c.at.UnixNano(), c.takenBy, c.sent, owner(c.owner))
	}
	fmt.Fprintf(&b, "counts %d %d %d %d %d %d %d %d %d %d %d; ", len(m.heldCut), m.heldBytes, m.heldShared, m.resolvedTestCount, len(m.recentWindows), len(m.open), m.givenUpInFlight, m.cutDrops.Load(), m.goneLetGo.Load(), m.owedLetGo.Load(), m.windowsGivenUp.Load())
	fmt.Fprintf(&b, "%d %d %d %d; ", m.keptLeftOut.Load(), m.pressureDropped.Load(), m.totalAdded.Load(), m.cutLeftOut.Load())
	m.mu.Unlock()
	s.toldMu.Lock()
	fmt.Fprintf(&b, "told %v", s.told)
	s.toldMu.Unlock()
	return b.String()
}

// unsettle has the side's next pass decide every mock in its buffer, as the
// walk did before passes stepped over the settled ones.
func (s *twinSide) unsettle() {
	s.m.mu.Lock()
	s.m.settled = settledPrefix{}
	s.m.mu.Unlock()
}

// mockOf is a mock of the row's kind: a test's own (most), one any test may
// reuse (a session's or a connection's), an answer the client never read, or
// a test's with no request time.
func mockOf(row int, at time.Time, kind int) *models.Mock {
	mk := &models.Mock{Kind: models.HTTP, Name: "mocks", Spec: models.MockSpec{
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
		Metadata:         map[string]string{"row": strconv.Itoa(row)},
	}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
	switch kind {
	case 1:
		mk.TestModeInfo.Lifetime = models.LifetimeSession
	case 2:
		mk.TestModeInfo.Lifetime = models.LifetimeConnection
	case 3:
		mk.Spec.Metadata[models.MetadataUndeliveredKey] = models.MetadataUndelivered
	case 4:
		mk.Spec.ReqTimestampMock = time.Time{}
	}
	return mk
}

// TestPassesThatStepOverSettledMocksDecideAsTheWalkOfEveryMockDid drives two
// managers with the same randomized calls:
//   - mocks added: a test's own, ones any test may reuse, unread answers, no
//     request time;
//   - windows resolved (kept and duplicates', overlapping, some late over a
//     mock that waits in the buffer), and bursts of a thousand resolves
//     every hundred calls, which fill the ring past its bound so it lets go
//     of windows;
//   - requests in flight that yield, are kept and resolved or closed;
//   - flushes, the duplicate's prune, memory pressure on and off, and a pause
//     that takes a buffered mock (its request moved inside the pause, as one
//     made just after the pause began, before the guard opened it);
//   - the proven switch flipped;
//   - an output wired late, an output closed and a new one bound (a recording
//     ended and another one started in the same agent), a recording's stop
//     and end, and a new recording;
//   - test cases sent and revoked, and mocks sent to two destinations;
//   - time passing the stale cutoff for the mocks buffered, and rows writing
//     the ring or the buffer directly.
//
// Mock and window times are taken with a monotonic reading, one in ten of
// them across a wall-clock step of a millisecond or three.
//
// One manager steps over the mocks the last pass left settled; the other
// decides every buffered mock on every pass, as the walk did. After every call
// both handed on the same mocks in the same order, with the same mappings, and
// hold the same buffer, hold, owed and sent mocks with the same counts and the
// same drops told. Both managers' owner lookups agree with the walk of the ring
// for every mock they hold.
//
// Mocks and windows are an hour from now, or an hour before (past the
// cutoff), so the cutoff, read from the clock by each call, splits them the
// same way for both managers however slow the run. Time passing is the
// buffered mocks' requests moved two hours back: the cutoff compares only
// them, and no window is resolved over the hour they move to. The bursts are
// ten hours ahead, where no mock is.
func TestPassesThatStepOverSettledMocksDecideAsTheWalkOfEveryMockDid(t *testing.T) {
	for seed := range uint64(8) {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, 0xd1ff))
			future := time.Now().Add(time.Hour)
			past := future.Add(-2 * time.Hour)
			far := future.Add(10 * time.Hour)
			// Mocks spread over twice the span windows are resolved over:
			// many wait buffered across passes, and a window comes later for
			// some.
			span := 64
			at := func(base time.Time) time.Time {
				d := time.Duration(r.IntN(span)) * time.Millisecond
				if r.IntN(4) == 0 {
					d += time.Duration(r.IntN(3) - 1)
				}
				t0 := base.Add(d)
				if r.IntN(10) == 0 {
					steps := []time.Duration{-3 * time.Millisecond, -time.Millisecond, time.Millisecond, 3 * time.Millisecond}
					t0 = clockStepped(t, t0, steps[r.IntN(len(steps))])
				}
				return t0
			}
			bound := seed%4 != 3 // one seed in four wires its output late
			a, b := newTwinSide(bound), newTwinSide(bound)
			sides := []*twinSide{a, b}
			// Half the seeds' clients can be told of a revoke.
			for _, s := range sides {
				s.m.SetRevokeCapable(seed%2 == 0)
			}
			t.Cleanup(func() {
				for _, s := range sides {
					for _, w := range s.open {
						w.Close()
					}
				}
			})
			each := func(fn func(s *twinSide)) {
				b.unsettle()
				for _, s := range sides {
					fn(s)
				}
			}
			proven := false
			row := 0
			var sent []string
			destSet := false
			resolved, farN := 0, 0
			done := map[string]int{}
			for step := range 1500 {
				var did string
				op := r.IntN(100)
				if step%100 == 50 {
					op = -1
				}
				switch {
				case op < 0:
					did = "burst"
					for range 1000 {
						farN++
						s0 := far.Add(time.Duration(farN) * time.Millisecond)
						keep := farN%5 == 0
						name := fmt.Sprintf("far-%d", farN)
						each(func(s *twinSide) { s.m.ResolveRange(s0, s0.Add(100*time.Microsecond), name, keep, false) })
					}
					resolved += 1000
				case op < 36:
					row++
					kind := 0
					if k := r.IntN(20); k < 5 {
						kind = k
					}
					base := future
					if r.IntN(8) == 0 {
						base = past
					}
					span = 128
					when := at(base)
					span = 64
					did = fmt.Sprintf("add %d %d", row, kind)
					each(func(s *twinSide) { s.m.AddMock(mockOf(row, when, kind)) })
				case op < 54:
					start := at(future)
					end := start.Add(time.Duration(r.IntN(8)) * time.Millisecond)
					over := ""
					if r.IntN(3) == 0 {
						// A window resolved late over a mock that waits in the
						// buffer, settled or not: an out-of-order request's.
						var waiting []time.Time
						a.m.mu.Lock()
						for _, mk := range a.m.buffer {
							if t0 := mk.Spec.ReqTimestampMock; t0.After(past.Add(time.Minute)) {
								waiting = append(waiting, t0)
							}
						}
						a.m.mu.Unlock()
						if len(waiting) > 0 {
							t0 := waiting[r.IntN(len(waiting))]
							start = t0.Add(-time.Duration(r.IntN(3)) * time.Millisecond)
							end = t0.Add(time.Duration(r.IntN(3)) * time.Millisecond)
							over = " over a buffered mock"
						}
					}
					keep, mapping := r.IntN(3) > 0, r.IntN(4) > 0
					name := fmt.Sprintf("test-%d", step)
					did = fmt.Sprintf("resolve%s %v %v", over, keep, mapping)
					each(func(s *twinSide) { s.m.ResolveRange(start, end, name, keep, mapping) })
					resolved++
				case op < 60:
					base := future
					if r.IntN(6) == 0 {
						base = past
					}
					start := at(base)
					did = "open"
					each(func(s *twinSide) { s.open = append(s.open, s.m.OpenWindow(start, nil)) })
				case op < 63:
					if len(a.open) == 0 {
						continue
					}
					i, when := r.IntN(len(a.open)), at(future)
					did = "yield"
					each(func(s *twinSide) { s.open[i].Yield(when) })
				case op < 69:
					if len(a.open) == 0 {
						continue
					}
					// Resolved over the hour ahead only: no window is resolved
					// over the hour the buffered mocks move back to.
					i := r.IntN(len(a.open))
					start := at(future)
					end := start.Add(time.Duration(r.IntN(8)) * time.Millisecond)
					mapping := r.IntN(4) > 0
					name := fmt.Sprintf("kept-%d", step)
					did = "keep"
					each(func(s *twinSide) {
						w := s.open[i]
						s.open = append(s.open[:i:i], s.open[i+1:]...)
						if w.Claim() {
							s.m.ResolveKept(w, start, end, name, mapping)
						}
						w.Close()
					})
				case op < 71:
					if len(a.open) == 0 {
						continue
					}
					i := r.IntN(len(a.open))
					did = "close"
					each(func(s *twinSide) {
						w := s.open[i]
						s.open = append(s.open[:i:i], s.open[i+1:]...)
						w.Close()
					})
				case op < 79:
					did = "flush"
					each(func(s *twinSide) { s.m.FlushOwnedWindows() })
				case op < 81:
					base := future
					if r.IntN(3) == 0 {
						base = past
					}
					before := at(base)
					did = "prune"
					each(func(s *twinSide) { s.m.DeleteMocksStrictlyBefore(before) })
				case op < 83:
					on := r.IntN(2) == 0
					did = fmt.Sprintf("pressure %v", on)
					each(func(s *twinSide) { s.m.SetMemoryPressure(on) })
				case op < 84:
					// The memory guard pauses recording, and a buffered test's
					// mock was requested inside the pause: one made just after
					// the pause began, taken before the guard opened it
					// (SetMemoryPressure reads the clock before it takes mu).
					// A row moves the request of one such mock, or of one added
					// for it, to now, inside the pause on both managers (each
					// opened it before now was read), with the request time kept
					// beside the buffer; the guard's next tick wipes it.
					each(func(s *twinSide) { s.m.SetMemoryPressure(true) })
					var cand []int
					a.m.mu.Lock()
					for i, mk := range a.m.buffer {
						if mk.TestModeInfo.Lifetime == models.LifetimePerTest && !isStartupMock(mk) {
							cand = append(cand, i)
						}
					}
					a.m.mu.Unlock()
					if len(cand) == 0 {
						row++
						when := at(future)
						each(func(s *twinSide) { s.m.AddMock(mockOf(row, when, 0)) })
						a.m.mu.Lock()
						if n := len(a.m.buffer); n > 0 && rowOf(a.m.buffer[n-1]) == strconv.Itoa(row) && !isStartupMock(a.m.buffer[n-1]) {
							cand = append(cand, n-1)
						}
						a.m.mu.Unlock()
					}
					if len(cand) == 0 {
						// A startup mock: the wipe keeps it whenever it was
						// requested.
						did = "pressure true"
						break
					}
					i := cand[r.IntN(len(cand))]
					now := time.Now()
					did = "a pause over a buffered mock"
					each(func(s *twinSide) {
						s.m.mu.Lock()
						s.m.buffer[i].Spec.ReqTimestampMock = now
						if i < len(s.m.settled.at) {
							s.m.settled.at[i] = now
						}
						s.m.mu.Unlock()
						s.m.SetMemoryPressure(true)
					})
				case op < 86:
					proven = !proven
					did = fmt.Sprintf("proven %v", proven)
					t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: "0"}[proven])
				case op < 88 && bound:
					did = "output closed"
					bound = false
					each(func(s *twinSide) { s.closeOutput() })
				case op < 88:
					did = "new output bound"
					bound = true
					each(func(s *twinSide) { s.m.SetOutputChannel(s.out) })
				case op < 89:
					stopAt := time.Now()
					did = "stop and end"
					each(func(s *twinSide) {
						s.m.NoteStop(stopAt)
						s.m.EndStoppedRecording()
					})
				case op < 90:
					did = "new recording"
					each(func(s *twinSide) {
						s.m.StartRecordSession()
						s.m.SetFirstRequestSignaled()
					})
				case op < 93:
					start := at(future)
					end := start.Add(time.Duration(r.IntN(8)) * time.Millisecond)
					name := fmt.Sprintf("sent-%d", step)
					did = "sending"
					each(func(s *twinSide) { s.m.SendingTestCase(name, start, end) })
					sent = append(sent, name)
					if r.IntN(2) == 0 {
						victim := sent[r.IntN(len(sent))]
						did += " and revoke"
						each(func(s *twinSide) { s.m.RevokeTestCase(victim) })
					}
				case op < 94:
					if destSet {
						continue
					}
					destSet = true
					did = "two destinations"
					each(func(s *twinSide) {
						s.m.SetDestinationOf(func(mk *models.Mock) string {
							if n, _ := strconv.Atoi(rowOf(mk)); n%3 == 0 {
								return "other"
							}
							return "own"
						}, func() string { return "own" })
					})
				case op < 95 && r.IntN(2) == 0:
					// A row's own window, put in the ring directly over the
					// hour ahead, where mocks wait: the manager must not take
					// the mocks it settled for ones no window holds.
					w := resolvedWindow{start: at(future), keep: r.IntN(3) > 0, mapping: true, testName: fmt.Sprintf("row-%d", step)}
					w.end = w.start.Add(time.Duration(r.IntN(30)) * time.Millisecond)
					did = "a row adds a window"
					each(func(s *twinSide) {
						s.m.mu.Lock()
						s.m.recentWindows = append(s.m.recentWindows, w)
						s.m.mu.Unlock()
					})
				case op < 95:
					// A row writes the buffer: its first mock moved to its end.
					did = "a row moves a buffered mock"
					each(func(s *twinSide) {
						s.m.mu.Lock()
						if b := s.m.buffer; len(b) > 1 {
							s.m.buffer = append(append([]*models.Mock(nil), b[1:]...), b[0])
						}
						s.m.mu.Unlock()
					})
				case op < 97:
					did = "time passes"
					each(func(s *twinSide) {
						s.m.mu.Lock()
						for _, mk := range s.m.buffer {
							if mk.Spec.ReqTimestampMock.After(past.Add(time.Minute)) {
								mk.Spec.ReqTimestampMock = mk.Spec.ReqTimestampMock.Add(-2 * time.Hour)
							}
						}
						// The settled mocks' request times, kept beside the
						// buffer, move with them: time passed for the manager.
						for i := range min(len(s.m.settled.at), len(s.m.buffer)) {
							s.m.settled.at[i] = s.m.buffer[i].Spec.ReqTimestampMock
						}
						s.m.mu.Unlock()
					})
				default:
					if bound {
						continue
					}
					bound = true
					did = "output wired"
					each(func(s *twinSide) { s.m.SetOutputChannel(s.out) })
				}
				// What the call was, its numbers and switches left out.
				var kind []string
				for _, w := range strings.Fields(did) {
					if _, err := strconv.Atoi(w); err != nil && w != "true" && w != "false" {
						kind = append(kind, w)
					}
				}
				done[strings.Join(kind, " ")]++
				if sa, sb := a.state(), b.state(); sa != sb {
					t.Fatalf("step %d (%s): the pass that steps over settled mocks left\n%s\nthe walk of every mock left\n%s", step, did, sa, sb)
				}
				for _, s := range sides {
					checkHeldLookups(t, s.m)
				}
			}
			// The ring reached its bound and holds fewer windows than were
			// resolved: it let go of the rest. (A row's own window put in it
			// after the last resolve may hold it one past its bound until the
			// next resolve.)
			a.m.mu.Lock()
			ring := len(a.m.recentWindows)
			a.m.mu.Unlock()
			if ring < maxRecentWindows || resolved <= ring {
				t.Fatalf("the ring holds %d windows of the %d resolved: the bursts did not take it past its bound, %d", ring, resolved, maxRecentWindows)
			}
			// Every event the calls are drawn to make, made.
			for _, kind := range []string{"burst", "resolve over a buffered mock", "output closed", "stop and end", "new recording", "sending", "sending and revoke", "two destinations", "a pause over a buffered mock", "prune", "pressure", "time passes"} {
				if done[kind] == 0 {
					t.Fatalf("no call made %q: %v", kind, done)
				}
			}
			if done["new output bound"]+done["output wired"] == 0 {
				t.Fatalf("no output was bound after the first: %v", done)
			}
			da, _ := a.m.passWorkLocked()
			db, _ := b.m.passWorkLocked()
			t.Logf("calls %v; decided %d stepping over settled mocks, %d deciding every one", done, da, db)
		})
	}
}

// checkHeldLookups holds the owner lookup to the walk of the ring for every
// mock m holds, buffered or held.
func checkHeldLookups(t *testing.T, m *SyncMockManager) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.windowsLocked()
	check := func(at time.Time) {
		wo, wok, ws, wb := linearOwnerOrShared(m, at)
		if o, ok, s, b := m.ownerOrSharedLocked(at); o != wo || ok != wok || s != ws || b != wb {
			t.Fatalf("ownerOrSharedLocked(%v) = (%+v, %v, %+v, %v), the walk of the ring gives (%+v, %v, %+v, %v)", at, o, ok, s, b, wo, wok, ws, wb)
		}
	}
	for _, mk := range m.buffer {
		check(mk.Spec.ReqTimestampMock)
	}
	for _, h := range m.held {
		check(h.mock.Spec.ReqTimestampMock)
	}
}

// TestTheIndexWidensForAClockStepWhoseWindowsAreStillInTheRing: the older half
// of a full ring was resolved across a wall-clock step, its windows' wall
// clocks 5 ms ahead of their monotonic readings. A window more lets go of the
// oldest, and the skew is worked out again from the ring: the stepped windows
// still in it keep the searches wide. A time the monotonic clock puts inside
// one of them, its wall clock 4.75 ms before the window's, is that window's,
// as the walk of the ring finds.
func TestTheIndexWidensForAClockStepWhoseWindowsAreStillInTheRing(t *testing.T) {
	m := openWindowManager(make(chan *models.Mock, 1), make(chan models.TestMockMapping, 1))
	base := time.Now().Add(-time.Hour)
	ms := func(i int) time.Time { return base.Add(time.Duration(i) * time.Millisecond) }
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range maxRecentWindows + 1 {
		start, end := ms(i), ms(i).Add(500*time.Microsecond)
		if i < maxRecentWindows/2 {
			start, end = clockStepped(t, start, 5*time.Millisecond), clockStepped(t, end, 5*time.Millisecond)
		}
		m.addRecentWindowLocked(resolvedWindow{start: start, end: end, keep: true, testName: fmt.Sprintf("test-%d", i)})
	}
	for i := maxRecentWindows/2 - 8; i < maxRecentWindows/2; i++ {
		// Window i's monotonic reading is 5 ms behind its wall clock: a quarter
		// of a millisecond into it by that reading is 4.75 ms before it by
		// the wall clock.
		at := ms(i).Add(-4750 * time.Microsecond)
		wo, wok, _, _ := linearOwnerOrShared(m, at)
		if !wok || wo.testName != fmt.Sprintf("test-%d", i) {
			t.Fatalf("the walk of the ring does not put %v in test-%d (got %q, %v): the row's clock step is not what it means", at, i, wo.testName, wok)
		}
		if o, ok, _, _ := m.ownerOrSharedLocked(at); !ok || o != wo {
			t.Fatalf("ownerOrSharedLocked(%v) = (%q, %v), the walk of the ring gives %q", at, o.testName, ok, wo.testName)
		}
	}
}

// TestAMockAWipeKeepsAfterOneItDropsIsDecidedByItsOwnRequestTime: two settled
// mocks, the first requested while the memory guard paused recording and the
// second an hour ahead. The pause's wipe drops the first and keeps the second,
// whose request time moves down with it beside the buffer: the window
// resolved over the second takes it.
func TestAMockAWipeKeepsAfterOneItDropsIsDecidedByItsOwnRequestTime(t *testing.T) {
	out := make(chan *models.Mock, 16)
	m := openWindowManager(out, make(chan models.TestMockMapping, 16))
	paused := time.Now().Add(20 * time.Millisecond)
	ahead := time.Now().Add(time.Hour)
	m.AddMock(httpMockAt(paused))
	m.AddMock(httpMockAt(ahead))
	far := ahead.Add(time.Hour)
	m.ResolveRange(far, far, "elsewhere", true, false)
	m.mu.Lock()
	settled := m.settledLocked()
	m.mu.Unlock()
	if settled != 2 {
		t.Fatalf("%d of the 2 buffered mocks settled, want both", settled)
	}
	// The pause opens before the first mock's request; the guard's next tick,
	// once the clock is past that request, wipes it.
	m.SetMemoryPressure(true)
	t.Cleanup(func() { m.SetMemoryPressure(false) })
	for !time.Now().After(paused) {
		time.Sleep(time.Millisecond)
	}
	m.SetMemoryPressure(true)
	m.mu.Lock()
	n, settled := len(m.buffer), m.settledLocked()
	m.mu.Unlock()
	if n != 1 || settled != 1 {
		t.Fatalf("after the wipe %d mocks buffered, %d settled, want the one ahead, settled", n, settled)
	}
	sentMocks(out)
	m.ResolveRange(ahead, ahead, "ahead", true, false)
	if got := len(sentMocks(out)); got != 1 {
		t.Fatalf("the window over the kept mock handed on %d mocks, want it", got)
	}
}

// TestAKeptWindowsMockBufferedWhileNoOutputIsBoundGoesToTheNextOutput: three
// mocks settle with the output bound. The output closes, as when a recording
// ends and the agent records again; a kept window is resolved over the second
// mock while none is bound, so the mock waits in the buffer for an output; and
// a new output is bound. The next flush hands it on to the kept window's test:
// a pass with no output bound leaves nothing settled, whatever was settled
// before it, since what it kept for the output a later pass must decide.
func TestAKeptWindowsMockBufferedWhileNoOutputIsBoundGoesToTheNextOutput(t *testing.T) {
	out := make(chan *models.Mock, 64)
	m := openWindowManager(out, make(chan models.TestMockMapping, 64))
	base := time.Now().Add(time.Hour)
	s1, s2, s3 := httpMockAt(base), httpMockAt(base.Add(10*time.Millisecond)), httpMockAt(base.Add(20*time.Millisecond))
	for _, mk := range []*models.Mock{s1, s2, s3} {
		m.AddMock(mk)
	}
	far := base.Add(time.Hour)
	m.ResolveRange(far, far, "elsewhere", true, false)
	if got := handedOn(out); len(got) != 0 {
		t.Fatalf("the resolve over no mock's request handed on %d mocks, want none", len(got))
	}
	m.mu.Lock()
	settled := m.settledLocked()
	m.mu.Unlock()
	if settled != 3 {
		t.Fatalf("%d of the 3 buffered mocks settled, want all", settled)
	}
	m.CloseOutChan()
	if got := handedOn(out); len(got) != 0 {
		t.Fatalf("closing the output handed on %d mocks, want none: no window holds any", len(got))
	}
	m.ResolveRange(base.Add(5*time.Millisecond), base.Add(15*time.Millisecond), "two", true, false)
	out2 := make(chan *models.Mock, 64)
	m.SetOutputChannel(out2)
	m.FlushOwnedWindows()
	if got := handedOn(out2); len(got) != 1 || got[0] != s2 {
		t.Fatalf("the flush after a new output was bound handed on %d mocks, want 1: the second, the kept window's", len(got))
	}
}
