package integrations

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// RecordingWarmer begins the warm of one staged set for the parser of one
// kind of mock: under exact serving (the proven switch) the agent calls it
// once as it stages a set, and hands the warm it returns each recording of
// the set of that kind, in recorded order, so the set's first calls find
// what the parser reads of a recording the first time a call compares it or
// serves it already read, into the parser's own caches. What a warm keeps
// must be the same pure function of the recording's content the call's own
// read is (content-keyed, no call-time state), and it may only fill a
// cache, never decide or narrow a match. It keeps within the caches' own
// bounds, and returns false once it can keep no more of the set: the agent
// then hands it no more (the calls read the rest themselves, as with no
// warming). A nil warm reads nothing of the set.
type RecordingWarmer func() (warm func(m *models.Mock) (more bool))

var (
	warmersMu sync.RWMutex
	warmers   = map[models.Kind]RecordingWarmer{}
)

// RegisterRecordingWarmer registers the warmer of kind's recordings, as a
// parser that keeps caches of its recordings' reads does from its init,
// beside Register. A kind with no warmer has nothing read ahead. A nil w
// unregisters kind's.
func RegisterRecordingWarmer(kind models.Kind, w RecordingWarmer) {
	warmersMu.Lock()
	defer warmersMu.Unlock()
	if w == nil {
		delete(warmers, kind)
		return
	}
	warmers[kind] = w
}

// RecordingWarmers is a copy of the warmers registered, by kind.
func RecordingWarmers() map[models.Kind]RecordingWarmer {
	warmersMu.RLock()
	defer warmersMu.RUnlock()
	out := make(map[models.Kind]RecordingWarmer, len(warmers))
	for k, w := range warmers {
		out[k] = w
	}
	return out
}
