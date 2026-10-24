package proxy

import (
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// answeredLog remembers, for the test set staged now, the per-test recordings
// that answered a call, by their request keys (models.RequestKeys), so that a
// later miss can tell it is a repeat of a call an earlier test made
// (FindAnsweredEarlier, models.RepeatNote). A consumed per-test recording is
// in no read after it answers, so without this the miss of an identical
// later call names nothing ("no_mocks, candidates 0").
//
// It holds pointers to recordings the agent already decoded, never a copy of
// a value the application sent, and is bounded: at most answeredPerKey
// recordings per key (the newest), and at most answeredMaxKeys keys (the
// oldest-noted key goes first). A repeat whose earlier twin was pushed out
// is reported as before, with no note. It is for a miss's diagnosis only:
// nothing in it is ever served.
type answeredLog struct {
	mu    sync.Mutex
	byKey map[string][]answeredEntry
	order []string // keys in the order first noted, for eviction
}

// answeredEntry is one recording that answered, and when.
type answeredEntry struct {
	mock *models.Mock
	at   time.Time
}

const (
	answeredPerKey  = 16
	answeredMaxKeys = 8192
)

// note records that mk answered a call at at. A recording of a kind that
// declares no request keys is not kept.
func (l *answeredLog) note(mk *models.Mock, at time.Time) {
	if mk == nil || mk.Name == "" {
		return
	}
	keys := models.RequestKeys(mk)
	if len(keys) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byKey == nil {
		l.byKey = map[string][]answeredEntry{}
	}
	for _, k := range keys {
		list, seen := l.byKey[k]
		if !seen {
			l.order = append(l.order, k)
		}
		list = append(list, answeredEntry{mock: mk, at: at})
		if len(list) > answeredPerKey {
			list = append(list[:0:0], list[len(list)-answeredPerKey:]...)
		}
		l.byKey[k] = list
	}
	for len(l.order) > answeredMaxKeys {
		delete(l.byKey, l.order[0])
		l.order = l.order[1:]
	}
}

// candidates are the recordings noted under any of keys, newest first per
// key, each once.
func (l *answeredLog) candidates(keys []string) []answeredEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []answeredEntry
	seen := map[string]bool{}
	for _, k := range keys {
		list := l.byKey[k]
		for i := len(list) - 1; i >= 0; i-- {
			e := list[i]
			if seen[e.mock.Name] {
				continue
			}
			seen[e.mock.Name] = true
			out = append(out, e)
		}
	}
	return out
}

// reset forgets every recording noted: a new test set, or a new session.
func (l *answeredLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byKey, l.order = nil, nil
}

// noteAnswered records that the per-test recording mk answered a call now.
func (m *MockManager) noteAnswered(mk models.Mock) {
	c := mk
	m.answered.note(&c, time.Now())
}

// FindAnsweredEarlier implements integrations.AnsweredFinder: of the per-test
// recordings of kind that answered a call earlier in this test set, under any
// of keys, the one answered last that match accepts, and is not the open
// test's own (recorded inside its window, or in its exact view). ok is false
// when there is none. TestRecordedStart is when the open test began at record
// time, zero when no window is set.
func (m *MockManager) FindAnsweredEarlier(kind models.Kind, keys []string, match func(*models.Mock) bool) (integrations.AnsweredCall, bool) {
	if match == nil || len(keys) == 0 {
		return integrations.AnsweredCall{}, false
	}
	m.windowMu.RLock()
	start := m.windowStart
	m.windowMu.RUnlock()
	own := func(mk *models.Mock) bool {
		if v := m.exactView(); v != nil {
			if _, ok := v.index[mk.Name]; ok {
				return true
			}
		}
		return m.InTestWindow(mk.Spec.ReqTimestampMock)
	}
	var best answeredEntry
	for _, e := range m.answered.candidates(keys) {
		if e.mock.Kind != kind || (best.mock != nil && !e.at.After(best.at)) || own(e.mock) {
			continue
		}
		if match(e.mock) {
			best = e
		}
	}
	if best.mock == nil {
		return integrations.AnsweredCall{}, false
	}
	return integrations.AnsweredCall{Mock: best.mock, AnsweredAt: best.at, TestRecordedStart: start}, true
}

var _ integrations.AnsweredFinder = (*MockManager)(nil)
