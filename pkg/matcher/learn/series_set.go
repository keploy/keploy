package learn

import (
	"fmt"
	"sort"
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
// step's window for the call (Window.Rec). Not read: a protocol-phase
// recording (a handshake, a driver's heartbeat: no call of the
// application's), and one with no request time or no previous call, whose
// reads of the clock no span bounds (the set's first call): a series can
// check no read of its, so it is in no series, and judging and the agent
// treat it as they did. known excuses leaves as SeriesOf's.
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

// SeriesOfRange is SeriesOfSet over the recordings rng visits (each once, in
// any order; rng's error is returned, with no series), for a reader that
// never holds a set's recordings at once (keploy's agent, whose store keeps
// them on disk and decodes one per visit): each recording is read into its
// shape and its call's times as it is visited, and kept only when keep says
// so. SetSeries.Mocks holds a kept recording as visited, and any other as a
// stand-in that carries only its name, kind and times: never one to answer a
// call with. The series are SeriesOfSet's, whatever keep says.
func SeriesOfRange(rng func(visit func(*models.Mock) bool) error, keep func(*models.Mock) bool, known Known) (SetSeries, error) {
	type read struct {
		m     *models.Mock
		shape Shape
	}
	var calls []callTimes
	var reads []read
	err := rng(func(m *models.Mock) bool {
		if m == nil || m.IsProtocolPhase() {
			return true
		}
		calls = append(calls, callTimes{req: m.Spec.ReqTimestampMock, res: m.Spec.ResTimestampMock})
		if m.Spec.ReqTimestampMock.IsZero() {
			return true
		}
		shape, ok := SeriesShapeOf(m)
		if !ok {
			return true
		}
		if !keep(m) {
			m = standIn(m)
		}
		reads = append(reads, read{m: m, shape: shape})
		return true
	})
	if err != nil {
		return SetSeries{}, err
	}
	prev := previousOf(calls)
	type spanned struct {
		m *models.Mock
		r SeriesRead
	}
	var rs []spanned
	for _, r := range reads {
		from := prev(r.m.Spec.ReqTimestampMock)
		if from.IsZero() {
			continue
		}
		rs = append(rs, spanned{m: r.m, r: SeriesRead{Shape: r.shape, Span: Span{From: from, To: r.m.Spec.ReqTimestampMock}}})
	}
	sort.SliceStable(rs, func(i, j int) bool {
		return rs[i].r.Span.To.Before(rs[j].r.Span.To)
	})
	out := SetSeries{Mocks: make([]*models.Mock, len(rs))}
	srs := make([]SeriesRead, len(rs))
	for i, r := range rs {
		out.Mocks[i], srs[i] = r.m, r.r
	}
	out.Series = SeriesOf(srs, known)
	return out, nil
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
