package proxy

import (
	"encoding/binary"
	"hash/maphash"
	"math"
	"reflect"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// answeredLog remembers, for the test set staged now, the per-test recordings
// that answered a call, and the background's that a call spent (one of the
// worker scope's in a test's window, one of the pool's), by their request
// keys (models.RequestKeys), so that a later miss can tell it is a repeat of
// a call made earlier (FindAnsweredEarlier, models.RepeatNote). A consumed
// per-test recording, or a spent one of the background's, is in no read
// after it answers, so without this the miss of an identical later call
// names nothing ("no_mocks, candidates 0").
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
	// redacting holds, apart from the keyed entries and their bounds, every
	// recording noted that holds a pattern (Mock.Noise: the obfuscator
	// redacted something in it), once a name and content (redactingKey), in
	// the order noted (AnsweredRedacting); redactingKeys are their keys and
	// redactingBytes their sizes (answeredSize). One that would pass
	// redactingMaxBytes is not kept, and its kind is marked in redactingLost.
	redacting      []*models.Mock
	redactingKeys  map[redactingKey]bool
	redactingBytes int
	redactingLost  map[models.Kind]bool
}

// redactingMaxBytes bounds what the log keeps of a test set's answered
// recordings that hold a pattern (answeredLog.redacting).
const redactingMaxBytes = 16 << 20

