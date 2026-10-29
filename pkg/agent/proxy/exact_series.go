package proxy

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
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
// now, begun here when the last one read something else; nil while the set
// is served by today's rules or its background is not named yet (settling),
// and when nothing is marked: then no read is pending. Holds c.mu.
func (m *MockManager) seriesReadLocked() *setSeriesRead {
	c := &m.exact.series
	bg := m.exact.bg.Load()
	marks := m.exact.unrequired.Load()
	if bg == nil || bg.settling || marks == nil || marks.Len() == 0 {
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
		byKind: map[models.Kind][]integrations.MarkedSeries{}, once: map[string]bool{}}
	for _, byName := range *marks {
		for name, why := range byName {
			r.marked[name] = true
			if models.IsBackgroundSeries(why) {
				r.series[name] = true
			}
		}
	}
	c.read = r
	go m.readSetSeries(r)
	return r
}

// background reports whether mk is the set's background in r's read: a
// test's mark (SetUnrequired) or the worker scope's or the pool's.
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
// SetUnrequired, or the worker scope or the pool, ServeExactBackground) and
// at least one marked, each with its recordings in the order of their calls.
// None with exact serving off, for a set served by today's rules or whose
// background is not named yet, and when nothing is marked. The set's
// recordings were read as it was staged (beginMarkedSeries); a call that
// comes while they are still being read waits for them, or for ctx. known is
// the first call's of kind in a staging (MarkedSeriesReader).
func (m *MockManager) MarkedSeries(ctx context.Context, kind models.Kind, known learn.Known) ([]integrations.MarkedSeries, error) {
	if m.exactView() == nil {
		return nil, nil
	}
	return m.seriesFrom(ctx, m.currentSeriesRead(), kind, known)
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
		for _, i := range s.Members {
			mk := set.Mocks[i]
			if !r.background(mk) {
				recs = nil
				break
			}
			anyMarked = anyMarked || r.marked[mk.Name]
			again = again || r.series[mk.Name]
			recs = append(recs, mk)
		}
		if len(recs) > 0 && anyMarked {
			out = append(out, integrations.MarkedSeries{Series: s, Recordings: recs, Again: again, Once: r.answerOnce})
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

var _ integrations.MarkedSeriesReader = (*MockManager)(nil)
