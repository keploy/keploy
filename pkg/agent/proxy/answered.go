package proxy

import (
	"reflect"
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
// It keeps of each recording only what a match reads (answeredProbe): its
// name, kind, times, noise and request, never its response, so the on-disk
// store's promise that a set's answers are not resident holds. It is bounded
// by bytes: at most answeredPerKey recordings per key (the newest), at most
// answeredMaxBytes in all (answeredSize; the oldest noted goes first), and a
// recording whose request alone is over answeredMaxEntryBytes is not kept. A
// repeat whose earlier twin was not kept, or was pushed out, is reported as
// before, with no note. It is for a miss's diagnosis only: nothing in it is
// ever served.
type answeredLog struct {
	mu    sync.Mutex
	byKey map[string][]*answeredEntry
	// queue holds the entries in the order noted, oldest first, for the
	// byte bound's eviction; one a key's bound dropped from every list it
	// was in stays as an empty shell until compact.
	queue []*answeredEntry
	live  int // entries in some key's list
	bytes int // the live entries' sizes
	// set is the test set generation (MockManager.setGen) whose answers the
	// log holds, moved by fence.
	set uint64
}

// answeredEntry is one recording that answered, and when.
type answeredEntry struct {
	mock *models.Mock // answeredProbe's; nil once the entry is dropped
	at   time.Time
	keys []string
	refs int // the key lists it is in: 0 once dropped
	size int // answeredSize
}

const (
	answeredPerKey = 16
	// answeredMaxBytes bounds what the log holds for one test set.
	answeredMaxBytes = 16 << 20
	// answeredMaxEntryBytes is the largest recording the log keeps: one
	// bigger would push out many smaller ones.
	answeredMaxEntryBytes = answeredMaxBytes / 64
)

// note records that mk answered a call at at. A recording of a kind that
// declares no request keys is not kept, nor one over answeredMaxEntryBytes,
// nor one an earlier test set's staging pooled (TestModeInfo.StagedSet):
// a consume of it is no answer of this set (errPreviousSet), however late
// after the fence the parser that held it gets here.
func (l *answeredLog) note(mk *models.Mock, at time.Time) {
	if mk == nil || mk.Name == "" {
		return
	}
	keys := models.RequestKeys(mk)
	if len(keys) == 0 {
		return
	}
	probe := answeredProbe(mk)
	size := answeredSize(probe, keys)
	if size > answeredMaxEntryBytes {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if g := mk.TestModeInfo.StagedSet; g != 0 && g != l.set {
		return
	}
	if l.byKey == nil {
		l.byKey = map[string][]*answeredEntry{}
	}
	e := &answeredEntry{mock: probe, at: at, keys: keys, size: size}
	l.queue = append(l.queue, e)
	l.live++
	l.bytes += size
	for _, k := range keys {
		list := append(l.byKey[k], e)
		e.refs++
		if len(list) > answeredPerKey {
			l.unref(list[0])
			list = append(list[:0:0], list[1:]...)
		}
		l.byKey[k] = list
	}
	for l.bytes > answeredMaxBytes && l.live > 0 {
		l.evictOldest()
	}
	l.compact()
}

// unref takes e out of one key's list; out of its last, it is dropped.
// Caller holds mu.
func (l *answeredLog) unref(e *answeredEntry) {
	e.refs--
	if e.refs == 0 {
		l.drop(e)
	}
}

// drop forgets e's recording and its bytes. Caller holds mu.
func (l *answeredLog) drop(e *answeredEntry) {
	e.mock, e.refs = nil, 0
	l.live--
	l.bytes -= e.size
}

// evictOldest takes the oldest entry still kept out of every key's list.
// Caller holds mu.
func (l *answeredLog) evictOldest() {
	for len(l.queue) > 0 {
		e := l.queue[0]
		l.queue[0] = nil
		l.queue = l.queue[1:]
		if e.mock == nil {
			continue
		}
		for _, k := range e.keys {
			list := l.byKey[k][:0:0]
			for _, o := range l.byKey[k] {
				if o != e {
					list = append(list, o)
				}
			}
			if len(list) == 0 {
				delete(l.byKey, k)
			} else {
				l.byKey[k] = list
			}
		}
		l.drop(e)
		return
	}
}

// compact sheds the queue's dropped entries once they outnumber the kept
// ones, so a key answered again and again holds no growing queue. Caller
// holds mu.
func (l *answeredLog) compact() {
	if len(l.queue) <= 2*l.live+64 {
		return
	}
	kept := make([]*answeredEntry, 0, l.live)
	for _, e := range l.queue {
		if e.mock != nil {
			kept = append(kept, e)
		}
	}
	l.queue = kept
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
			out = append(out, *e)
		}
	}
	return out
}

