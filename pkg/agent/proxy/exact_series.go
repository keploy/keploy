package proxy

import (
	"math"
	"sync"
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
// series of the staged set once per staging and kind, the way judging read
// them (learn.SeriesOfRange over every recording of the set: the trees' and
// the on-disk store's, RangeRecordedBefore), keeping in memory only the
// recordings of the kind that are background, and of them only those of a
// marked series once read.

// markedSeriesCache is the staged set's marked series, by kind, read at one
// staging (stagingEpoch) with one set of marks (SetUnrequired), one
// background (ServeExactBackground) and one on-disk source
// (SetRecordedBefore): any of them changing reads the series again.
type markedSeriesCache struct {
	mu     sync.Mutex
	epoch  uint64
	marks  *models.BackgroundMarks
	bg     *backgroundState
	gen    uint64 // moved by SetRecordedBefore
	byKind map[models.Kind]markedSeriesRead
}

type markedSeriesRead struct {
	series []integrations.MarkedSeries
	err    error
}

// forget drops the series read so far: the set's recordings changed
// (SetRecordedBefore).
func (c *markedSeriesCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.byKind = nil
}

// allRecorded is an instant after every recording: RangeRecordedBefore at it
// visits them all.
var allRecorded = time.Unix(0, math.MaxInt64)

// MarkedSeries implements integrations.MarkedSeriesReader: the staged set's
// series of kind whose every recording is background (a test's mark,
// SetUnrequired, or the worker scope or the pool, ServeExactBackground) and
// at least one marked, each with its recordings in the order of their calls.
// None with exact serving off, for a set served by today's rules or whose
// background is not named yet, and when nothing is marked. known is the
// first call's in a staging (MarkedSeriesReader).
func (m *MockManager) MarkedSeries(kind models.Kind, known learn.Known) ([]integrations.MarkedSeries, error) {
	if m.exactView() == nil {
		return nil, nil
	}
	bg := m.exact.bg.Load()
	marks := m.exact.unrequired.Load()
	if bg == nil || bg.settling || marks == nil || marks.Len() == 0 {
		return nil, nil
	}
	epoch := m.stagingEpoch.Load()
	c := &m.exact.series
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKind == nil || c.epoch != epoch || c.marks != marks || c.bg != bg {
		c.epoch, c.marks, c.bg = epoch, marks, bg
		c.byKind = map[models.Kind]markedSeriesRead{}
	}
	if r, ok := c.byKind[kind]; ok {
		return r.series, r.err
	}
	r := m.readMarkedSeries(kind, known, *marks, bg)
	c.byKind[kind] = r
	return r.series, r.err
}

// readMarkedSeries reads the staged set's marked series of kind
// (MarkedSeries) off every recording of the set.
func (m *MockManager) readMarkedSeries(kind models.Kind, known learn.Known, marks models.BackgroundMarks, bg *backgroundState) markedSeriesRead {
	marked := map[string]bool{}
	for _, byName := range marks {
		for name := range byName {
			marked[name] = true
		}
	}
	background := func(mk *models.Mock) bool { return marked[mk.Name] || bg.has(mk.Name) }
	set, err := learn.SeriesOfRange(func(visit func(*models.Mock) bool) error {
		return m.RangeRecordedBefore(allRecorded, visit)
	}, func(mk *models.Mock) bool { return mk.Kind == kind && background(mk) }, known)
	if err != nil {
		return markedSeriesRead{err: err}
	}
	var out []integrations.MarkedSeries
	for _, s := range set.Series {
		recs := make([]*models.Mock, 0, len(s.Members))
		anyMarked := false
		for _, i := range s.Members {
			mk := set.Mocks[i]
			if mk.Kind != kind || !background(mk) {
				recs = nil
				break
			}
			anyMarked = anyMarked || marked[mk.Name]
			recs = append(recs, mk)
		}
		if len(recs) > 0 && anyMarked {
			out = append(out, integrations.MarkedSeries{Series: s, Recordings: recs})
		}
	}
	return markedSeriesRead{series: out}
}

var _ integrations.MarkedSeriesReader = (*MockManager)(nil)
