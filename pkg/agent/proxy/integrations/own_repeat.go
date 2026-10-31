package integrations

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ONE CALL MORE THAN THE OPEN TEST RECORDED, AND THE BACKGROUND (under the
// proven switch: a set served by the background rules, S12Background, or any
// view that shows a test the worker scope the run reached,
// SequenceView.Worker).
//
// In a test, the store shows the set's background after the open test's own
// recordings: under the background rules its pool and the worker scope the
// run reached, under any other rules that worker scope. So a call the
// application makes while the test is open, recorded between tests or by a
// worker, is answered. A call identical to one the open test's own
// recordings already answered is no such call: it is the test's own call,
// made once more than it was recorded (models.MissExtraCall), and the
// background never answers it. Answered by the background's copy of it, a
// duplicate of a recorded call (a write made twice, a retried request) would
// pass.
//
// The parser decides whether a call is one of the open test's own calls: one
// its matcher, as it reads "the same call", matches with a recording the
// test owns, answered or not (OwnRepeatRead). Such a call is matched on the
// store HideBackground returns: the background is in none of its reads and
// every consume door refuses it, so the call finds the test's own
// recordings not yet answered, if any, and otherwise misses as one call more
// than the test recorded. The background's recording stays unspent for a
// call it is the recording of. The decision reads what the test owns, never
// what has answered so far, so calls made at once are held to it too.

// BackgroundHider is an optional capability of a store HoldToRound returned
// for one call: the store as a call one more than the open test's own
// recordings of it reads it (HideBackground). The agent's MockManager's held
// store implements it, and every MockMemDb wrapper that forwards
// RoundHolder must forward it.
type BackgroundHider interface {
	// HideBackground is the store with the set's background recordings
	// (the pool's and the worker scope's under the background rules, the
	// reached worker scope's under any rules; none of them the open
	// test's own) out of every read and refused at every consume and
	// spend door while a test is open. It is one call's, as the held store is: it may change the
	// store in place and return it.
	HideBackground() MockMemDb
}

// HideBackground is db, a store HoldToRound returned for one call that is
// one more than the open test's own recordings of it, with the set's
// background out of every read and refused at every consume door
// (BackgroundHider): neither the pool nor the worker scope answers the call.
// db itself when it hides nothing (no BackgroundHider): such a store shows a
// test no background.
func HideBackground(db MockMemDb) MockMemDb {
	if h, ok := db.(BackgroundHider); ok {
		return h.HideBackground()
	}
	return db
}

// BackgroundMember is an optional capability of a store (and of a
// RoundSnapshot): whether a recording, by name, is one of the background's
// (the pool's or the worker scope's) of a set served by the background
// rules that the view in force holds as no test's own. The learn step reads it to keep the background from answering a
// call one more than the open test recorded (learn.Candidate.Background).
// The agent's MockManager implements it, and every MockMemDb wrapper that
// forwards RoundHolder must forward it.
type BackgroundMember interface {
	InBackground(name string) bool
}

// InBackground is db's BackgroundMember answer for name: false from a store
// that has none (it serves no set by the background rules).
func InBackground(db MockMemDb, name string) bool {
	b, ok := db.(BackgroundMember)
	return ok && b.InBackground(name)
}

// OwnRepeatRead is db, the store HoldToRound returned for call, as the call
// reads it: with the set's background hidden (HideBackground) when the call
// is one of the open test's own calls, and db itself otherwise. It is the one
// check every parser makes before its matcher reads db, so no protocol lets
// the background answer a call one more than the open test recorded.
//
// owns is the parser's own reading of "the same call": whether the call, as
// its matcher judges it (its noise, its bindings, its keys), matches one of
// the open test's own recordings, answered or not (SharedReads'
// OwnedRecordings, through the parser's OwnIndex). It never consumes and
// never changes the connection's state. It is asked only under the proven
// switch, when the call's view showed a test a background
// (ShowsBackgroundIn), and from a store that can hide it: a view that shows
// a test none has no background to hide.
//
// The check reads the recordings the test owns, never which of them have
// answered so far, so it holds for calls made at once. Two identical calls
// of a call the test recorded once both find the test's recording, and both
// read the store with the background hidden. One is answered with the
// recording; the other's consume is refused, and it misses as one call more
// than the test recorded. A check made on what had answered so far would
// let both calls read the background, and the call that lost the test's
// recording would be answered with the background's copy. A call that
// matches an own recording not yet answered is answered with it, as before:
// the store shows the test's own recordings before its background.
//
// Config and session recordings (a handshake, a login, a heartbeat) are no
// test's own, so they are never in the owned list and stay readable to the
// call either way.
func OwnRepeatRead(db MockMemDb, call CallRound, owns func() bool) MockMemDb {
	if !provenswitch.On() || !ShowsBackgroundIn(call, db) {
		return db
	}
	if _, ok := db.(BackgroundHider); !ok {
		return db
	}
	if !owns() {
		return db
	}
	return HideBackground(db)
}

// OwnedLister is the part of SharedReads an OwnIndex reads: the open test's
// own recordings, answered or not, and the number of the view that shows
// them.
type OwnedLister interface {
	OwnedRecordings() (view uint64, owned []*models.Mock, ok bool)
}

// OwnIndex is a parser's index of the open test's own recordings of one kind
// (OwnedLister), answered or not, filed under a key its matcher's "the same
// call" implies: a call and a recording it matches have the same key. A
// view's own recordings never change while it lasts, and each test's open
// shows a new one, so the index is built once per view, at the test's first
// call that asks it, and each call then reads its key's recordings (Of):
// a call costs what the test's recordings of its key cost, never every
// recording the test owns.
//
// It keeps the last view's index only, and with it that view's recordings,
// until a call of another view asks.
type OwnIndex[K comparable] struct {
	kind models.Kind
	key  func(*models.Mock) (K, bool)

	mu sync.Mutex
	// view, first and n name the list the index was built of: the view's
	// number, its list's first element and its length. Numbers restart in
	// each store, and a store's list is its own, so the three name one
	// list; holding first keeps its address from being reused.
	view  uint64
	first **models.Mock
	n     int
	by    map[K][]*models.Mock
}

// NewOwnIndex is an OwnIndex of kind's recordings, each filed under key's
// answer for it; a recording key reports false for (one no call can match)
// is filed under none.
func NewOwnIndex[K comparable](kind models.Kind, key func(*models.Mock) (K, bool)) *OwnIndex[K] {
	return &OwnIndex[K]{kind: kind, key: key}
}

// Of is the open test's own recordings of x's kind filed under k, in the
// view's order, as db reads them now: none while no test is open, or from a
// store that lists none (no OwnedLister). The caller only reads them.
func (x *OwnIndex[K]) Of(db MockMemDb, k K) []*models.Mock {
	l, ok := db.(OwnedLister)
	if !ok {
		return nil
	}
	view, owned, ok := l.OwnedRecordings()
	if !ok || len(owned) == 0 {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.by == nil || x.view != view || x.first != &owned[0] || x.n != len(owned) {
		by := make(map[K][]*models.Mock)
		for _, mk := range owned {
			if mk == nil || mk.Kind != x.kind {
				continue
			}
			if rk, ok := x.key(mk); ok {
				by[rk] = append(by[rk], mk)
			}
		}
		x.view, x.first, x.n, x.by = view, &owned[0], len(owned), by
	}
	return x.by[k]
}

// ShowsBackgroundIn reports whether the view call's first read found showed a
// test a background that is no test's own: the set's under the background
// rules (S12BackgroundIn), or, under any rules, the worker scope the run
// reached (CallRound.ShowsWorker: SequenceView.Worker, set only in a test's
// view that shows one, or a snapshot's read of such a view). Either answers
// a call the open test makes after its own recordings, and neither may
// answer one call more than the test recorded (OwnRepeatRead).
func ShowsBackgroundIn(call CallRound, db MockMemDb) bool {
	return S12BackgroundIn(call, db) || call.ShowsWorker()
}
