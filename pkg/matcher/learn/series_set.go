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
	prev := previousCalls(recorded)
	type read struct {
		m *models.Mock
		r SeriesRead
	}
	var reads []read
	for _, m := range recorded {
		if m == nil || m.IsProtocolPhase() {
			continue
		}
		from := prev(m) // zero for a call of no request time
		if from.IsZero() {
			continue
		}
		shape, ok := SeriesShapeOf(m)
		if !ok {
			continue
		}
		reads = append(reads, read{m: m, r: SeriesRead{Shape: shape, Span: Span{From: from, To: m.Spec.ReqTimestampMock}}})
	}
	sort.SliceStable(reads, func(i, j int) bool {
		return reads[i].r.Span.To.Before(reads[j].r.Span.To)
	})
	out := SetSeries{Mocks: make([]*models.Mock, len(reads))}
	rs := make([]SeriesRead, len(reads))
	for i, r := range reads {
		out.Mocks[i], rs[i] = r.m, r.r
	}
	out.Series = SeriesOf(rs, known)
	return out
}

// previousCalls is PreviousCall for every recording of recorded at once:
// the request time of a recording's previous data-plane call among
// recorded, zero when it has none. The recordings are ordered by request
// time once, and each search walks back from its own call to the first
// sent before it that is no protocol phase and was answered by the time it
// was sent: the latest such call by request time, PreviousCall's.
func previousCalls(recorded []*models.Mock) func(*models.Mock) time.Time {
	byAt := make([]*models.Mock, 0, len(recorded))
	for _, m := range recorded {
		if m != nil && !m.IsProtocolPhase() {
			byAt = append(byAt, m) // one of no request time sorts first, as none
		}
	}
	sort.SliceStable(byAt, func(i, j int) bool {
		return byAt[i].Spec.ReqTimestampMock.Before(byAt[j].Spec.ReqTimestampMock)
	})
	return func(paired *models.Mock) time.Time {
		if paired == nil {
			return time.Time{}
		}
		// Sent strictly before paired: never paired itself (MockID holds
		// its request time), and nothing for one of no request time.
		to := paired.Spec.ReqTimestampMock
		i := sort.Search(len(byAt), func(i int) bool { return !byAt[i].Spec.ReqTimestampMock.Before(to) })
		for i--; i >= 0; i-- {
			m := byAt[i]
			// Answered after paired was sent: pending across its send,
			// another flow's call. One whose answer time is not known
			// (zero) still counts, as PreviousCall's.
			if m.Spec.ResTimestampMock.After(to) {
				continue
			}
			return m.Spec.ReqTimestampMock
		}
		return time.Time{}
	}
}