// reset forgets every recording noted: a new test set, or a new session.
func (l *answeredLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byKey, l.queue, l.live, l.bytes = nil, nil, 0, 0
}

// fence empties the log for the test set of generation set, which from now
// on refuses a note of a mock another set pooled (note). Under the one lock
// a note takes: a note of the previous set's mock lands before and is
// emptied, or after and is refused.
func (l *answeredLog) fence(set uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.byKey, l.queue, l.live, l.bytes = nil, nil, 0, 0
	l.set = set
}

// answeredProbe is what the log keeps of mk: a copy with its name, kind,
// noise, test-mode bookkeeping, metadata, times, learned request noise and
// request, whatever its kind, and no response, nor the loader that would read
// one back from disk. A match (FindAnsweredEarlier) reads the request side
// only, and the note names the recording and its times.
func answeredProbe(mk *models.Mock) *models.Mock {
	c := mk.ShallowCopy()
	c.SetResponseHydrator(nil)
	s := mk.Spec
	c.Spec = models.MockSpec{
		Metadata:           s.Metadata,
		GenericRequests:    s.GenericRequests,
		HTTPReq:            s.HTTPReq,
		Created:            s.Created,
		MongoRequests:      s.MongoRequests,
		PostgresRequestsV2: s.PostgresRequestsV2,
		GRPCReq:            s.GRPCReq,
		MySQLRequests:      s.MySQLRequests,
		DNSReq:             s.DNSReq,
		HTTP2Req:           s.HTTP2Req,
		ReqTimestampMock:   s.ReqTimestampMock,
		ResTimestampMock:   s.ResTimestampMock,
		ReqBodyNoise:       s.ReqBodyNoise,
	}
	return c
}

// answeredEntryOverhead is what an entry costs beside its recording's
// strings and slices: the entry, the probe's structs, and a key list's slot.
const answeredEntryOverhead = 512

// answeredSize is about how many bytes the log holds for probe under keys:
// every string, slice and map it reaches, counted as if it held them alone,
// and the keys. It errs high, never low.
func answeredSize(probe *models.Mock, keys []string) int {
	n := answeredEntryOverhead + reachableBytes(reflect.ValueOf(probe), 0)
	for _, k := range keys {
		n += len(k) + 16
	}
	return n
}

// reachableBytes is the bytes of strings, slices and maps v reaches, depth
// at most 24 (a request holds no deeper value; a cycle stops there).
func reachableBytes(v reflect.Value, depth int) int {
	if depth > 24 || !v.IsValid() {
		return 0
	}
	switch v.Kind() {
	case reflect.String:
		return v.Len()
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return 0
		}
		return int(v.Elem().Type().Size()) + reachableBytes(v.Elem(), depth+1)
	case reflect.Struct:
		n := 0
		for i := 0; i < v.NumField(); i++ {
			n += reachableBytes(v.Field(i), depth+1)
		}
		return n
	case reflect.Slice:
		if v.IsNil() {
			return 0
		}
		n := v.Len() * int(v.Type().Elem().Size())
		for i := 0; i < v.Len(); i++ {
			n += reachableBytes(v.Index(i), depth+1)
		}
		return n
	case reflect.Array:
		n := 0
		for i := 0; i < v.Len(); i++ {
			n += reachableBytes(v.Index(i), depth+1)
		}
		return n
	case reflect.Map:
		if v.IsNil() {
			return 0
		}
		n := 0
		it := v.MapRange()
		for it.Next() {
			n += int(it.Key().Type().Size()) + int(it.Value().Type().Size()) + 16
			n += reachableBytes(it.Key(), depth+1) + reachableBytes(it.Value(), depth+1)
		}
		return n
	}
	return 0
}

// noteAnswered records that the per-test recording mk answered a call now.
func (m *MockManager) noteAnswered(mk models.Mock) {
	m.answered.note(&mk, time.Now())
}

// FindAnsweredEarlier implements integrations.AnsweredFinder: of the per-test
// recordings of kind that answered a call earlier in this test set, under any
// of keys, the one answered last that match accepts, and is not the open
// test's own (recorded inside its window, or in its exact view). ok is false
// when there is none.
func (m *MockManager) FindAnsweredEarlier(kind models.Kind, keys []string, match func(*models.Mock) bool) (integrations.AnsweredCall, bool) {
	if match == nil || len(keys) == 0 {
		return integrations.AnsweredCall{}, false
	}
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
	return integrations.AnsweredCall{Mock: best.mock, AnsweredAt: best.at}, true
}

var _ integrations.AnsweredFinder = (*MockManager)(nil)
