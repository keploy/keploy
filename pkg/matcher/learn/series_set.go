package learn

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A SET'S SERIES, AS JUDGING AND THE AGENT BOTH READ THEM.
//
// Judging marks a series' recordings as the application's background work,
// and the agent answers a call of a marked series from them: both must read
// the same recordings into the same series, or a call judging marked would
// meet a series the agent never built. Each reads a set's recordings with
// SetSeries, every recording by its kind's one reading (RegisterSeriesShape)
// and in its call's own span on the recording's clock, as the learn step
// reads it (PreviousCall, Window.Rec).

// SeriesShape reads a recording's request as the learn step reads it (its
// values typed: an Instant leaf where its decoder types the field as one),
// for SetSeries. ok is false for a recording it cannot read.
type SeriesShape func(m *models.Mock) (Shape, bool)

var seriesShapes = struct {
	mu     sync.RWMutex
	byKind map[models.Kind]SeriesShape
}{byKind: map[models.Kind]SeriesShape{}}

// RegisterSeriesShape declares how kind's parser reads its recordings for
// series (SeriesShape): the reading its learn step and its exact serving
// compare requests by, so that a call the agent reads is read as judging
// read its recordings. A kind nobody registers has no series: none of its
// recordings is read by SetSeries, and judging and the agent treat each as
// they did. Call it from the parser's init function. One registration per
// kind: a second panics, as does a nil of. The returned function removes it
// (tests).
func RegisterSeriesShape(kind models.Kind, of SeriesShape) (unregister func()) {
	if of == nil {
		panic(fmt.Sprintf("learn: RegisterSeriesShape(%q) with a nil reading", kind))
	}
	seriesShapes.mu.Lock()
	defer seriesShapes.mu.Unlock()
	if _, ok := seriesShapes.byKind[kind]; ok {
		panic(fmt.Sprintf("learn: RegisterSeriesShape(%q) twice", kind))
	}
	seriesShapes.byKind[kind] = of
	return func() {
		seriesShapes.mu.Lock()
		defer seriesShapes.mu.Unlock()
		delete(seriesShapes.byKind, kind)
	}
}

// SeriesShapeOf is m's request as its kind's registered reading reads it
// (RegisterSeriesShape): false when none is registered for its kind, or it
// cannot read m.
func SeriesShapeOf(m *models.Mock) (Shape, bool) {
	if m == nil {
		return Shape{}, false
	}
	seriesShapes.mu.RLock()
	of, ok := seriesShapes.byKind[m.Kind]
	seriesShapes.mu.RUnlock()
	if !ok {
		return Shape{}, false
	}
	return of(m)
}

// SetSeries is a set's recordings read into series (SeriesOf): Series, with
// each Members index naming a recording of Mocks, the recordings read, in
// the order of their calls (by request time; the newest last).
type SetSeries struct {
	Series []Series
	Mocks  []*models.Mock
}

// SeriesOfSet reads recorded, a set's recordings (every one of them: the
// previous call of one may be of any kind), into series. A recording is read
// when its kind's reading reads it (SeriesShapeOf) and its call has a span
// on the recording's clock: from its previous data-plane call (PreviousCall,
// over every recording of recorded) to its own request time, as the learn
// step's window for the call (Window.Rec); the set's first call, which no
// previous call bounds, in a span with no start, which bounds its reads of
// the clock at its end alone (EpochTrack): it is a recording of its call's
// series like any other, so a test whose miss is closest to it varied in the
// series as one closest to a later copy does. Not read: a protocol-phase
// recording (a handshake, a driver's heartbeat: no call of the
// application's), and one with no request time, whose reads of the clock no
// span bounds: a series can check no read of its, so it is in no series, and
// judging and the agent treat it as they did. known excuses leaves as
// SeriesOf's.
func SeriesOfSet(recorded []*models.Mock, known Known) SetSeries {
	out, _ := SeriesOfRange(func(visit func(*models.Mock) bool) error {
		for _, m := range recorded {
			if !visit(m) {
				break
			}
		}
		return nil
	}, func(*models.Mock) bool { return true }, known)
	return out
}