// answeredEntry is one recording that answered, and when.
type answeredEntry struct {
	mock *models.Mock // answeredProbe's; nil once the entry is dropped
	at   time.Time
	// background is the scope of the set's background mock is of
	// (models.RepeatNote.Background), empty for a test's own recording.
	background string
	keys       []string
	refs       int // the key lists it is in: 0 once dropped
	size       int // answeredSize
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
//
// A recording that holds a pattern is kept apart too, whatever its keys and
// size, under redactingMaxBytes alone (keepRedacting).
func (l *answeredLog) note(mk *models.Mock, at time.Time) {
	l.noteFrom(mk, at, "")
}

// noteFrom is note of a recording of the set's background of scope
// background (models.RepeatNote.Background); "" is a test's own.
func (l *answeredLog) noteFrom(mk *models.Mock, at time.Time, background string) {
	if mk == nil || mk.Name == "" {
		return
	}
	keys := models.RequestKeys(mk)
	redacts := len(mk.Noise) > 0
	if len(keys) == 0 && !redacts {
		return
	}
	probe := answeredProbe(mk)
	size := answeredSize(probe, keys)
	var key redactingKey
	keyed := false
	if redacts {
		key, keyed = redactingKeyOf(probe)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if g := mk.TestModeInfo.StagedSet; g != 0 && g != l.set {
		return
	}
	if redacts {
		l.keepRedacting(probe, key, keyed, size)
	}
	if len(keys) == 0 || size > answeredMaxEntryBytes {
		return
	}
	if l.byKey == nil {
		l.byKey = map[string][]*answeredEntry{}
	}
	e := &answeredEntry{mock: probe, at: at, background: background, keys: keys, size: size}
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

// keepRedacting keeps probe, of a recording that holds a pattern, once a
// key (redactingKey; one with no key, keyed false, each time it is noted):
// one that would pass redactingMaxBytes is not kept, and its kind is marked
// lost (redactingOf). Caller holds mu.
func (l *answeredLog) keepRedacting(probe *models.Mock, key redactingKey, keyed bool, size int) {
	if keyed && l.redactingKeys[key] {
		return
	}
	if l.redactingBytes+size > redactingMaxBytes {
		if l.redactingLost == nil {
			l.redactingLost = map[models.Kind]bool{}
		}
		l.redactingLost[probe.Kind] = true
		return
	}
	if keyed {
		if l.redactingKeys == nil {
			l.redactingKeys = map[redactingKey]bool{}
		}
		l.redactingKeys[key] = true
	}
	l.redacting = append(l.redacting, probe)
	l.redactingBytes += size
}

// redactingOf is what the log kept of the recordings of kind that answered
// and hold a pattern, and whether it kept every one noted.
func (l *answeredLog) redactingOf(kind models.Kind) ([]*models.Mock, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*models.Mock
	for _, m := range l.redacting {
		if m.Kind == kind {
			out = append(out, m)
		}
	}
	return out, !l.redactingLost[kind]
}

// forgetRedacting empties what the log keeps apart (keepRedacting). Caller
// holds mu.
func (l *answeredLog) forgetRedacting() {
	l.redacting, l.redactingKeys, l.redactingBytes, l.redactingLost = nil, nil, 0, nil
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
// What it keeps apart of those that redacted something (keepRedacting), and
// whether it kept them all, it keeps until the next fence: a reset also runs
// mid-set (the agent-replacement repair, MockManager.ResetForReplaySession),
// where a call that repeats a recording that answered before it still carries
// the secret that recording redacted. A reset that starts another test set
// is followed by its staging's fence (ArmSetFence) when the CLI says the set
// starts, which forgets them. A CLI that never says so arms no fence: the
// log then keeps them across the run's test sets, a recording of a later
// set beside an earlier set's of the same name (names are unique within a
// set only: keepRedacting keys by name and content), until
// redactingMaxBytes, past which their kind reads incomplete and its calls
// are redacted whole.
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
	l.forgetRedacting()
	l.set = set
}

// answeredProbe is what the log keeps of mk: a copy with its name, kind,
// noise, test-mode bookkeeping, metadata, times, learned request noise,
// learned clock reads and request, whatever its kind, and no response, nor the loader that would read
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
		Clocks:             s.Clocks,
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

// redactingKey tells apart the recordings the log keeps apart
// (keepRedacting): their name and the digest of their content
// (redactingKeyOf). Two copies of one recording (the agent stages each
// test's pool from copies) are one; two recordings of one name with another
// request or other patterns are two, as test sets name their recordings
// alike (a name is unique within a set only).
type redactingKey struct {
	name    string
	content [2]uint64
}

// redactingKeyOf is probe's key: its name, and the digest of its kind, its
// request (answeredProbe's spec) and its patterns, last, which is all a
// redaction reads of it: every string, number and flag they reach, each
// with its kind, a slice's and a map's length, a map's pairs in no order,
// its times aside (no place is read from a time). It is two 64-bit hashes
// (hash/maphash) under seeds drawn at start: no two recordings' contents
// meet by chance in 128 bits. ok is false for a value the walk does not
// read (deeper than reachableBytes reads, or a channel or a function): such
// a recording has no key, and is kept each time it is noted.
func redactingKeyOf(probe *models.Mock) (redactingKey, bool) {
	h := newContentHash()
	h.str(string(probe.Kind))
	if !h.walk(reflect.ValueOf(&probe.Spec).Elem(), 0) {
		return redactingKey{}, false
	}
	h.word(uint64(len(probe.Noise)))
	for _, pat := range probe.Noise {
		h.str(pat)
	}
	return redactingKey{name: probe.Name, content: h.sum()}, true
}

// contentSeeds are contentHash's seeds.
var contentSeeds = [2]maphash.Seed{maphash.MakeSeed(), maphash.MakeSeed()}

var timeType = reflect.TypeFor[time.Time]()

// contentHash is two hashes of one content under contentSeeds.
type contentHash [2]maphash.Hash

func newContentHash() *contentHash {
	var h contentHash
	h[0].SetSeed(contentSeeds[0])
	h[1].SetSeed(contentSeeds[1])
	return &h
}

func (h *contentHash) word(u uint64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], u)
	_, _ = h[0].Write(b[:])
	_, _ = h[1].Write(b[:])
}

// str writes s with its length first, so no two splits of the same bytes
// hash alike.
func (h *contentHash) str(s string) {
	h.word(uint64(len(s)))
	_, _ = h[0].WriteString(s)
	_, _ = h[1].WriteString(s)
}

func (h *contentHash) sum() [2]uint64 { return [2]uint64{h[0].Sum64(), h[1].Sum64()} }

// walk writes v, each value with its kind first (a value an interface
// holds, with its type), depth at most 24 as reachableBytes reads; false for a deeper value or one of a kind a request
// holds none of (a channel, a function, an unsafe pointer).
func (h *contentHash) walk(v reflect.Value, depth int) bool {
	if depth > 24 {
		return false
	}
	if !v.IsValid() {
		h.word(0)
		return true
	}
	h.word(uint64(v.Kind()) + 1)
	switch v.Kind() {
	case reflect.String:
		h.str(v.String())
	case reflect.Bool:
		if v.Bool() {
			h.word(1)
		} else {
			h.word(0)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		h.word(uint64(v.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		h.word(v.Uint())
	case reflect.Float32, reflect.Float64:
		h.word(math.Float64bits(v.Float()))
	case reflect.Complex64, reflect.Complex128:
		c := v.Complex()
		h.word(math.Float64bits(real(c)))
		h.word(math.Float64bits(imag(c)))
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			h.word(0)
			return true
		}
		h.word(1)
		if v.Kind() == reflect.Interface {
			// The held value's type, by its identity in this process (a
			// type is one *rtype, which Type values compare by): two types
			// of one name and layout are two.
			h.word(uint64(reflect.ValueOf(v.Elem().Type()).Pointer()))
		}
		return h.walk(v.Elem(), depth+1)
	case reflect.Struct:
		if v.Type() == timeType {
			return true
		}
		h.word(uint64(v.NumField()))
		for i := 0; i < v.NumField(); i++ {
			if !h.walk(v.Field(i), depth+1) {
				return false
			}
		}
	case reflect.Slice, reflect.Array:
		h.word(uint64(v.Len()))
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			b := v.Bytes()
			_, _ = h[0].Write(b)
			_, _ = h[1].Write(b)
			return true
		}
		for i := 0; i < v.Len(); i++ {
			if !h.walk(v.Index(i), depth+1) {
				return false
			}
		}
	case reflect.Map:
		h.word(uint64(v.Len()))
		// Each pair hashed alone, the sums added: the same pairs in any
		// order are one.
		var acc [2]uint64
		it := v.MapRange()
		for it.Next() {
			e := newContentHash()
			if !e.walk(it.Key(), depth+1) || !e.walk(it.Value(), depth+1) {
				return false
			}
			s := e.sum()
			acc[0] += s[0]
			acc[1] += s[1]
		}
		h.word(acc[0])
		h.word(acc[1])
	default:
		return false
	}
	return true
}

// noteAnswered records that the per-test recording mk answered a call now.
func (m *MockManager) noteAnswered(mk models.Mock) {
	m.answered.note(&mk, time.Now())
}

// AnsweredRedacting implements integrations.AnsweredRedactingReader: since
// the last fence (the test set staged now; the run, for a CLI that never
// says a set starts: answeredLog.reset), every per-test recording of kind
// that answered a call and holds a pattern, and every such background
// recording a call spent (noteBackgroundSpent), once a name and content, as
// the answered log keeps it (answeredProbe), and whether the log kept every
// one (redactingMaxBytes).
func (m *MockManager) AnsweredRedacting(kind models.Kind) ([]*models.Mock, bool) {
	return m.answered.redactingOf(kind)
}

var _ integrations.AnsweredRedactingReader = (*MockManager)(nil)

// noteBackgroundSpent records that mk, a recording of the set's background
// of scope background (models.RepeatBackgroundWorker or
// models.RepeatBackgroundPool), answered a call now and is spent for the
// calls it answers no more: a later identical call it does not answer then
// names it, and its scope (FindAnsweredEarlier).
func (m *MockManager) noteBackgroundSpent(mk *models.Mock, background string) {
	m.answered.noteFrom(mk, time.Now(), background)
}

// FindAnsweredEarlier implements integrations.AnsweredFinder: of the per-test
// recordings of kind that answered a call earlier in this test set, and of
// the background's a call spent (AnsweredCall.Background), under any of
// keys, the one answered last that match accepts, and is not the open test's
// own (recorded inside its window, or in its exact view). ok is false when
// there is none.
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
	return integrations.AnsweredCall{Mock: best.mock, AnsweredAt: best.at, Background: best.background}, true
}

var _ integrations.AnsweredFinder = (*MockManager)(nil)
