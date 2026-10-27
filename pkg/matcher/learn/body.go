package learn

import (
	"container/list"
	"errors"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// rawBody is what a Raw reads of its recording's payload (RawOf): its text,
// where each field stands in it, its answer's own clock stamps and clock
// reads, and whether it was read at all.
type rawBody struct {
	text        string
	answerClock []time.Time
	fields      []rawField
	stamps      []rawField
	read        bool
	// unread, of a body whose recording could not be loaded, is why
	// (rawUnread names it).
	unread string
}

// size is about what b holds in memory, in bytes: its text, each field's
// place and path, and its clock reads.
func (b rawBody) size() int64 {
	n := int64(len(b.text)) + int64(len(b.unread)) + 64
	for _, list := range [][]rawField{b.fields, b.stamps} {
		for _, f := range list {
			n += 48 + int64(len(f.path))
		}
	}
	return n + 24*int64(len(b.answerClock))
}

// bodyOf is m's payload as RawOf reads it, and whether m is a connection's
// recorded start-up (isStartUp) where it was read.
func bodyOf(m *models.Mock) (b rawBody, startUp bool) {
	r := rawOfNow(m, false)
	return r.payload(), r.startUp
}

// rawSource is where a lazy Raw (DriftCache.LazyRawOf) reads its payload:
// its recording's identity (MockID) in the set's store of payloads, and the
// loader that gives the recording with its answer when the store holds none
// of it (first read, or evicted since).
type rawSource struct {
	id    string
	name  string
	load  func() (*models.Mock, error)
	store *bodyStore
}

// body is the payload of the recording s names: the store's, or loaded now.
func (s *rawSource) body() rawBody {
	return s.store.get(s.id, s.name, s.load)
}

// payload is what x read of its recording's payload: for a lazy Raw, loaded
// on first use from its source and kept in the set's bounded store.
func (x Raw) payload() rawBody {
	if x.src == nil {
		return rawBody{text: x.rText, answerClock: x.rClock, fields: x.rFields, stamps: x.rStamps, read: x.rRead}
	}
	return x.src.body()
}

// withoutStamps is x with its payload read now and no answer stamps: what
// a search that reads the stamps as any text reads (searched, keep).
func (x Raw) withoutStamps() Raw {
	b := x.payload()
	x.src = nil
	x.rText, x.rClock, x.rFields, x.rStamps, x.rRead = b.text, b.answerClock, b.fields, nil, b.read
	if x.unread == "" {
		x.unread = b.unread
	}
	return x
}

// isRead is payload().read without loading a payload the store has read
// before (bodyStore.readOf): a rule that only asks whether a place was read
// loads nothing it has loaded once in the set.
func (x Raw) isRead() bool {
	if x.src == nil {
		return x.rRead
	}
	if read, ok := x.src.store.readOf(x.src.id); ok {
		return read
	}
	return x.payload().read
}

// unreadWhy is why x was not read, where its source says so: a lazy Raw's
// recording that could not be loaded.
func (x Raw) unreadWhy() string {
	if x.unread != "" || x.src == nil {
		return x.unread
	}
	return x.payload().unread
}

// everyRawLazy, set by the package's tests alone (KEPLOY_LEARN_LAZY_EVERY_RAW),
// makes every RawOf a lazy Raw whose payload is read again at each use, so
// that every row of the package reads its payloads through the accessor:
// a rule reading a payload field past it reads an empty one there, and its
// rows fail. nil in keploy itself.
var everyRawLazy func(m *models.Mock, served bool) Raw

// bodyBudget is the bytes of payloads the set's store keeps loaded (about,
// by rawBody.size). Payloads past it are let go, least recently read
// first, and loaded again where a rule next reads them: memory stays within
// it whatever the set's size, at the cost of reading one again.
var bodyBudget int64 = 64 << 20

// bodyStore keeps the payloads of the set's lazy Raws (LazyRawOf) loaded,
// by recording (MockID), within bodyBudget bytes, the least recently read
// let go first. A second read of a payload being loaded waits for that
// load; a payload larger than the whole budget is handed to its reader and
// kept by none. Whether each was read at all (its read bit) is kept past
// its payload, a bool by recording, so that a rule asking only that loads
// nothing again. The tracker makes one per set (setDerived), so what it
// holds goes with the set.
type bodyStore struct {
	mu     sync.Mutex
	budget int64
	used   int64
	order  *list.List // of *bodyEntry, the most recently read first
	by     map[string]*bodyEntry
	read   map[string]bool
	// loads counts the payloads loaded, for the rows.
	loads int
}

type bodyEntry struct {
	id    string
	ready chan struct{}
	body  rawBody
	size  int64
	at    *list.Element
}

func newBodyStore(budget int64) *bodyStore {
	return &bodyStore{budget: budget, order: list.New(), by: map[string]*bodyEntry{}, read: map[string]bool{}}
}

// readOf is whether the recording id names was read, where the store has
// loaded it once; ok is false where it has not.
func (s *bodyStore) readOf(id string) (read, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	read, ok = s.read[id]
	return read, ok
}

// get is the payload of the recording id (named name) names: kept, being
// loaded (waited for), or loaded now through load.
func (s *bodyStore) get(id, name string, load func() (*models.Mock, error)) rawBody {
	s.mu.Lock()
	if e, ok := s.by[id]; ok {
		if e.at != nil {
			s.order.MoveToFront(e.at)
		}
		s.mu.Unlock()
		<-e.ready
		return e.body
	}
	e := &bodyEntry{id: id, ready: make(chan struct{})}
	s.by[id] = e
	s.loads++
	s.mu.Unlock()

	e.body = loadBody(name, load)
	e.size = e.body.size()

	s.mu.Lock()
	s.read[id] = e.body.read
	if e.size > s.budget {
		delete(s.by, id)
	} else {
		e.at = s.order.PushFront(e)
		s.used += e.size
		for s.used > s.budget {
			last := s.order.Back()
			if last == nil || last == e.at {
				break
			}
			old := last.Value.(*bodyEntry)
			s.order.Remove(last)
			delete(s.by, old.id)
			s.used -= old.size
		}
	}
	s.mu.Unlock()
	close(e.ready)
	return e.body
}

// loadBody reads the payload load gives of the recording named name; one
// it cannot give is an unread body that says why (rawUnread's reason).
func loadBody(name string, load func() (*models.Mock, error)) rawBody {
	m, err := load()
	if err == nil && m == nil {
		err = errors.New("no recording was given")
	}
	if err != nil {
		return rawBody{unread: "keploy could not read back " + name + ", an earlier place of the call, with its recorded answer (" + err.Error() +
			"), so it cannot tell whether what changed came from there: a fault of keploy's, which keploy reports rather than learn anything"}
	}
	b, _ := bodyOf(m)
	return b
}

// Loads is how many payloads the set's store has loaded so far (each first
// read, and each read again after it was let go): the rows read it to show
// a lazy Raw is loaded only where a rule reads it.
func (dc *DriftCache) Loads() int {
	if dc == nil || dc.set == nil || dc.set.bodies == nil {
		return 0
	}
	s := dc.set.bodies
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// LazyRawOf is m as RawOf reads it, served as served, but with its payload
// read only where a rule first reads it, and then kept in the tracker's
// store of the set's payloads, within a fixed budget (bodyStore): what a
// call holds of the recordings before it no longer grows with them. load
// gives m with its answer (one the agent parked on disk read back); nil is m
// itself. Its identity is MockID(m), whatever m's answer, since what load
// gives is the whole recording: what is read of its payload is kept by it
// for the set (setDerived). A cache no tracker made keeps no store, and its
// Raw is read now, as RawOf (or, its answer parked, as load gives it).
//
// A recording load cannot give is an unread place, naming why: a rule that
// reads it refuses (rawUnread), never learns from it.
func (dc *DriftCache) LazyRawOf(m *models.Mock, served bool, load func() (*models.Mock, error)) Raw {
	if load == nil {
		load = func() (*models.Mock, error) { return m, nil }
	}
	r := Raw{mock: m.Name, at: m.Spec.ReqTimestampMock, answered: m.Spec.ResTimestampMock, served: served, call: callOf(m),
		startUp: isStartUp(m), id: MockID(m)}
	if dc == nil || dc.set == nil {
		b := loadBody(m.Name, load)
		r.rText, r.rClock, r.rFields, r.rStamps, r.rRead, r.unread = b.text, b.answerClock, b.fields, b.stamps, b.read, b.unread
		if !b.read {
			r.startUp = false
		}
		return r
	}
	dc.set.mu.Lock()
	if dc.set.bodies == nil {
		dc.set.bodies = newBodyStore(bodyBudget)
	}
	store := dc.set.bodies
	dc.set.mu.Unlock()
	r.src = &rawSource{id: r.id, name: m.Name, load: load, store: store}
	return r
}
