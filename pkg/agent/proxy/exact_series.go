package proxy

import (
	"context"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A TIMER'S CALL JUDGING MARKED SET-WIDE: ITS SERIES, AS THE PARSERS ASK.
//
// Judging marks a recurring call's every recording as the application's
// background work (models.MockEntry.Background on each test's list that has
// one; the rest are the worker scope's or the pool's): its copies at replay
// carry other reads of the clock, and come as often as the replay's timing
// makes them. A parser answers such a call from the series judging read the
// recordings into (integrations.MarkedSeriesReader). The manager reads the
// staged set's recordings once per staging, as it is staged and on a
// goroutine of its own (beginMarkedSeries), so no call pays for the read, the
// way judging reads them (learn.ReadSetRange over every recording of the set:
// the trees' and the on-disk store's, RangeRecordedBefore), keeping in memory
// only the background recordings and the requests of their operations; each
// kind's series are learned from them at its first call, with its known.

// markedSeriesCache is the read of the staged set's series last begun
// (beginMarkedSeries): at most one read is pending or held at a time.
type markedSeriesCache struct {
	mu   sync.Mutex
	gen  uint64 // moved by SetRecordedBefore
	read *setSeriesRead
}

// seriesKey is what one read of the set's series reads: one staging
// (stagingEpoch), one set of marks (SetUnrequired), one background
// (ServeExactBackground) and one on-disk source (SetRecordedBefore). Any of
// them changing begins another read.
type seriesKey struct {
	epoch uint64
	marks *models.BackgroundMarks
	bg    *backgroundState
	gen   uint64
}

// setSeriesRead is one read of the staged set's recordings into series,
// begun at staging on a goroutine of its own, so no call pays for it: a call
// that comes before it ends waits for it (done), with no budget. reads and
// err are set before done closes; stopped is set when another read takes its
// place before it ends, and its reads are then no set's.
type setSeriesRead struct {
	key     seriesKey
	done    chan struct{}
	stop    atomic.Bool
	stopped bool
	reads   learn.SetReads
	err     error
	marked  map[string]bool
	series  map[string]bool // marked as a series' recording on some list (models.IsBackgroundSeries)
	// own is, for a recording marked as a test's own series' on a list
	// (models.IsBackgroundOwnSeries), that test; "" when another list
	// marks it too (another test's own series, or any other mark): it then
	// owns no series (seriesOf), failing closed.
	own map[string]string
	// ownIDs is, for a recording in own, the paths of the id its own
	// series' marks name (models.BackgroundOwnSeriesIDs); nil when one names
	// none, or two name two sets of paths: then no id of its series is
	// given back.
	ownIDs map[string][]string

	mu     sync.Mutex
	byKind map[models.Kind][]integrations.MarkedSeries // the first call's known, per kind
	once   map[string]bool                             // one-offs that answered set-wide (MarkedSeries.Once)
}

// forget drops the series read so far: the set's recordings changed
// (SetRecordedBefore).
func (c *markedSeriesCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	if c.read != nil {
		c.read.stop.Store(true)
		c.read = nil
	}
}

// allRecorded is an instant after every recording: RangeRecordedBefore at it
// visits them all.
var allRecorded = time.Unix(0, math.MaxInt64)

// beginMarkedSeries begins reading the staged set's series as soon as all
// they are read from is named (seriesReadLocked): called wherever any of it
// changes, so the read is under way before the set's first call asks.
func (m *MockManager) beginMarkedSeries() {
	c := &m.exact.series
	c.mu.Lock()
	defer c.mu.Unlock()
	m.seriesReadLocked()
}

// seriesReadLocked is the read of the staged set's series for what is named
// now, begun here when the last one read something else; nil while the set's
// mocks are being staged (StageExactBackground until ServeExactBackground:
// the trees may still hold the previous set's recordings, whose names may be
// this set's), with the switch off, and when nothing is marked: then no read
// is pending. Holds c.mu.
//
// A set served by today's rules is read too: judging marks a series in any
// set it judges (keploy mock record's sets carry no routing mark, so they
// are all served by today's rules), and answering a call of it from its
// newest recording reads none of the background rules. Its background is
// then the marks alone: today's rules name no pool or worker scope, so a
// series one of whose recordings is on no test's list is no marked series
// there (background), and its calls go on as before.
func (m *MockManager) seriesReadLocked() *setSeriesRead {
	c := &m.exact.series
	bg := m.exact.bg.Load()
	marks := m.exact.unrequired.Load()
	if !provenswitch.On() || m.exact.staging.Load() || bg != nil && bg.settling || marks == nil || marks.Len() == 0 {
		if c.read != nil {
			c.read.stop.Store(true)
			c.read = nil
		}
		return nil
	}
	key := seriesKey{epoch: m.stagingEpoch.Load(), marks: marks, bg: bg, gen: c.gen}
	if c.read != nil && c.read.key == key {
		return c.read
	}
	if c.read != nil {
		c.read.stop.Store(true)
	}
	r := &setSeriesRead{key: key, done: make(chan struct{}), marked: map[string]bool{}, series: map[string]bool{},
		own: map[string]string{}, ownIDs: map[string][]string{}, byKind: map[models.Kind][]integrations.MarkedSeries{}, once: map[string]bool{}}
	plain := map[string]bool{}
	for test, byName := range *marks {
		for name, why := range byName {
			r.marked[name] = true
			switch {
			case models.IsBackgroundOwnSeries(why):
				ids, _ := models.BackgroundOwnSeriesIDs(why)
				// A test marks a recording once (marks is test -> name), so a
				// second sight of name is another test's: no test's own.
				if _, seen := r.own[name]; seen {
					r.own[name] = ""
				} else {
					r.own[name] = test
					r.ownIDs[name] = ids
				}
			case models.IsBackgroundSeries(why):
				r.series[name] = true
				plain[name] = true
			default:
				plain[name] = true
			}
		}
	}
	for name := range r.own {
		if plain[name] {
			r.own[name] = ""
		}
	}
	c.read = r
	go m.readSetSeries(r)
	return r
}

// background reports whether mk is the set's background in r's read: a
// test's mark (SetUnrequired) or, under the background rules, the worker
// scope's or the pool's (today's rules name neither).
func (r *setSeriesRead) background(mk *models.Mock) bool {
	return r.marked[mk.Name] || r.key.bg.has(mk.Name)
}

// readSetSeries reads the staged set's recordings for r: every recording of
// the set (the trees' and the on-disk store's, RangeRecordedBefore), as
// judging reads them (learn.ReadSetRange), keeping in memory only the
// background ones and the requests of their operations. It stops at the
// next recording once another read takes r's place.
func (m *MockManager) readSetSeries(r *setSeriesRead) {
	defer close(r.done)
	r.reads, r.err = learn.ReadSetRange(func(visit func(*models.Mock) bool) error {
		return m.RangeRecordedBefore(allRecorded, func(mk *models.Mock) bool {
			return !r.stop.Load() && visit(mk)
		})
	}, r.background)
	r.stopped = r.stop.Load()
}

// MarkedSeries implements integrations.MarkedSeriesReader: the staged set's
// series of kind whose every recording is background (a test's mark,
// SetUnrequired, or, under the background rules, the worker scope or the
// pool, ServeExactBackground) and at least one marked, each with its
// recordings in the order of their calls, whichever rules serve the set.
// None with exact serving or the switch off, while the set's mocks are being
// staged, and when nothing is marked. The set's
// recordings were read as it was staged (beginMarkedSeries); a call that
// comes while they are still being read waits for them, or for ctx. known is
// the first call's of kind in a staging (MarkedSeriesReader).
//
// A test's own series (integrations.MarkedSeries.Owner) is handed only while
// its test's window is open as the call asks: in another test's window, or
// between tests, its call is compared as any other.
func (m *MockManager) MarkedSeries(ctx context.Context, kind models.Kind, known learn.Known) ([]integrations.MarkedSeries, error) {
	v := m.exactView()
	if v == nil {
		return nil, nil
	}
	s, err := m.seriesFrom(ctx, m.currentSeriesRead(), kind, known)
	return integrations.OwnedBy(s, v.test), err
}

// currentSeriesRead is the read of the staged set's series for what is
// named now (seriesReadLocked).
func (m *MockManager) currentSeriesRead() *setSeriesRead {
	c := &m.exact.series
	c.mu.Lock()
	defer c.mu.Unlock()
	return m.seriesReadLocked()
}

// seriesFrom is MarkedSeries once r, a read of the set's series, ends: its
// series of kind, or, when another read took its place before it ended (its
// reads are no set's), the current read's.
func (m *MockManager) seriesFrom(ctx context.Context, r *setSeriesRead, kind models.Kind, known learn.Known) ([]integrations.MarkedSeries, error) {
	for r != nil {
		select {
		case <-r.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if r.stopped {
			r = m.currentSeriesRead()
			continue
		}
		if r.err != nil {
			return nil, r.err
		}
		return r.seriesOf(kind, known), nil
	}
	return nil, nil
}

// seriesOf is r's marked series of kind (MarkedSeries), learned with the
// first call's known and kept for the rest.
func (r *setSeriesRead) seriesOf(kind models.Kind, known learn.Known) []integrations.MarkedSeries {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byKind[kind]; ok {
		return s
	}
	set := r.reads.OfKind(kind).Series(known)
	var out []integrations.MarkedSeries
	for _, s := range set.Series {
		recs := make([]*models.Mock, 0, len(s.Members))
		anyMarked, again := false, false
		owner, owned, mixed := "", 0, false
		var ownIDs []string
		for _, i := range s.Members {
			mk := set.Mocks[i]
			if !r.background(mk) {
				recs = nil
				break
			}
			anyMarked = anyMarked || r.marked[mk.Name]
			again = again || r.series[mk.Name]
			if o, ok := r.own[mk.Name]; ok {
				if owned == 0 {
					owner, ownIDs = o, r.ownIDs[mk.Name]
				}
				mixed = mixed || o != owner
				if !slices.Equal(r.ownIDs[mk.Name], ownIDs) {
					ownIDs = nil // two sets of paths named: give no id back
				}
				owned++
			}
			recs = append(recs, mk)
		}
		switch {
		case len(recs) == 0 || !anyMarked:
		case owned == 0:
			out = append(out, integrations.MarkedSeries{Series: s, Recordings: recs, Again: again, Once: r.answerOnce})
		case owned == len(recs) && !mixed && owner != "":
			// A test's own series: answered again in its window alone.
			out = append(out, integrations.MarkedSeries{Series: s, Recordings: recs, Again: true, Owner: owner, OwnIDs: ownIDs})
		default:
			// Some of it is a test's own series and some is not (another
			// test's, a series marked set-wide, a one-off, the pool's):
			// none of it is answered as marked, failing closed.
		}
	}
	r.byKind[kind] = out
	return out
}

// answerOnce is MarkedSeries.Once for r's one-offs: whether name had not
// answered a call set-wide in r's staging, recording that it now has. It
// takes nothing from a test's own recordings. A read that another takes the
// place of (a new staging, new marks, the store's recordings changed) starts
// with none answered.
func (r *setSeriesRead) answerOnce(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.once[name] {
		return false
	}
	r.once[name] = true
	return true
}

// keepSeriesRead keeps, as h's own (heldScope.series), the read of the
// set's series current now while it is of h's call's staging: the read
// another of the call's staging takes the place of (new marks, the store's
// recordings changed), and none when nothing is marked in it. A read of a
// later staging is never kept: a call made in one test set is never handed
// the series of the set the replay staged after it (MarkedSeries). Called
// as h is held (HoldToRound), so a call whose staging moves before it asks
// still has its own staging's read, and as it asks.
func (h *heldScope) keepSeriesRead() {
	r := h.currentSeriesRead()
	switch {
	case r != nil && r.key.epoch == h.g.at.Epoch:
		h.series = r
	case r == nil && h.stagingEpoch.Load() == h.g.at.Epoch:
		h.series = nil
	}
}

// MarkedSeries is the manager's (integrations.MarkedSeriesReader) for h's
// call: its own staging's series (keepSeriesRead), read whole. Once the
// replay staged a test set after the call's mark (the next set, or this one
// again), they are handed as that staging's (integrations.MarkedSeries.
// Restaged), answering no call: recordings' names restart in each set, and
// a call is never answered from the series of a set it was not made in, nor
// charged to that set's test. A staging whose read another read took the
// place of before it ended (the next staging's) hands none: its series were
// never read whole, and the call goes on as any other call the replay
// staged a set under. A test's own series is handed only to a call read in
// its test's window (the view the call was read in, SequenceView): a call
// read there and asking after the window closed is still its test's.
func (h *heldScope) MarkedSeries(ctx context.Context, kind models.Kind, known learn.Known) ([]integrations.MarkedSeries, error) {
	if h.exactView() == nil {
		return nil, nil
	}
	sv, _ := h.SequenceView()
	s, err := h.markedSeries(ctx, kind, known)
	return integrations.OwnedBy(s, sv.Test), err
}

// markedSeries is MarkedSeries, every owner's own series included.
func (h *heldScope) markedSeries(ctx context.Context, kind models.Kind, known learn.Known) ([]integrations.MarkedSeries, error) {
	for {
		h.keepSeriesRead()
		r := h.series
		if r == nil {
			h.moved()
			return nil, nil
		}
		select {
		case <-r.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if r.stopped {
			// Its reads are no set's: within the call's staging the read
			// that took its place is kept next; after a staging, none is.
			h.series = nil
			continue
		}
		if r.err != nil {
			if h.moved() {
				return nil, nil
			}
			return nil, r.err
		}
		s := r.seriesOf(kind, known)
		if !h.moved() {
			return s, nil
		}
		return restagedSeries(s), nil
	}
}

// restagedSeries is s, a call's staging's series, handed after the replay
// staged a test set under the call (integrations.MarkedSeries.Restaged):
// none answers a call, also for a parser that does not tell the mark, as a
// recording neither answered again (Again) nor spent once (Once) answers
// none.
func restagedSeries(s []integrations.MarkedSeries) []integrations.MarkedSeries {
	out := make([]integrations.MarkedSeries, len(s))
	for i, m := range s {
		m.Restaged, m.Again, m.Once = true, false, nil
		out[i] = m
	}
	return out
}

var _ integrations.MarkedSeriesReader = (*MockManager)(nil)