// SeriesOfRange is SeriesOfSet over the recordings rng visits, for a reader
// that never holds a set's recordings at once (keploy's agent, whose store
// keeps them on disk and decodes one per visit), keeping only what keep
// keeps: ReadSetRange's reads, read into series with known. The series that
// hold a kept recording are SeriesOfSet's, whatever keep says; no other is
// read. rng's error is returned, with no series.
func SeriesOfRange(rng func(visit func(*models.Mock) bool) error, keep func(*models.Mock) bool, known Known) (SetSeries, error) {
	r, err := ReadSetRange(rng, keep)
	if err != nil {
		return SetSeries{}, err
	}
	return r.Series(known), nil
}

// SetReads is a set's recordings read for series (ReadSetRange), before any
// noise excuses a leaf: Reads, in the order of their calls (the newest last),
// each of the recording Mocks holds at its index.
type SetReads struct {
	Reads []SeriesRead
	Mocks []*models.Mock
}

// Series reads r into series (SeriesOf) with known.
func (r SetReads) Series(known Known) SetSeries {
	return SetSeries{Series: SeriesOf(r.Reads, known), Mocks: r.Mocks}
}

// OfKind is r's reads of kind alone, in their order.
func (r SetReads) OfKind(kind models.Kind) SetReads {
	var out SetReads
	for i, rd := range r.Reads {
		if rd.Kind == kind {
			out.Reads = append(out.Reads, rd)
			out.Mocks = append(out.Mocks, r.Mocks[i])
		}
	}
	return out
}

// ReadSetRange reads the recordings rng visits as SeriesOfSet reads a set's,
// holding in memory only the requests of the operations a recording keep
// keeps has (its kind, key and fixed parts: SeriesOf parts recordings by
// them first, so no other recording changes a kept one's series), so a
// set's other calls, however many and however large, are never in memory
// together (founder rule: no component OOMs). rng is called once or twice,
// and visits the same recordings each time, each once, in any order; keep
// says the same of a recording each time. The first pass reads every call's
// times and the requests keep keeps; the second, only when a recording it
// does not keep has a kind with a reading, the requests of the others of a
// kept one's operation. Mocks holds a kept recording as visited, and any
// other read as a stand-in that carries only its name, kind and times: never
// one to answer a call with. rng's error is returned, with no reads.
func ReadSetRange(rng func(visit func(*models.Mock) bool) error, keep func(*models.Mock) bool) (SetReads, error) {
	type read struct {
		m     *models.Mock
		shape Shape
	}
	var calls []callTimes
	var reads []read
	ops := map[string]bool{}
	others := false
	err := rng(func(m *models.Mock) bool {
		if m == nil || m.IsProtocolPhase() {
			return true
		}
		calls = append(calls, callTimes{req: m.Spec.ReqTimestampMock, res: m.Spec.ResTimestampMock})
		if m.Spec.ReqTimestampMock.IsZero() || !hasSeriesShape(m.Kind) {
			return true
		}
		if !keep(m) {
			others = true
			return true
		}
		shape, ok := SeriesShapeOf(m)
		if !ok {
			return true
		}
		ops[operationOf(m.Kind, shape)] = true
		reads = append(reads, read{m: m, shape: shape})
		return true
	})
	if err != nil {
		return SetReads{}, err
	}
	if others && len(ops) > 0 {
		err = rng(func(m *models.Mock) bool {
			if m == nil || m.IsProtocolPhase() || m.Spec.ReqTimestampMock.IsZero() || !hasSeriesShape(m.Kind) || keep(m) {
				return true
			}
			if shape, ok := SeriesShapeOf(m); ok && ops[operationOf(m.Kind, shape)] {
				reads = append(reads, read{m: standIn(m), shape: shape})
			}
			return true
		})
		if err != nil {
			return SetReads{}, err
		}
	}
	prev := previousOf(calls)
	type spanned struct {
		m *models.Mock
		r SeriesRead
	}
	var rs []spanned
	for _, r := range reads {
		// No previous call (the set's first): a span with no start, which
		// bounds the read at its end alone (EpochTrack).
		from := prev(r.m.Spec.ReqTimestampMock)
		rs = append(rs, spanned{m: r.m, r: SeriesRead{Kind: r.m.Kind, Shape: r.shape, Span: Span{From: from, To: r.m.Spec.ReqTimestampMock}}})
	}
	// In the order of their calls; two sent at one instant by kind and
	// name, so the order is the set's, whichever pass or order read them.
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if !a.r.Span.To.Equal(b.r.Span.To) {
			return a.r.Span.To.Before(b.r.Span.To)
		}
		if a.m.Kind != b.m.Kind {
			return a.m.Kind < b.m.Kind
		}
		return a.m.Name < b.m.Name
	})
	out := SetReads{Reads: make([]SeriesRead, len(rs)), Mocks: make([]*models.Mock, len(rs))}
	for i, r := range rs {
		out.Mocks[i], out.Reads[i] = r.m, r.r
	}
	return out, nil
}

