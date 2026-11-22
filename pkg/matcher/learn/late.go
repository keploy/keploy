package learn

import (
	"strconv"
	"strings"
	"time"
)

// A late call is one the application makes in a later test's window while
// it still works on what an older test gave it (a consumer that retried a
// message F did not answer, and fell behind): the value it carries now is
// what F gave the application in that older test, and the value the
// call's recording holds stands in a recording of the window's own test. The
// search for the recorded value's source (appearedIn) finds that recording,
// but the value the call carries never came from it: a reason that named it
// as the value's source sent the user to the wrong recording.

// lateServes are the recordings F gave the application in one older test of
// the set (givenWords.test, Raw.test) that hold live, the value a call
// carries now, whole, as F gave it there (givenWords.gaveLive): those among
// raw, the recordings handed to the call (a protocol may hand it those F
// served since the test it ran before the call's began: Tracker.Before),
// then those F gave earlier in the set (offsetPlaces.words), in the order
// the word rule reads them. The call is late only where nothing else F
// gave the application before the call may hold live too, so none where
// something does (a likelier source, or one as likely, which the reason
// would hide):
//   - a recording F served at the start-up, between tests, in the call's
//     own test or in more than one test (test 0), as recorded or by its
//     view, among raw or the set's;
//   - one whose view keploy cannot tell (gaveLive: no adapter said, it could
//     not be read, or one of its serves' differing views holds live), even
//     of one older test;
//   - a leaf of an exchange F served leaf by leaf, of the test case's own
//     request or of a value the test gave (served: each Earlier.Seen's
//     live value, as F gave it, the test sent it or the application sent
//     it), holding live anywhere;
//   - a place of served keploy cannot read (Earlier.Unread): the test
//     case's own request, or the run's places past its bound, any of which
//     may hold live;
//   - a recording F gave earlier in the set that could not be read
//     (offsetPlaces.unread).
//
// One among raw that could not be read is refused before, with its own
// reason. A recording F had not served before the call (Raw.served,
// Raw.servedAfter) gave the application nothing.
func (dc *DriftCache) lateServes(live string, served []Earlier, raw []Raw, op *offsetPlaces) []givenWords {
	if op.unread != "" {
		return nil
	}
	for _, e := range served {
		if e.Unread != "" {
			return nil
		}
		for _, s := range e.Seen {
			if strings.Contains(s.Live, live) {
				return nil
			}
		}
	}
	var out []givenWords
	// alone reads g: a serve of one older test that gave live whole is
	// named; false where g may hold live otherwise.
	alone := func(g givenWords) bool {
		switch whole, may := g.gaveLive(live); {
		case whole && g.test > 0:
			out = append(out, g)
		case may:
			return false
		}
		return true
	}
	for _, x := range raw {
		if x.served && !x.servedAfter && !alone(dc.wordsOf(x)) {
			return nil
		}
	}
	for _, g := range op.words {
		if !alone(g) {
			return nil
		}
	}
	return out
}

// minLateRun is the shortest run of letters and digits that names a late
// call by itself (lateID): sixteen, 64 bits of hex and more of the other
// alphabets, which no answer holds by chance.
const minLateRun = 16

// lateID reports whether a late call may be named by live, the value it
// carries now: an id no answer holds by chance, which holds a letter and a
// digit. That is a UUID (8-4-4-4-12 hex digits), or ASCII letters, digits,
// '-' and '_' alone, one of whose runs between the '-' and '_' is
// minLateRun or longer and holds a letter and a digit itself (a ULID, a
// hex or unpadded base32 token: "evt-3N9WB5TJ6HCAQ8MV"; a nanoid or
// base64url token only where its '-' and '_' leave such a run). A count, a
// flag, a status, a code or a short name ("4", "true", "pending", "201",
// "us-west-2", "worker-11", "t3-medium", "en_US-v2") stands in many
// answers, so an older test's serve that holds one shows nothing of where
// the call's value came from, and keploy says it cannot name it. lateID
// reads the shape alone: a compact timestamp ("20261103T100512Z") or a
// long name with a digit ("ProductCatalogV3Service") passes too.
func lateID(live string) bool {
	for i := 0; i < len(live); i++ {
		if c := live[i]; !isWord(c) && c != '-' && c != '_' {
			return false
		}
	}
	if uuidForm(live) {
		return mixed(live)
	}
	for _, run := range strings.FieldsFunc(live, func(r rune) bool { return r == '-' || r == '_' }) {
		if len(run) >= minLateRun && mixed(run) {
			return true
		}
	}
	return false
}

// uuidForm reports whether s is a UUID's text: 36 bytes, '-' at 8, 13, 18
// and 23, hex digits elsewhere.
func uuidForm(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if dash := i == 8 || i == 13 || i == 18 || i == 23; dash != (s[i] == '-') || !dash && !isHex(s[i]) {
			return false
		}
	}
	return true
}

// mixed reports whether s, of ASCII letters, digits, '-' and '_', holds a
// letter and a digit.
func mixed(s string) bool {
	letter, digit := false, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digit = true
		case c != '-' && c != '_':
			letter = true
		}
	}
	return letter && digit
}

