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
// test owns, answered or not (OwnRepeatRead). The decision is asked where a
// background recording is about to answer the call, at the store's consume
// door, never before: a call the test's own recordings answer never asks it.
// For a call that is one of the test's own, the store refuses that consume
// and from then on hides the background: it is in none of the store's reads
// and every consume door refuses it, so the call, matched again, finds the
// test's own recordings not yet answered, if any, and otherwise misses as
// one call more than the test recorded. The background's recording stays
// unspent for a call it is the recording of. The decision reads what the
// test owns, never what has answered so far, so calls made at once are held
// to it too.

// BackgroundHider is an optional capability of a store HoldToRound returned
// for one call: the store with the decision whether the call is one of the
// open test's own calls kept for the door where the background is about to
// answer it (HideBackgroundWhen). The agent's MockManager's held store
// implements it.
type BackgroundHider interface {
	// HideBackgroundWhen is the store with owns, the decision whether its
	// call is one of the open test's own calls, asked at most once: at the
	// first consume or spend door a recording of the set's background
	// reaches while a test is open (the pool's or the worker scope's under
	// the background rules, the reached worker scope's under any rules;
	// none of them the open test's own), or by HidesBackground, whichever
	// asks first. When owns says true, the store refuses that consume, and
	// from then on the background is out of every read and refused at every
	// consume and spend door. owns never consumes and never reaches a door
	// of the store. It is one call's, as the held store is: it may change
	// the store in place and return it.
	HideBackgroundWhen(owns func() bool) MockMemDb
	// HidesBackground reports whether the store hides the background from
	// its call: owns's answer, asked now if no door asked it; false when
	// the store was given no owns.
	HidesBackground() bool
	// RefusesAnswer reports whether the store's consume door would refuse
	// mk for its call (owns asked first when mk is one of the background's
	// recordings and nothing asked it), changing nothing else.
	RefusesAnswer(mk *models.Mock) bool
}

// BackgroundMember is an optional capability of a store (and of a
// RoundSnapshot): whether a recording, by name, is one of the background's
// that the view in force holds as no test's own: the pool's or the worker
// scope's of a set served by the background rules, or, in a test under any
// rules, one of the worker scope the run reached. The learn step reads it to
// keep the background from answering a call one more than the open test
// recorded (learn.Candidate.Background).
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
// reads it: with the decision whether the call is one of the open test's own
// calls kept for the door where a background recording is about to answer it
// (BackgroundHider), and db itself where there is no background to hide. It
// is the one check every parser makes before its matcher reads db, so no
// protocol lets the background answer a call one more than the open test
// recorded.
//
// owns is the parser's own reading of "the same call": whether the call, as
// its matcher judges it (its noise, its bindings, its keys), matches one of
// the open test's own recordings, answered or not (SharedReads'
// OwnedRecordings, through the parser's OwnIndex). It never consumes and
// never changes the connection's state. It is kept only under the proven
// switch, when the call's view showed a test a background
// (ShowsBackgroundIn), and by a store that can hide it: a view that shows a
// test none has no background to hide.
//
// owns is asked only when the background is about to answer the call, so a
// call the test's own recordings answer costs nothing more. When it says
// true, the store refuses that consume and hides the background, and the
// parser's match again (the one it makes for any consume the store refused)
// finds the test's own recordings not yet answered, if any, and otherwise
// misses. A miss asks the decision itself (OwnRepeat): one call more than
// the test recorded reads the store with the background hidden, as it is
// diagnosed and reported, whether or not the background held a copy of it.
//
// The decision reads the recordings the test owns, never which of them have
// answered so far, so it holds for calls made at once. Two identical calls
// of a call the test recorded once both find the test's recording first:
// the store shows the test's own recordings before its background. One is
// answered with it; the other's consume is refused, its match again finds
// the background's copy, whose door asks owns, which finds the test's
// recording, so the store refuses that copy too and the call misses as one
// call more than the test recorded. A decision made on what had answered so
// far would let the call that lost the test's recording be answered with the
// background's copy.
//
// Config and session recordings (a handshake, a login, a heartbeat) are no
// test's own, so they are never in the owned list and stay readable to the
// call either way.
func OwnRepeatRead(db MockMemDb, call CallRound, owns func() bool) MockMemDb {
	if !provenswitch.On() || !ShowsBackgroundIn(call, db) {
		return db
	}
	h, ok := db.(BackgroundHider)
	if !ok {
		return db
	}
	return h.HideBackgroundWhen(owns)
}

// OwnRepeat reports whether db, a store OwnRepeatRead returned, hides the
// background from its call, the call being one of the open test's own calls:
// the decision asked now if no consume door asked it (HidesBackground). A
// parser asks it when the call missed, before it diagnoses the miss: one
// call more than the test recorded is then read, compared and reported with
// the background hidden. False for a store OwnRepeatRead returned as it was.
func OwnRepeat(db MockMemDb) bool {
	h, ok := db.(BackgroundHider)
	return ok && h.HidesBackground()
}

// RefusesAnswer reports whether db, a store OwnRepeatRead returned, refuses
// to answer its call with mk, as its consume door would: mk is one of the
// background's recordings, and the call is one of the open test's own (the
// decision asked now if nothing asked it). A parser that acts on the
// recording it picked before its consume door (binds the recording's
// variables) asks first, and treats a refusal as the door's, so the
// background's recording is left as it was for the call it is the recording
// of. False from a store that hides nothing.
func RefusesAnswer(db MockMemDb, mk *models.Mock) bool {
	h, ok := db.(BackgroundHider)
	return ok && h.RefusesAnswer(mk)
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