// hasSeriesShape reports whether kind's parser registered a reading.
func hasSeriesShape(kind models.Kind) bool {
	seriesShapes.mu.RLock()
	defer seriesShapes.mu.RUnlock()
	_, ok := seriesShapes.byKind[kind]
	return ok
}

// operationOf is what recordings of one operation share whatever noise
// excuses (SeriesOf parts by it first): kind, key and fixed parts.
func operationOf(kind models.Kind, s Shape) string {
	parts := make([]string, 0, len(s.Fixed))
	for n, v := range s.Fixed {
		parts = append(parts, n+"\x00"+v)
	}
	sort.Strings(parts)
	return string(kind) + "\x02" + s.Key + "\x01" + strings.Join(parts, "\x01")
}

// standIn is m as SeriesOfRange keeps a recording it was not asked to keep:
// its name, kind and call times alone.
func standIn(m *models.Mock) *models.Mock {
	s := &models.Mock{Name: m.Name, Kind: m.Kind}
	s.Spec.ReqTimestampMock, s.Spec.ResTimestampMock = m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
	return s
}

// callTimes is a data-plane call's request and answer times, all
// previousOf reads of a recording.
type callTimes struct{ req, res time.Time }

// previousCalls is PreviousCall for every recording of recorded at once:
// the request time of a recording's previous data-plane call among
// recorded, zero when it has none (previousOf).
func previousCalls(recorded []*models.Mock) func(*models.Mock) time.Time {
	var calls []callTimes
	for _, m := range recorded {
		if m != nil && !m.IsProtocolPhase() {
			calls = append(calls, callTimes{req: m.Spec.ReqTimestampMock, res: m.Spec.ResTimestampMock})
		}
	}
	prev := previousOf(calls)
	return func(paired *models.Mock) time.Time {
		if paired == nil {
			return time.Time{}
		}
		return prev(paired.Spec.ReqTimestampMock)
	}
}

// previousOf is, for a call sent at to, the request time of its previous
// call among calls (each a data-plane call's times), zero when it has none.
// The calls are ordered by request time once, and each search walks back
// from to to the first call sent before it that was answered by the time it
// was sent: the latest such call by request time, PreviousCall's.
func previousOf(calls []callTimes) func(to time.Time) time.Time {
	byAt := append([]callTimes(nil), calls...) // one of no request time sorts first, as none
	sort.SliceStable(byAt, func(i, j int) bool { return byAt[i].req.Before(byAt[j].req) })
	return func(to time.Time) time.Time {
		// Sent strictly before to: never the call itself (MockID holds its
		// request time), and nothing for one of no request time.
		if to.IsZero() {
			return time.Time{}
		}
		i := sort.Search(len(byAt), func(i int) bool { return !byAt[i].req.Before(to) })
		for i--; i >= 0; i-- {
			c := byAt[i]
			// Answered after the call was sent: pending across its send,
			// another flow's call. One whose answer time is not known
			// (zero) still counts, as PreviousCall's.
			if c.res.After(to) {
				continue
			}
			return c.req
		}
		return time.Time{}
	}
}
