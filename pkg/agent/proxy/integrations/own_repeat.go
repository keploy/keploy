package integrations

import (
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// ONE CALL MORE THAN THE OPEN TEST RECORDED, AND THE BACKGROUND (under the
// proven switch, a set served by the background rules: S12Background).
//
// In a test, the store shows the set's background (its pool and the worker
// scope the run reached) after the open test's own recordings, so a call the
// application makes while the test is open, recorded between tests or by a
// worker, is answered. A call identical to one the open test's own
// recordings already answered is no such call: it is the test's own call,
// made once more than it was recorded (models.MissExtraCall), and the
// background never answers it. Answered by the background's copy of it, a
// duplicate of a recorded call (a write made twice, a retried request) would
// pass.
//
// The parser decides whether a call repeats the open test's own recordings,
// as its matcher reads "the same call", and matches that call on the store
// HideBackground returns: the background is in none of its reads and every
// consume door refuses it, so the call finds the test's own recordings not
// yet answered, if any, and otherwise misses as one call more than the test
// recorded. The background's recording stays unspent for a call it is the
// recording of.

// BackgroundHider is an optional capability of a store HoldToRound returned
// for one call: the store as a call one more than the open test's own
// recordings of it reads it (HideBackground). The agent's MockManager's held
// store implements it, and every MockMemDb wrapper that forwards
// RoundHolder must forward it.
type BackgroundHider interface {
	// HideBackground is the store with the set's background recordings
	// (the pool's and the worker scope's data-plane recordings, under the
	// background rules, none of them the open test's own) out of every
	// read and refused at every consume and spend door while a test is
	// open. It is one call's, as the held store is: it may change the
	// store in place and return it.
	HideBackground() MockMemDb
}

// HideBackground is db, a store HoldToRound returned for one call that is
// one more than the open test's own recordings of it, with the set's
// background out of every read and refused at every consume door
// (BackgroundHider): neither the pool nor the worker scope answers the call.
// db itself when it hides nothing (no BackgroundHider): a store that serves
// no set by the background rules shows no background in a test.
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

// OwnRepeatRead is db, the store HoldToRound returned for call, as a call of
// kind reads it: with the set's background hidden (HideBackground) when the
// call repeats one of the open test's own recordings of kind that already
// answered, and db itself otherwise. It is the one check every parser makes
// before its matcher reads db, so no protocol lets the background answer a
// call one more than the open test recorded.
//
// repeats is the parser's own reading of "the same call": whether the call,
// as its matcher judges it (its noise, its bindings, its keys), matches one
// of answered, the open test's recordings of kind that answered
// (ExactConsumedReader), in the order they did. It never consumes and never
// changes the connection's state. It is asked only under the proven switch,
// on a set served by the background rules (S12BackgroundIn), from a store
// that can hide the background, and only when the test answered a recording
// of kind: a set served by today's rules shows a test no background, and a
// call before the test's first answer repeats nothing.
//
// Config and session recordings (a handshake, a login, a heartbeat) are no
// test's own, so they are never in answered and stay readable to the call
// either way.
func OwnRepeatRead(db MockMemDb, call CallRound, kind models.Kind, repeats func(answered []*models.Mock) bool) MockMemDb {
	if !provenswitch.On() || !S12BackgroundIn(call, db) {
		return db
	}
	if _, ok := db.(BackgroundHider); !ok {
		return db
	}
	er, ok := db.(ExactConsumedReader)
	if !ok {
		return db
	}
	var answered []*models.Mock
	for _, mk := range er.ConsumedTestMocks() {
		if mk != nil && mk.Kind == kind {
			answered = append(answered, mk)
		}
	}
	if len(answered) == 0 || !repeats(answered) {
		return db
	}
	return HideBackground(db)
}
