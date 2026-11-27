package replay

import (
	"context"
	"slices"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const (
	// WindowDerivedStartupReason is written on startup: entries derived from
	// mocks recorded strictly before the first recorded test window when
	// creating a new mappings.yaml.
	WindowDerivedStartupReason = "recorded before the first recorded test window (window-derived startup); remove this entry from startup: in mappings.yaml to stop treating matching misses as background"

	// WindowDerivedWorkerReason is written on worker: entries derived from
	// mocks recorded outside every recorded test window (between or after test
	// windows) when creating a new mappings.yaml.
	WindowDerivedWorkerReason = "recorded outside every recorded test window (window-derived background); remove this entry from worker: in mappings.yaml to stop treating matching misses as background"

	windowDerivedMaxPerShape = 8
	windowDerivedMaxPerTier  = 256
)

// windowBgTracker observes mocks during RunTestSet's already-existing single
// pass over mocks.yaml (loadMocksForAgent or readMockLookup), keeping a tiny
// bounded set of outside-window startup/worker candidates in O(1) memory so
// persisting them on a newly created mappings.yaml causes ZERO extra decodes
// of the mock file.
type windowBgTracker struct {
	classifier *models.WindowClassifier
	firstStart time.Time
	seen       map[string]struct{}
	startupCol *windowBgCollector
	workerCol  *windowBgCollector
	sawPass    bool
}

func newWindowBgTracker(testCases []*models.TestCase) *windowBgTracker {
	windows := recordedTestWindows(testCases)
	if len(windows) == 0 {
		return nil
	}
	return &windowBgTracker{
		classifier: models.NewWindowClassifier(windows),
		firstStart: firstRecordedTestStart(testCases),
		seen:       make(map[string]struct{}),
		startupCol: newWindowBgCollector(windowDerivedMaxPerTier, windowDerivedMaxPerShape, WindowDerivedStartupReason),
		workerCol:  newWindowBgCollector(windowDerivedMaxPerTier, windowDerivedMaxPerShape, WindowDerivedWorkerReason),
	}
}

func (t *windowBgTracker) markPass() {
	if t != nil {
		t.sawPass = true
	}
}

func (t *windowBgTracker) observe(m *models.Mock) {
	if t == nil || m == nil || m.Name == "" {
		return
	}
	if _, dup := t.seen[m.Name]; dup {
		return
	}
	t.seen[m.Name] = struct{}{}
	if !t.classifier.IsWorker(m) {
		return
	}
	if !t.firstStart.IsZero() && m.Spec.ReqTimestampMock.Before(t.firstStart) {
		t.startupCol.add(m)
	} else {
		t.workerCol.add(m)
	}
}

func (t *windowBgTracker) observeSlice(mocks []*models.Mock) {
	if t == nil {
		return
	}
	t.sawPass = true
	for _, m := range mocks {
		t.observe(m)
	}
}

// apply merges the observed outside-window startup/worker entries into
// mappingDoc, excluding any mock owned by a test case in mappingDoc or actual
// and any mock already listed in mappingDoc.Startup or mappingDoc.Worker.
func (t *windowBgTracker) apply(mappingDoc, actual *models.Mapping) {
	if t == nil || mappingDoc == nil {
		return
	}
	exclude := make(map[string]bool)
	for _, doc := range []*models.Mapping{mappingDoc, actual} {
		if doc == nil {
			continue
		}
		for _, tc := range doc.TestCases {
			for _, m := range tc.Mocks {
				if m.Name != "" {
					exclude[m.Name] = true
				}
			}
		}
	}
	for _, m := range mappingDoc.Startup {
		if m.Name != "" {
			exclude[m.Name] = true
		}
	}
	for _, m := range mappingDoc.Worker {
		if m.Name != "" {
			exclude[m.Name] = true
		}
	}

	if added := filterExcludedEntries(t.startupCol.entries(), exclude); len(added) > 0 {
		mappingDoc.Startup = append(mappingDoc.Startup, added...)
		sortByRecordedTime(mappingDoc.Startup)
	}
	if added := filterExcludedEntries(t.workerCol.entries(), exclude); len(added) > 0 {
		mappingDoc.Worker = append(mappingDoc.Worker, added...)
		sortByRecordedTime(mappingDoc.Worker)
	}
}

func filterExcludedEntries(entries []models.MockEntry, exclude map[string]bool) []models.MockEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]models.MockEntry, 0, len(entries))
	for _, e := range entries {
		if !exclude[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

// populateWindowDerivedBackground populates mappingDoc.Startup and
// mappingDoc.Worker with bounded, legible window-derived background entries
// when creating a NEW mappings.yaml (never overwriting or re-adding entries on
// an existing mappings.yaml).
//
// Only mocks classified strictly outside every recorded test window by
// models.NewWindowClassifier(windows) (and not owned by any test in mappingDoc
// or actual) are admitted:
//   - mocks recorded before firstRecordedTestStart -> Startup (with WindowDerivedStartupReason)
//   - mocks recorded between or after test windows -> Worker (with WindowDerivedWorkerReason)
//
// Persisting these entries when Pass 1 / the first replay creates mappings.yaml
// closes the Pass 1 -> Pass 2 amnesia where Pass 1 derived outside-window
// workers in RAM (DeriveBackgroundFromWindows=true) but wrote an empty worker:
// list, causing Pass 2 (and subsequent cloud replays) to drop all between-window
// background recordings.
func (r *Replayer) populateWindowDerivedBackground(ctx context.Context, testSetID string, testCases []*models.TestCase, mappingDoc, actual *models.Mapping) {
	if r.mockDB == nil || mappingDoc == nil || len(testCases) == 0 {
		return
	}
	tracker := newWindowBgTracker(testCases)
	if tracker == nil {
		return
	}
	err := r.eachSetMock(ctx, testSetID, true, func(m *models.Mock, _ bool) {
		tracker.observe(m)
	})
	if err != nil {
		r.logger.Debug("failed to stream mocks while deriving window background entries for new mappings.yaml",
			zap.String("testSetID", testSetID), zap.Error(err))
		return
	}
	tracker.apply(mappingDoc, actual)
}

type windowBgItem struct {
	entry models.MockEntry
	at    time.Time
	shape string
}

// windowBgCollector keeps a bounded, per-shape-capped set of MockEntries
// ordered by recorded ReqTimestampMock (keeping the latest maxPerShape entries
// per canonical request shape and evicting the oldest entries globally once
// maxEntries is reached).
type windowBgCollector struct {
	maxEntries  int
	maxPerShape int
	reason      string
	byShape     map[string][]windowBgItem
	total       int
}

func newWindowBgCollector(maxEntries, maxPerShape int, reason string) *windowBgCollector {
	return &windowBgCollector{
		maxEntries:  maxEntries,
		maxPerShape: maxPerShape,
		reason:      reason,
		byShape:     make(map[string][]windowBgItem),
	}
}

func (c *windowBgCollector) add(m *models.Mock) {
	at := m.Spec.ReqTimestampMock
	entry := models.MockEntry{
		Name:             m.Name,
		Kind:             string(m.Kind),
		Timestamp:        at.Unix(),
		ReqTimestampMock: at.Format(time.RFC3339Nano),
		Reason:           c.reason,
	}
	if !m.Spec.ResTimestampMock.IsZero() {
		entry.ResTimestampMock = m.Spec.ResTimestampMock.Format(time.RFC3339Nano)
	}
	shape := models.BackgroundShapeKey(m)
	item := windowBgItem{entry: entry, at: at, shape: shape}

	bucket := c.byShape[shape]
	if len(bucket) < c.maxPerShape {
		bucket = insertWindowBgItem(bucket, item)
		c.byShape[shape] = bucket
		c.total++
	} else if item.at.After(bucket[0].at) || (item.at.Equal(bucket[0].at) && item.entry.Name > bucket[0].entry.Name) {
		bucket = insertWindowBgItem(bucket[1:], item)
		c.byShape[shape] = bucket
	} else {
		return
	}

	for c.total > c.maxEntries {
		// Evict the oldest item across buckets with >1 sample first so distinct
		// background operations are preserved; fall back to global oldest.
		evictShape := ""
		var evictAt time.Time
		foundMulti := false
		for s, b := range c.byShape {
			if len(b) == 0 {
				continue
			}
			isMulti := len(b) > 1
			if foundMulti && !isMulti {
				continue
			}
			if (isMulti && !foundMulti) || evictShape == "" || b[0].at.Before(evictAt) || (b[0].at.Equal(evictAt) && s < evictShape) {
				evictShape = s
				evictAt = b[0].at
				foundMulti = isMulti
			}
		}
		if evictShape == "" {
			break
		}
		b := c.byShape[evictShape]
		if len(b) <= 1 {
			delete(c.byShape, evictShape)
		} else {
			c.byShape[evictShape] = b[1:]
		}
		c.total--
	}
}

func insertWindowBgItem(bucket []windowBgItem, item windowBgItem) []windowBgItem {
	idx := len(bucket)
	for i := range bucket {
		if item.at.Before(bucket[i].at) || (item.at.Equal(bucket[i].at) && item.entry.Name < bucket[i].entry.Name) {
			idx = i
			break
		}
	}
	return slices.Insert(bucket, idx, item)
}

func (c *windowBgCollector) entries() []models.MockEntry {
	if c.total == 0 {
		return nil
	}
	items := make([]windowBgItem, 0, c.total)
	for _, b := range c.byShape {
		items = append(items, b...)
	}
	slices.SortStableFunc(items, func(a, b windowBgItem) int {
		if cmp := a.at.Compare(b.at); cmp != 0 {
			return cmp
		}
		if a.entry.Name < b.entry.Name {
			return -1
		}
		if a.entry.Name > b.entry.Name {
			return 1
		}
		return 0
	})
	out := make([]models.MockEntry, len(items))
	for i, it := range items {
		out[i] = it.entry
	}
	return out
}