// gaveLive says what keploy can tell of whether F gave the application live
// whole, as one word in a reading of g's answer (answerLists.holdsWhole):
// whole where it did, in its recording's answer where F served it as
// recorded or in the view F gave of it where it stated one; may where it
// did or may have (whole), and where keploy cannot tell what F gave there
// (no adapter said, a view could not be read) or one of the views its
// serves gave, which differ, holds live. A word of a reading is a run of
// its bytes alone, so one equal to live is all of it.
func (g givenWords) gaveLive(live string) (whole, may bool) {
	if g.unstated || g.unread {
		return false, true
	}
	for _, view := range g.views {
		if view.holdsWhole(live) {
			return false, true
		}
	}
	if len(g.views) > 0 {
		return false, false
	}
	lists := g.answerLists
	if g.view.words != nil {
		lists = g.view
	}
	whole = lists.holdsWhole(live)
	return whole, whole
}

// holdsWhole reports whether l holds t as a word in a reading, its stamps
// blanked.
func (l answerLists) holdsWhole(t string) bool {
	for i := range readings {
		if l.holds(i, t, false) {
			return true
		}
	}
	return false
}

// notFromWhy is the reason of a drifted value v whose recorded value the
// search found at from (appearedIn), a recording's payload of the call's own
// test, where F served from as the rule a copy follows its source sets
// aside (follows.refute: as recorded, or by a view the value did not move
// with): nothing F gave the application there holds the value
// the call carries now. Where the value is an id (lateID) that serves of
// older tests gave the application whole, and nothing else F gave it before
// the call may hold (lateServes), it names those serves (lateOf), and late
// is true: the leaf is no variable of from then. Otherwise it says plainly
// that keploy cannot name where the value came from, never that it flowed
// from from. "" where F's view of from may hold the value, keploy cannot tell what
// F gave there, or F had not served it before the call (Place.unserved,
// whose reason says so: correction), and where every word of the value that
// changed is a read of the clock (clockSet.whole: the clock's rules read it,
// and an answer's instant is followed whole whatever F gave there), which
// leaves the search's reason (evidence). Only past those does it read the
// recordings F gave the application earlier in the set (places).
func (dc *DriftCache) notFromWhy(v *valueDrift, from Place, at time.Time, served []Earlier, raw []Raw, places func() *offsetPlaces) (why string, late bool) {
	if v.clock.whole {
		return "", false
	}
	aside := ""
	for _, x := range raw {
		if x.mock != from.Mock {
			continue
		}
		keep := dc.stampsEchoed(v, served, raw)
		if f := (&follows{dc: dc, rec: v.rec, live: v.live, at: at}); f.refute(x, dc.searched(x, keep), keep, v, from) {
			aside = f.refuted
		}
		break
	}
	if aside == "" {
		return "", false
	}
	// Only an id names a late call (lateID): any other value reads no
	// recording F gave earlier in the set here.
	if lateID(v.live) {
		if why := lateOf(v, from, dc.lateServes(v.live, served, raw, places())); why != "" {
			return why, true
		}
	}
	return aside + "; " + notClock(v) + ", and keploy cannot name where the value the call carries now came from, so it cannot follow it. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it", false
}

// lateOf is the reason of a call that carries what F gave the application
// in an older test (late: lateServes), where its recorded value stands in
// from, a recording of the call's own test (appearedIn), at which F did not
// give it that value (notFromWhy): the call is late, and nothing of its own
// test answers it; "" where no older test's serve gave it. It names the
// serves by their recordings, never by the order F ran its tests in, which
// a selection or a deferred test makes no test case's number; only a serve
// of the test F ran just before the call's says so (givenWords.prev), whose
// values keploy follows where the call was recorded with them.
func lateOf(v *valueDrift, from Place, late []givenWords) string {
	if len(late) == 0 {
		return ""
	}
	at := placeAt(from)
	found := "the value it was recorded with stands whole in " + at
	if from.Held != v.rec && from.Held != v.decRec {
		found = "a part of the value it was recorded with (" + partOf(v.rec, from.Held) + ") stands in " + at
	}
	const tail = ", a recording of the call's own test, where record's full round did not give the application that value. "
	// A value of the test F ran before the call's is one keploy follows
	// where the call was recorded with it: this call was not.
	notFollowed := "no recording of the call's own test answers it, since keploy does not follow a value another test's recording gave the application. "
	for _, g := range late {
		if g.prev {
			notFollowed = "no recording of the call's own test answers it, and the call was recorded with another value than the application got there, so keploy cannot follow it from there either. "
		}
	}
	notFollowed += "Look at why the application fell behind (a call of that test keploy did not answer, which the application retried)"
	if len(late) > 1 {
		names := make([]string, 0, 3)
		for _, g := range late[:min(3, len(late))] {
			names = append(names, g.mock)
		}
		more := ""
		if len(late) > 3 {
			more = " and " + strconv.Itoa(len(late)-3) + " more"
		}
		return "the value it carries now is, whole, what record's full round gave the application in each of " + strings.Join(names, ", ") + more + ", each in an older test than the call's, so keploy cannot tell which of them it carries; " + found + tail + "The call is late: the application still works on what an older test gave it, and " + notFollowed
	}
	g := late[0]
	test := "an older test than the call's"
	if g.prev {
		test = "the test record's full round ran just before the call's"
	}
	return "the value it carries now is, whole, what record's full round gave the application in " + g.mock + ", in " + test + "; " + found + tail + "The call is a late call of " + g.mock + "'s test: the application still works on what " + g.mock + " gave it there, and " + notFollowed
}
