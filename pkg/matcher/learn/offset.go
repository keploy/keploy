package learn

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// AN INSTANT BUILT ON A SERVED ONE BY AN OFFSET.
//
// The echo (instants.go) takes a value for a copy of what the application was
// given where one rule names the moment F saw by what F served and the moment
// it was recorded with by what the recordings hold, each cut to the value's
// own precision: built on what it was given by no offset. An application
// also builds a value on what it was given by an offset (a message's
// visibility five minutes after the time it was sent, a due date a day after
// a served date). Where record's full round gave the application, at one
// place of what it was given, another instant than the recording did
// (another recording of the call F delivered there, a part of the test
// case's request keploy test sent with a template value), by about as much as
// the value moved, the value may be built on that place by an offset. It then
// moves as a read of the clock does wherever the place moved by about the
// lead, and at the minute, the hour or the day the lead takes a move of a
// unit or two for a read whatever moved it. The offset is read from the
// moments, never from a text's look:
//
//   - a moved place is an instant of one layout in one field of an answer
//     (models.MockSpec's answer fields; every field of a kind whose spec
//     holds its request and its answer in one), named by the word written
//     before it (instantLabel: its key, "created_at" of a JSON object), as
//     two recordings of one call (callOf) hold it, at two moments, at any
//     position in the field (a list F gave the application with another
//     number of elements puts the instant a value is built on at another
//     index): one the recording
//     gave the application before the call (recorded before it: hr), and
//     another that record's full round gave it before the call (served
//     earlier in the test, the payload of an exchange served leaf by leaf
//     too, Earlier.Payload, or before the test in the running set: at its
//     start-up, in an earlier test, between tests; another recorded test's
//     too, which this rule alone reads, Raw.OfAnotherTest: hf), where one
//     of the two is not what the other run
//     gave it too: hr one F did not give it, or hf one the recording did not
//     give it before the call, or both what both runs gave it, which F gave
//     in another order than the recording did (an application that takes
//     the first or the last of what it received takes the other one then)
//     or in an order keploy cannot tell (Raw.ServedIn: it fails closed).
//     F's order is one sequence of serves across the set (Order.Of), so a
//     recording F served before the test and one it served in it are in
//     order or not as two served in it are; one F served again after the
//     other (a pool's reusable recording) is in neither order. Two
//     recordings both runs gave the application in the order the recording
//     did, each served wholly before the other's first serve, wherever in
//     the set F gave them, are no move (F gave it nothing in their place), nor
//     are answers to two calls; or an instant of a part of the test case's request that keploy
//     test sent with another value than recorded (Earlier.Given). An answer's own clock stamp
//     and a protocol-phase answer's own clock reads are none (rules 2 and 3:
//     no value came from one), nor is what the application itself sent, or
//     an answer F served with the application's own value (a variable):
//     those moved with the application's own value;
//   - a changed instant of the value (its moment as recorded, rec, and as F
//     saw it, live, each at its own resolution) is built on a moved place
//     (hr where the recording gave it, hf where F did) by an offset where one
//     offset o puts hr+o in rec's unit and hf+o in live's, as a value cut to
//     its unit, truncated or rounded, after an offset is;
//   - a dated value is read against the places' dated instants; a time of
//     day against their times of day, in UTC or as each writes it in its own
//     zone, around the day.
//
// Such a value is refused, before any lead or window excuses it, as a pick
// the echo finds is, and the reason names the two places and the offset
// (offsetWhy): keploy follows no variable with an offset yet. A read of the
// clock that a moved place matches by chance is refused too: a false red,
// which fails closed and names the place, never noise.

// offsetPlace is an instant at a place the offset rule reads: its moment, the
// mock (or the test case) it stands in, and where in it ("<mock> at <field>");
// call, the call its recording answers (callOf; "" of the test case's
// request); recorded, whether the recording gave the application it before
// the call (its mock recorded before the call: a place's moment as the
// recording gave it, hr), and given, whether record's full round gave it
// before the call (served earlier in the test or before it in the set: a
// place's moment as F gave it, hf); at, when its mock's request was
// recorded, and first and last, the first and the last of F's serves of it
// in the set (Raw.ServedIn; 0 where it did not or that is not known): the
// two orders the two runs gave the application their recordings in. A place
// whose mock was recorded at the call's own instant (atCall: a fixture's
// whole second; or one whose recorded time keploy cannot read, read as it:
// noTime) is recorded, as every rule reads the equal instant, the
// fail-closed reading: the recording may have given it before the call. It
// is never one the recording surely gave, which only lets a pair go
// (inRecordedOrder).
type offsetPlace struct {
	m                       moment
	mock                    string
	where                   string
	call                    string
	recorded, given, atCall bool
	// noTime: its mock's recorded time keploy cannot read (Raw.noTime),
	// read as the call's own instant: the reason says so (offsetWhy).
	noTime      bool
	at          time.Time
	first, last int
}

// inRecordedOrder reports whether F gave the application the recordings of
// the places a and b, both of which both runs gave it, in the order the
// recording did: every serve of the one the recording gave first came
// before every serve of the other, so F gave it nothing in the other's
// place, and a value that moved from one to the other moved by something
// else. One recording's two places are in its order. Two recordings F gave
// it in another order, with their serves interleaved (a pool's recording F
// served at the start-up and again after the other), or in an order keploy
// cannot tell (either's not known, or both recorded at one instant), are
// not: the application may have taken the other one.
func inRecordedOrder(a, b offsetPlace) bool {
	if a.mock == b.mock && a.at.Equal(b.at) {
		return true
	}
	if a.first == 0 || b.first == 0 || a.at.Equal(b.at) {
		return false
	}
	if b.at.Before(a.at) {
		a, b = b, a
	}
	return a.last < b.first
}

// offsetKey is a place of an answer, as two recordings of a kind each hold
// it: the field (rawField.path), which of its forms (0: as it stands, 1: its
// other declared forms, rawExtra), the instant's layout, and the word written
// before it (instantLabel). Every instant of that layout and label in the
// field stands at it, at any position.
type offsetKey struct {
	path         string
	form, layout int
	label        string
}

// instantLabel is the word written before the i-th of spans, the instants of
// t: the last word before it that is no part of another instant ("at" of
// "created_at": "2026-…", "jobs" of each instant of "jobs": ["2026-…",
// "2026-…"]), "" where none is. It names where in a field an instant stands
// whatever its position: no value is read.
func instantLabel(t string, spans []instant, i int) string {
	j := spans[i].start - 1
	for j >= 0 {
		in := false
		for _, sp := range spans {
			if sp.start <= j && j < sp.end {
				j, in = sp.start-1, true
				break
			}
		}
		if in {
			continue
		}
		if !isWord(t[j]) {
			j--
			continue
		}
		end := j + 1
		for j >= 0 && isWord(t[j]) {
			j--
		}
		return t[j+1 : end]
	}
	return ""
}

// offsetPlaces are the places a call's offset rule reads (offsetPlacesOf):
// the answers' instants by place and moment, the set's (snap, read once for
// the set: offsetIndex) as the call reads them (view, by the index's
// recording: offsetView), and what F served at the leaves of a late
// exchange's answer (moves, the call's own), each place's sorted by moment;
// and the test case's request's moved instants (given), each as recorded and
// as keploy test sent it. at is when the call was recorded.
type offsetPlaces struct {
	at    time.Time
	snap  offsetSnap
	view  []offsetView
	moves map[offsetKey][]offsetPlace
	given [][2]offsetPlace
	// unread names a place F gave the application that could not be read
	// (GivenEarlier, or the payload of an exchange served leaf by leaf:
	// payloadUnread): every changed instant the rule reads is refused with
	// it (offsetFrom), and every word the word rule reads (givenWhy).
	unread string
	// words are the answers' words of each recording F gave the application
	// before the call that no earlier place of the call's own test is
	// (Raw.OfAnotherTest, and set's), in raw's order, then set's: the word
	// rule reads them (givenWhy).
	words []givenWords
	// answers are the instants the answers of those recordings hold, as F
	// gave them the application (answerMoments): the echo reads them
	// (echoedElsewhere).
	answers []givenMoment
	// dev are the index's recordings whose places can pair in the offset
	// rule (deviating), read once for the call; examined counts the places
	// its searches read, for the rows.
	dev      []int32
	devRead  bool
	examined int
}

// offsetPlacesOf reads the places the offset rule reads for a call recorded
// at at (offsetPlaces): each payload F served before the call or recorded
// before at (raw), each Earlier.Payload of an exchange served leaf by leaf
// (served), and each recording F gave the application earlier in the set
// that neither reads (set, read once for the set: GivenPlaces), with stamps
// and protocol-phase answer clock reads blanked (searched), each place said
// recorded where its recording came before at and given where F served it
// before the call (in the test, before it, or anywhere earlier in the set);
// and the parts of the test case's own request keploy test sent with
// another value than recorded.
func (dc *DriftCache) offsetPlacesOf(at time.Time, served []Earlier, raw []Raw, set []GivenPlaces) *offsetPlaces {
	op := &offsetPlaces{at: at}
	// One recording read once, whether F gave it the application before
	// the call as any of its readings says: a payload a leaf-by-leaf
	// exchange gives, the same recording among raw, and among set are one
	// (offsetSource.key). Its serves, where two readings of it number them,
	// span both: an order read wider than it is fails closed
	// (inRecordedOrder).
	var srcs []offsetSource
	// What F served at the leaves of an exchange's answer with another value
	// than recorded (a template value: Raw.moves), each where the payload
	// holds it, of each exchange: places F gave the application, never the
	// recording, which its payload's places at those leaves are.
	type moved struct {
		mock, call  string
		at          time.Time
		first, last int
		moves       []servedMove
	}
	var moves []moved
	for i := range raw {
		x := &raw[i]
		if !x.isRead() || !x.earlierThan(at) {
			continue
		}
		if len(x.moves) > 0 {
			// What F served at a late exchange's leaves (servedAnswer): read
			// where its payload holds them, as its own places, never merged
			// into the payload's.
			moves = append(moves, moved{mock: x.mock, call: x.call, at: x.at, first: x.first, last: x.last, moves: x.moves})
			continue
		}
		srcs = append(srcs, offsetSource{key: offsetKeyOf(x.mock, x.at), x: x, given: x.served, first: x.first, last: x.last})
	}
	for i := range served {
		e := &served[i]
		if e.Given {
			if e.Unread != "" {
				continue
			}
			for _, s := range e.Seen {
				if s.Live == s.Leaf.Value || s.Leaf.Type == Instant {
					continue
				}
				where := e.Mock + " at " + s.Leaf.Path
				for _, f := range [][2]string{{s.Leaf.Value, s.Live}, {dc.rawExtra(s.Leaf.Value), dc.rawExtra(s.Live)}} {
					for _, p := range driftedInstants(f[0], f[1]) {
						op.given = append(op.given, [2]offsetPlace{{m: p.rec, mock: e.Mock, where: where, recorded: true}, {m: p.live, mock: e.Mock, where: where, given: true}})
					}
				}
			}
			continue
		}
		if e.Payload.isRead() {
			x := &e.Payload
			srcs = append(srcs, offsetSource{key: offsetKeyOf(x.mock, x.at), x: x, given: true, first: x.first, last: x.last})
			if mv := e.servedMoves(); len(mv) > 0 {
				moves = append(moves, moved{mock: e.Mock, call: x.call, at: e.At, first: x.first, last: x.last, moves: mv})
			}
		} else if op.unread == "" {
			// What RawOf could not read, and an exchange that came with no
			// payload at all, is no place the rule can search: a value
			// built on its answer would pass as the lead's move.
			op.unread = payloadUnread(*e)
		}
	}
	for _, x := range raw {
		if x.isRead() && x.elsewhere {
			op.words = append(op.words, dc.wordsOf(x))
			op.answers = append(op.answers, dc.answerMoments(x)...)
		}
	}
	payloads := len(srcs)
	for i := range set {
		if g := &set[i]; g.read {
			srcs = append(srcs, offsetSource{key: offsetKeyOf(g.mock, g.at), g: g, given: true, first: g.first, last: g.last})
		}
	}
	// The set's index, shared by its handlers; past its budget, one of the
	// call's own, taking the places the set's holds as they are.
	ix := dc.offsetIndex()
	snap, recs, ok := ix.take(dc, srcs)
	if !ok {
		for i, r := range recs {
			if r >= 0 && !srcs[i].read {
				srcs[i].places, srcs[i].kept, srcs[i].read = snap.recs[r].places, true, true
			}
		}
		snap, recs, _ = newOffsetIndex(0).take(dc, srcs)
	}
	op.snap = snap
	op.view = make([]offsetView, len(snap.recs))
	merge := func(v *offsetView, given bool, first, last int) {
		if !given {
			first, last = 0, 0
		}
		v.given = v.given || given
		switch {
		case v.first == 0:
			v.first, v.last = int32(first), int32(last)
		case first > 0:
			v.first, v.last = min(v.first, int32(first)), max(v.last, int32(last))
		}
	}
	for i, s := range srcs {
		v := &op.view[recs[i]]
		switch {
		case i < payloads:
			if !v.in {
				v.in, v.noTime = true, s.x.noTime
			}
			merge(v, s.given, s.first, s.last)
		case v.in && !v.other:
			// Read among raw already: as an earlier place of the call's own
			// test, which every rule reads (unaccounted), or as another
			// test's, whose words are read above.
			merge(v, true, s.first, s.last)
		case !v.in:
			v.in, v.other, v.given = true, true, true
			v.first, v.last = int32(s.first), int32(s.last)
			w := s.g.words
			w.set = true
			op.words = append(op.words, w)
			op.answers = append(op.answers, s.g.answer...)
		}
	}
	if len(moves) > 0 {
		op.moves = map[offsetKey][]offsetPlace{}
		for _, mv := range moves {
			for _, kp := range dc.movePlaces(mv.mock, mv.moves) {
				op.moves[kp.k] = append(op.moves[kp.k], offsetPlace{m: kp.m, mock: mv.mock, where: kp.where, call: mv.call, given: true, at: mv.at, first: mv.first, last: mv.last})
			}
		}
		for _, list := range op.moves {
			sort.SliceStable(list, func(i, j int) bool { return list[i].m.key.at < list[j].m.key.at })
		}
	}
	return op
}

// placeOf is the place r of the index as the call reads it (offsetPlace),
// ok false where the call does not read its recording. A place recorded at
// the call's own instant is read as the recording gave it before the call,
// as every rule reads the equal instant: the fail-closed reading, which an
// offset on it refuses.
func (op *offsetPlaces) placeOf(r offsetRef) (offsetPlace, bool) {
	if int(r.rec) >= len(op.view) || !op.view[r.rec].in {
		return offsetPlace{}, false
	}
	v := op.view[r.rec]
	rec := &op.snap.recs[r.rec]
	kp := &rec.places[r.i]
	return offsetPlace{m: kp.m, mock: rec.mock, where: kp.where, call: rec.call, recorded: !rec.at.After(op.at), given: v.given, atCall: rec.at.Equal(op.at), noTime: v.noTime, at: rec.at, first: int(v.first), last: int(v.last)}, true
}

// keyedPlace is an instant of a recording where the offset rule reads it:
// where it stands (offsetKey), its moment, and the field it is in ("<mock>
// at <field>").
type keyedPlace struct {
	k     offsetKey
	m     moment
	where string
}

// keyedPlaces are the instants of x where the offset rule reads them: in
// each field of its answer (each field, of a kind whose spec holds its
// request and answer in one; none, of a recording that holds no answer
// apart from its request: answerApart), in its text and in its other
// declared forms,
// with its stamps and its answer's own clock reads blanked (searched). They
// are read once for the set where dc keeps x's (setPlaces).
func (dc *DriftCache) keyedPlaces(x Raw) []keyedPlace {
	if out, ok := dc.setPlaces(x); ok {
		return out
	}
	return dc.readPlaces(x)
}

// readPlaces reads keyedPlaces(x) from x's text.
func (dc *DriftCache) readPlaces(x Raw) []keyedPlace {
	var out []keyedPlace
	text := dc.searched(x, false)[0]
	xb := x.payload()
	answers := answerApart(xb.fields)
	for _, f := range xb.fields {
		if f.key || f.end > len(text) || answers && !answerPath(f.path) {
			continue
		}
		own := text[f.start:f.end]
		where := x.mock + " at " + f.describe()
		for form, t := range []string{own, dc.rawExtra(own)} {
			spans := dc.spansOf(t)
			for si, sp := range spans {
				if !sp.keyed {
					continue
				}
				out = append(out, keyedPlace{k: offsetKey{path: f.path, form: form, layout: sp.layout, label: instantLabel(t, spans, si)}, m: sp.moment(), where: where})
			}
		}
	}
	return out
}

// servedMove is a leaf of an exchange's answer F served with another value
// than recorded (a template value: Earlier.servedMoves): path, its field of
// the mock's spec ("Res.body.Messages[0].SentAt"); in, the field of the
// exchange's payload it stands in (rawField.path: "Res.body"), its own path
// where none holds it; name, the key written before it there ("SentAt"; ""
// where none is); live, what F served; quoted, whether it is written as a
// string.
type servedMove struct {
	path, in, name, live string
	quoted               bool
}

// movePlaces are the instants of what F served at moves, an exchange's
// leaves, where the offset rule reads them: keyed as the payload's own
// instants there are (keyedPlaces: the field it stands in, the form, the
// layout and the word written before it, here the leaf's key), so that the
// place F gave the application stands where another recording's payload
// holds the recording's, and named by the leaf ("<mock> at <leaf>").
func (dc *DriftCache) movePlaces(mock string, moves []servedMove) []keyedPlace {
	var out []keyedPlace
	for _, mv := range moves {
		t := mv.live
		switch {
		case mv.name != "" && mv.quoted:
			t = `"` + mv.name + `":"` + mv.live + `"`
		case mv.name != "":
			t = `"` + mv.name + `":` + mv.live
		}
		where := mock + " at " + rawField{path: mv.path}.describe()
		for form, ft := range []string{t, dc.rawExtra(t)} {
			spans := dc.spansOf(ft)
			for si, sp := range spans {
				if !sp.keyed {
					continue
				}
				out = append(out, keyedPlace{k: offsetKey{path: mv.in, form: form, layout: sp.layout, label: instantLabel(ft, spans, si)}, m: sp.moment(), where: where})
			}
		}
	}
	return out
}

// GivenPlaces is a recording F gave the application earlier in the running
// set, as the offset rule, the word rule and the echo read it (GivenEarlier):
// its instants where the offset rule reads them (keyedPlaces), the words of
// its answer (givenWords), the instants its answer holds as F gave them
// (answerMoments), and none of its text, so that each of the set's
// recordings is read once for the set (Order.GivenPlaces) and its text is
// not held; and where F served it in the set (ServedIn).
type GivenPlaces struct {
	mock, call  string
	at          time.Time
	read        bool
	first, last int
	places      []keyedPlace
	words       givenWords
	answer      []givenMoment
	// answers: its recording holds an answer the word rule reads
	// (holdsAnswer), where F may have given the application a word.
	answers bool
}

// PlacesOf is x as the offset rule and the word rule read it among the
// recordings F gave the application earlier in the set (GivenPlaces), served
// where x is (Raw.ServedIn), read with a cache of its own that it drops. It
// keeps no part of x's text: every word it keeps is a copy.
func PlacesOf(x Raw) GivenPlaces {
	dc := NewDriftCache()
	places := dc.keyedPlaces(x)
	for i := range places {
		k := &places[i]
		k.k.path, k.k.label, k.where = strings.Clone(k.k.path), strings.Clone(k.k.label), strings.Clone(k.where)
	}
	answer := dc.answerMoments(x)
	for i := range answer {
		a := &answer[i]
		a.mock, a.where = strings.Clone(a.mock), strings.Clone(a.where)
	}
	return GivenPlaces{mock: strings.Clone(x.mock), call: x.call, at: x.at, read: x.isRead(), first: x.first, last: x.last, places: places, words: dc.wordsOf(x), answer: answer, answers: holdsAnswer(x.payload().fields)}
}

// givenMoment is an instant the answer of a recording F gave the application
// before the call holds, at a place no earlier place of the call's own test
// is (another recorded test's, or one F gave earlier in the running set:
// Raw.OfAnotherTest, GivenEarlier), as the echo reads it (echoedElsewhere):
// its moment, its recording (mock, at: when its request was recorded), and
// where it stands ("<mock> at <field>", "<mock>'s payload").
type givenMoment struct {
	m           moment
	mock, where string
	at          time.Time
	// late is Raw.late: the reason says the recording is read both ways;
	// noTime, Raw.noTime: that keploy cannot read when it was recorded.
	late, noTime bool
}

// readAnswerMoments are the instants of x's answer as F gave the application
// them (givenMoment): of each field of its answer (of a kind whose spec holds
// its request and its answer in one field, or a recording with no fields
// read, its whole text: answerApart), in its text and its other declared
// forms, as it stands, its stamps and its answer's own clock reads too, as
// the echo reads every place (rules 2 and 3): F served that answer, and a
// value F's run copied from it moved as F's clock did. What F gives the
// application is an answer, never a request the application sent in another
// test.
func (dc *DriftCache) readAnswerMoments(x Raw) []givenMoment {
	var out []givenMoment
	add := func(t, where string) {
		for _, form := range []string{t, dc.rawExtra(t)} {
			for _, sp := range dc.spansOf(form) {
				if sp.keyed {
					out = append(out, givenMoment{m: sp.moment(), mock: x.mock, where: where, at: x.at, late: x.late, noTime: x.noTime})
				}
			}
		}
	}
	xb := x.payload()
	if !answerApart(xb.fields) {
		add(xb.text, x.mock+"'s payload")
		return out
	}
	for _, f := range xb.fields {
		if f.key || f.end > len(xb.text) || !answerPath(f.path) {
			continue
		}
		add(xb.text[f.start:f.end], x.mock+" at "+f.describe())
	}
	return out
}

// echoedElsewhere says why the echo refuses a drifted value whose changed
// instants (pairs: each as recorded and as F saw it) the call's own places
// do not account for (unaccounted, classifyInstant), "" when it does not:
// one of them is a copy of what F gave the application before the call at a
// place no earlier place of the call's own test is, or a pick from it by the
// application's clock (echoedPairs), which moves as a read of the clock does.
// The echo reads, as what F gave the application, every place the call's own
// echo reads (earlierMoments) and the answers of those recordings
// (places().answers: another recorded test's, Raw.OfAnotherTest, and every
// one F gave earlier in the running set, GivenEarlier, wherever and whenever
// it was recorded: one F served in this test before the call, at the
// start-up or in an earlier test, recorded after the call, too); and, as
// what the recordings gave it, the call's own places' (recordedMoments) and
// only those of these recordings that were recorded before the call, which
// the recording gave the application too: a recording made after the call
// holds nothing the recorded value was built on. One recorded at the call's
// own instant (a fixture's whole second) may have been given to the recorded
// application before the call or not, so it is read both ways: as one the
// recording gave it, and as one only F gave it. A recording made at or after
// the call was never given to the recorded application before the call, so
// only the moment F served there counts: an instant of it that names the
// moment F saw by a rule that does not name the recorded one is a copy,
// whatever else it names by another rule (lateEchoes; one that names both
// moments is no copy only where both runs were given it, holdsApart).
// No variable is bound there
// (keploy follows no value of another test's, nor of a recording made at or
// after the call): the value is refused, naming the place that holds the
// moment F saw (or, where that is a place of the call's own test, the one of
// those recordings that holds the moment it was recorded with by the same
// rule), never learned as noise. It reads the set (places) only where a changed
// instant asks, and only after the offset rule, which refuses every changed
// instant where one of those recordings could not be read (places().unread).
func (dc *DriftCache) echoedElsewhere(pairs []driftedInstant, at time.Time, served []Earlier, raw []Raw, places func() *offsetPlaces) string {
	if len(pairs) == 0 {
		return ""
	}
	op := places()
	if len(op.answers) == 0 {
		return ""
	}
	held, recorded := dc.earlierMoments(at, served, raw), dc.recordedMoments(at, served, raw)
	var late heldMoments
	for _, g := range op.answers {
		held.addMoment(g.m)
		if !g.at.After(at) {
			recorded.addMoment(g.m)
		}
		if !g.at.Before(at) {
			late.addMoment(g.m)
		}
	}
	echoed := echoedPairs(pairs, held, recorded)
	echoed = append(echoed, lateEchoes(pairs, echoed, &late)...)
	if len(echoed) == 0 {
		return ""
	}
	for _, g := range op.answers {
		for _, e := range echoed {
			if namesMomentApart(g.m, e.live, e.rec, e.rules) {
				return "the moment record's full round saw in it stands in " + g.where + ", a recording record's full round gave the application before the call that " + givenWhose(g.at, at, g.late, g.noTime) + ", so it cannot tell whether the value is a copy of what the application was given there, or a pick from it by its clock, which moves as a read of the clock does, or a read of the clock. " + correction(g.mock, false)
			}
		}
	}
	// The moment F saw stands at a place of the call's own test, which the
	// call's own echo takes for no copy (an instant there names both
	// moments), while one of those recordings, made before the call, names
	// the moment the value was recorded with by the rule that names F's
	// (oneCut): a copy all the same.
	where := dc.heldAt(echoed, at, served, raw)
	if where == "" {
		where = "earlier in the test set"
	}
	for _, g := range op.answers {
		if g.at.After(at) {
			continue
		}
		for _, e := range echoed {
			if namesMomentApart(g.m, e.rec, e.live, e.rules) {
				return "the moment record's full round saw in it stands " + where + ", and the moment it was recorded with stands in " + g.where + ", a recording record's full round gave the application before the call that " + givenWhose(g.at, at, g.late, g.noTime) + ", so it cannot tell whether the value is a copy of what the application was given, cut by one rule in both runs, or a read of the clock. " + correction(g.mock, false)
			}
		}
	}
	// Unreached but where an instant names both moments by one rule: refused
	// all the same.
	return pickUnaccounted(where)
}

// lateEchoes are the pairs of pairs that echoed (the echo's other reads)
// does not hold whose moment F saw an instant of late names by a rule that
// does not name the recorded moment (namesApart): late holds what F served
// at the places it gave the application before the call that were recorded
// at or after it, which the recorded application was never given before the
// call. The recorded value was built on none of them, so an instant there
// that names the moment F saw by such a rule is a copy of what F served,
// even where another rule names the recorded moment too (a minute rounded
// from "11:14:40" is F's "11:15", cut down it is the recorded "11:14"): only
// F's run had it to cut. One that names both by one rule (a served date the
// value is built on, the day both moments fall in) is the source of no
// change: what changed is the application's own.
func lateEchoes(pairs []driftedInstant, echoed []echoMatch, late *heldMoments) []echoMatch {
	var out []echoMatch
	for _, p := range pairs {
		if echoedPair(echoed, p) || !late.namesApart(p.live, p.rec) {
			continue
		}
		out = append(out, echoMatch{rec: p.rec, live: p.live})
	}
	return out
}

// echoedPair reports whether echoed holds p.
func echoedPair(echoed []echoMatch, p driftedInstant) bool {
	for _, e := range echoed {
		if e.rec == p.rec && e.live == p.live {
			return true
		}
	}
	return false
}

// givenWords are the words of a recording's answer where the word rule
// reads them (answerWords), of a recording F gave the application before
// the call that no earlier place of the call's own test is (another recorded
// test's, or one F gave earlier in the running set: Raw.OfAnotherTest,
// GivenEarlier): each reading's distinct words, sorted, and apart those its
// answer's stamps alone hold (stamped), and none of its text. at is when its
// request was recorded.
type givenWords struct {
	mock           string
	at             time.Time
	words, stamped [][]string
	// late is Raw.late: the reason says the recording is read both ways;
	// noTime, Raw.noTime: that keploy cannot read when it was recorded.
	late, noTime bool
	// same are the words of words, in each reading, that record's full
	// round gave the application at the fields the recording gave them at
	// (sameWords): what the rule a copy follows its source reads as a word
	// both runs gave there (follows.excusedGiven).
	same [][]string
	// gave are the words of its answer, in each reading, as record's full
	// round gave the application them where F served it changed and gave
	// its view (ServedLive), stamps read as any text; none otherwise (served
	// as recorded, F gave words and stamped). Where the value that lacks a
	// word both runs gave holds a word of the payload, or of any other place
	// the word rule reads, as either run gave it (inAnswer), the word shows
	// nothing (follows.tookElsewhere).
	gave [][]string
	// answered, conn and pos are the recording's (Raw), first and last F's
	// serves of it: where the rule lets a word of it go, they place it in
	// the order the recording and F gave the application the value's
	// candidate sources (follows.followed). set: a recording F gave the
	// application earlier in the set (GivenEarlier), which no Raw handed to
	// the call stands for.
	answered    time.Time
	conn        string
	pos         uint64
	first, last int
	set         bool
	// unstated: F served it and no adapter said what it gave the
	// application there (Raw.viewUnstated); unread: F served it changed with
	// a view keploy could not read (Raw.ServedChanged with none); views:
	// the words of its answer, in each reading, as each view F's serves of it
	// gave where they differ (Raw.ServedChanged), stamps read as any text.
	// A word a value holds now that may stand in what F gave there is
	// refused (servedWhy), as of a place of the call's own test (unseenWhy).
	unstated, unread bool
	views            [][][]string
}

// wordsOf is x's givenWords, every word a copy.
func (dc *DriftCache) wordsOf(x Raw) givenWords {
	g := givenWords{mock: strings.Clone(x.mock), at: x.at, late: x.late, noTime: x.noTime, answered: x.answered, conn: strings.Clone(x.conn), pos: x.pos, first: x.first, last: x.last}
	plain := dc.answerWords(x, false)
	kept := plain
	if len(dc.stampTexts(x)) > 0 {
		kept = dc.answerWords(x, true)
	}
	for i := range plain {
		g.words = append(g.words, sortedWords(plain[i], nil))
		g.stamped = append(g.stamped, sortedWords(kept[i], plain[i]))
	}
	// Served unchanged, every word is one both gave: the same lists.
	if x.AsRecorded() {
		g.same = g.words
		return g
	}
	// A recording of a kind that holds its answer apart from its request
	// that holds no answer gave the application nothing (answerApart),
	// whatever view F's serve of it had.
	if (x.viewUnstated() || x.unseen && len(x.views) == 0) && holdsAnswer(x.payload().fields) {
		g.unstated, g.unread = x.viewUnstated(), x.unseen && len(x.views) == 0
	}
	for _, view := range x.views {
		var words [][]string
		for _, w := range dc.answerWords(*view, true) {
			words = append(words, sortedWords(w, nil))
		}
		g.views = append(g.views, words)
	}
	for _, same := range dc.sameWords(x, plain) {
		g.same = append(g.same, sortedWords(same, nil))
	}
	if x.Live() {
		for _, gave := range dc.answerWords(*x.live, true) {
			g.gave = append(g.gave, sortedWords(gave, nil))
		}
	}
	return g
}

// sameWords are the words of words (x's answer's, by reading) that record's
// full round gave the application at every field of x's payload the
// recording gave them at, and at no other: every one where F served x
// unchanged (Raw.AsRecorded); of a payload F served changed and gave its
// view of (ServedLive), those the view holds at exactly the fields (by
// path) whose recorded text holds them, as liveAt reads a place F gave the
// word at (fSame); none of a recording F did not serve, or served changed
// with no view (F may have given another word there, which keploy does not
// see). Payloads are read as searched: stamps and protocol-phase clock reads
// blanked, so a word only a stamp holds is no word both gave.
func (dc *DriftCache) sameWords(x Raw, words []map[string]bool) []map[string]bool {
	out := make([]map[string]bool, len(words))
	switch {
	case x.AsRecorded():
		return words
	case !x.Live():
		return out
	}
	rec := dc.searched(x, false)[0]
	live := dc.searched(*x.live, false)[0]
	tokensAt := func(fields []rawField, text string, f rawField) ([]map[string]bool, bool) {
		for _, g := range fields {
			if g.path == f.path && g.key == f.key {
				if g.end > len(text) {
					return nil, false
				}
				return dc.rawTokens(text[g.start:g.end]), true
			}
		}
		return nil, false
	}
	held := make([]map[string]bool, len(words))
	bad := make([]map[string]bool, len(words))
	for i := range words {
		held[i], bad[i] = map[string]bool{}, map[string]bool{}
	}
	// One side's field holding a word: the other's field of its path must
	// hold it too, or the word is not given there alike.
	read := func(fields []rawField, text string, other []rawField, otherText string, recorded bool) {
		for _, f := range fields {
			own, ok := tokensAt(fields, text, f)
			if !ok {
				continue
			}
			there, found := tokensAt(other, otherText, f)
			for i := range words {
				if i >= len(own) {
					break
				}
				for w := range own[i] {
					if !words[i][w] {
						continue
					}
					switch {
					case !found || i >= len(there) || !there[i][w]:
						bad[i][w] = true
					case recorded:
						held[i][w] = true
					}
				}
			}
		}
	}
	xb := x.payload()
	read(xb.fields, rec, x.live.payload().fields, live, true)
	read(x.live.payload().fields, live, xb.fields, rec, false)
	for i := range words {
		out[i] = map[string]bool{}
		for w := range held[i] {
			if !bad[i][w] {
				out[i][w] = true
			}
		}
	}
	return out
}

// sameHolds reports whether record's full round gave the application t at
// the fields the recording gave it at, in reading i (givenWords.same).
func (g givenWords) sameHolds(i int, t string) bool {
	if i >= len(g.same) {
		return false
	}
	k := sort.SearchStrings(g.same[i], t)
	return k < len(g.same[i]) && g.same[i][k] == t
}

// sortedWords are the words of set that minus does not hold, each a copy,
// sorted.
func sortedWords(set, minus map[string]bool) []string {
	out := make([]string, 0, len(set))
	for w := range set {
		if !minus[w] {
			out = append(out, strings.Clone(w))
		}
	}
	sort.Strings(out)
	return out
}

// holds reports whether g's answer holds t in reading i, a word of its
// stamps too where keep says they are searched as any text (stampsEchoed).
func (g givenWords) holds(i int, t string, keep bool) bool {
	for _, list := range [][][]string{g.words, g.stamped} {
		if i < len(list) {
			if k := sort.SearchStrings(list[i], t); k < len(list[i]) && list[i][k] == t {
				return true
			}
		}
		if !keep {
			break
		}
	}
	return false
}

// servedWhy says why named, a word t a value holds now in reading i, may
// have come from what F gave the application in g, a recording no earlier
// place of the call's own test is (whose: givenWhose), "" where it may not:
// where F's view of it holds t (gave: Raw.ServedLive), where one of the
// views its serves gave holds t (views: Raw.ServedChanged), and wherever
// keploy cannot tell what F gave there (unstated, unread). The recording's
// own words are holds'; these are what F gave the application there and the
// recording did not, read as of a place of the call's own test
// (unaccounted, unseenWhy): keploy follows no value of another test's, nor
// can it tell a copy of what F gave there from a value of the call's own,
// so it refuses rather than learn the word as noise (fail closed). A word
// the recording holds (held) is no word only F gave: the views are not
// searched for it (givenWhy read it with the recording's words).
func (g givenWords) servedWhy(i int, t, named, whose string, held bool) string {
	in := func(list [][]string) bool {
		if i >= len(list) {
			return false
		}
		k := sort.SearchStrings(list[i], t)
		return k < len(list[i]) && list[i][k] == t
	}
	of := ", a recording record's full round gave the application before the call that " + whose
	switch {
	case g.unstated:
		return named + " may stand in " + g.mock + of + ", where its adapter did not say what it gave the application there: keploy cannot tell whether it came from there"
	case g.unread:
		return named + " may stand in " + g.mock + of + ", served changed, where keploy could not read what it gave the application: keploy cannot tell whether it came from there"
	case held:
		return ""
	case in(g.gave):
		return named + " appears in " + g.mock + " as record's full round gave it, where the recording did not give the application that word" + of + ": keploy cannot tell whether it came from there"
	}
	for _, view := range g.views {
		if in(view) {
			return named + " appears in " + g.mock + " as record's full round gave it in one of " + strconv.Itoa(len(g.views)) + " serves that gave the application different views of it, where the recording did not give the application that word" + of + ": keploy cannot tell which of them the application read, nor whether it came from there"
		}
	}
	return ""
}

// inAnswer says how t stands in g's answer in reading i, "" where it does
// not: as the recording gave it (its words and the words only its stamps
// hold), or as record's full round gave it (its view's words, stamps too).
func (g givenWords) inAnswer(i int, t string) string {
	for _, l := range []struct {
		list [][]string
		as   string
	}{{g.words, "as the recording gave it"}, {g.stamped, "as the recording gave it"}, {g.gave, "as record's full round gave it"}} {
		if i < len(l.list) {
			if k := sort.SearchStrings(l.list[i], t); k < len(l.list[i]) && l.list[i][k] == t {
				return l.as
			}
		}
	}
	return ""
}

// readAnswerWords are the words of x's answer in every declared form, by
// reading, where the word rule reads a recording F gave the application
// (givenWords): of each field of its answer, its keys too (of a kind whose
// spec holds its request and its answer in one field, or a recording with
// no fields read, its whole text: answerApart), with its stamps (unless
// keep) and its answer's own clock reads blanked, as searchedTokens reads a
// payload (rules 2 and 3). What F gives the application is an answer, never
// a request the application sent in another test: a recording that holds no
// answer apart from its request has no words here.
func (dc *DriftCache) readAnswerWords(x Raw, keep bool) []map[string]bool {
	if !answerApart(x.payload().fields) {
		return dc.searchedTokens(x, keep)
	}
	if keep {
		x = x.withoutStamps()
	}
	xb := x.payload()
	text := blankStamps(xb.text, xb.stamps)
	keys := clockKeys(x)
	var forms []string
	for _, f := range xb.fields {
		if f.end > len(text) || !answerPath(f.path) {
			continue
		}
		for _, form := range textForms(text[f.start:f.end]) {
			if len(keys) > 0 {
				form = blankSpans(form, func(sp instant) bool { return sp.keyed && keys[sp.key] })
			}
			forms = append(forms, form)
		}
	}
	return tokensOfForms(forms)
}

// holdsAnswer reports whether a recording whose fields are fields holds an
// answer the word rule reads (readAnswerWords): all of it where its kind
// does not hold its answer apart from its request, else a field of its
// answer (answerPath).
func holdsAnswer(fields []rawField) bool {
	if !answerApart(fields) {
		return true
	}
	for _, f := range fields {
		if answerPath(f.path) {
			return true
		}
	}
	return false
}

// Mock is the name of g's mock.
func (g GivenPlaces) Mock() string { return g.mock }

// Read reports whether g's recording was read (Raw.Read).
func (g GivenPlaces) Read() bool { return g.read }

// Span is where F first and last served g's mock (ServedIn).
func (g GivenPlaces) Span() (first, last int) { return g.first, g.last }

// ServedIn is g as F served its mock first at the first and last at the last
// of its serves in the set (Raw.ServedIn).
func (g GivenPlaces) ServedIn(first, last int) GivenPlaces {
	g.first, g.last = first, last
	return g
}

// payloadUnread is why every changed instant the offset rule reads and every
// word the word rule reads is refused where e, an exchange a learn step
// served leaf by leaf, came with a payload RawOf could not read, or with
// none at all (Earlier.Payload the zero Raw: a producer that gives none):
// its answer's places cannot be searched, so whether what changed was built
// on them cannot be told.
func payloadUnread(e Earlier) string {
	name := e.Payload.mock
	if name == "" {
		name = e.Mock
	}
	if name == "" {
		name = "an exchange of the test"
	}
	read := "could not read " + name + ", an exchange record's full round served the application leaf by leaf before the call, from its mock (learn.RawOf)"
	if !e.Payload.fromMock() {
		read = "was given no payload of " + name + ", an exchange record's full round served the application leaf by leaf before the call (learn.Earlier.Payload)"
	}
	return "keploy " + read + ", so it cannot tell whether what changed was built on its answer: a fault of keploy's, which keploy reports rather than learn anything"
}

// placesUnread is rawUnread of the recordings set holds.
func placesUnread(set []GivenPlaces) string {
	for _, g := range set {
		if !g.read {
			return rawUnread([]Raw{{mock: g.mock}})
		}
	}
	return ""
}

// setPlaces are the places (GivenPlaces) of each recording F served in one
// set, each read from its recording once for the set, by its identity
// (mockID): Order.GivenPlaces.
//
// views keeps, by the recording's identity and the view's place among those
// the set keeps of it (Order.viewsLocked's keys), the words of each view F's
// serves gave the application that a read of the set read (statedWords): a
// view the tracker keeps of a recording stays the same for the rest of the
// set, and it keeps at most maxLiveViews of one.
type setPlaces struct {
	mu    sync.Mutex
	of    map[string]GivenPlaces
	views map[string][][]string
}

func newSetPlaces() *setPlaces {
	return &setPlaces{of: map[string]GivenPlaces{}, views: map[string][][]string{}}
}

// GivenPlaces is m as the offset rule reads it among the recordings F gave
// the application earlier in the set (GivenPlaces), served where the read saw
// F serve it (Of): read from its recording, its answer read back where the
// agent parked it on disk (ReadRaw), once for the set the read was of, and
// only its places kept. err names an answer that cannot be read back;
// nothing is kept of it, nor of a recording that could not be read. What F
// gave the application there is read as the statements its adapters made of
// the serves of it the read saw say (statedWords), as Order.ServedView reads
// a Raw: a word a value holds now that may stand in what F gave there and
// the recording did not is refused (givenWords.servedWhy), never learned as
// noise.
func (o Order) GivenPlaces(m *models.Mock) (GivenPlaces, error) {
	g, err := o.givenPlaces(m)
	if err != nil {
		return GivenPlaces{}, err
	}
	g.words = o.statedWords(m, g.words, g.answers)
	return g, nil
}

// statedWords is w, the words of m's recording as the set reads it
// (givenPlaces: served, with no view stated), with what the statements of
// F's serves of m the read saw say F gave the application there, as
// Order.ServedView reads them for a Raw: nothing more where every serve was
// told as recorded; each view's words where they were told with views (gave:
// one view; views: more than one, or one and the recording); every word
// where a view could not be kept or read (unread); and every word where a
// serve was told of nothing, or keploy cannot tell (unstated). Words only
// where m's recording holds an answer (answers): one of a kind that holds
// its answer apart from its request and holds none gave the application
// nothing. What both gave (same) stays as read with no view stated, none:
// it lets no word go, the fail-closed side.
func (o Order) statedWords(m *models.Mock, w givenWords, answers bool) givenWords {
	if o.t == nil || !answers {
		return w
	}
	id := MockID(m)
	o.t.mu.Lock()
	recorded, keys, loads, unread, ok := o.viewsLocked(id)
	o.t.mu.Unlock()
	w.unstated, w.unread, w.gave, w.views = !ok, ok && unread, nil, nil
	if !ok || unread || len(loads) == 0 {
		return w
	}
	views := make([][][]string, 0, len(loads))
	for k, load := range loads {
		words, read := o.places.viewWords(m, id, keys[k], load)
		if !read {
			w.unread = true
			return w
		}
		views = append(views, words)
	}
	if !recorded && len(views) == 1 {
		w.gave = views[0]
		return w
	}
	// A serve told as recorded gave the recording itself: one of the views,
	// which holds no word the recorded text does not (Order.ServedView).
	if recorded {
		views = append(views, w.words)
	}
	w.views = views
	return w
}

// viewWords is the words of the answer of the view load gives of m, which F
// served (viewRaw), in each reading, stamps read as any text, as wordsOf
// reads a view: kept by c, a set's, by m's identity (id) and the view's
// place among those the set keeps of it (k). false where the view cannot be
// read; nothing is kept of it then.
func (c *setPlaces) viewWords(m *models.Mock, id string, k int, load func() (*models.Mock, error)) ([][]string, bool) {
	key := id + "\x00" + strconv.Itoa(k)
	if c != nil {
		c.mu.Lock()
		words, ok := c.views[key]
		c.mu.Unlock()
		if ok {
			return words, true
		}
	}
	r := Raw{mock: m.Name, call: callOf(m), served: true}
	lv, ok := r.viewOf(viewRaw(m, load))
	if !ok {
		return nil, false
	}
	var words [][]string
	for _, w := range NewDriftCache().answerWords(*lv, true) {
		words = append(words, sortedWords(w, nil))
	}
	if c != nil {
		c.mu.Lock()
		c.views[key] = words
		c.mu.Unlock()
	}
	return words, true
}

// givenPlaces is GivenPlaces with no statement read: m's recording served
// where the read saw F serve it, with no view stated.
func (o Order) givenPlaces(m *models.Mock) (GivenPlaces, error) {
	first, last := o.Of(m)
	id := MockID(m)
	if c := o.places; c != nil {
		c.mu.Lock()
		g, ok := c.of[id]
		c.mu.Unlock()
		if ok {
			return g.ServedIn(first, last), nil
		}
	}
	x, err := ReadRaw(m, true)
	if err != nil {
		return GivenPlaces{}, err
	}
	g := PlacesOf(x)
	if c := o.places; c != nil && g.read {
		c.mu.Lock()
		c.of[id] = g
		c.mu.Unlock()
	}
	return g.ServedIn(first, last), nil
}

// fieldRoot is the spec's own field a raw field's path starts in
// ("Res" of "Res.body", "MongoResponses" of "MongoResponses[0].Message").
func fieldRoot(path string) string {
	if i := strings.IndexAny(path, ".["); i >= 0 {
		return path[:i]
	}
	return path
}

// answerApart reports whether the spec fields stand in holds its answer
// apart from its request: one of them stands in the spec's answer or its
// request (answerPath, requestPath: one of answerFields or requestFields, or
// a field of a Postgres v3 query's spec), and only those of its answer are
// read. A recording of such a kind that holds no answer (a generic parser's
// frames the application sent, that nothing answered) gave the application
// nothing. A kind whose spec holds its request and its answer in one field
// with nothing below it to tell them apart (a Postgres v3 session, catalog,
// data or generator recording) has neither, and all its fields are read.
func answerApart(fields []rawField) bool {
	for _, f := range fields {
		if answerPath(f.path) || requestPath(f.path) {
			return true
		}
	}
	return false
}

// offsetMatch is a changed instant of a value (rec, live) built on a moved
// place (hr as the recording gave it, hf as record's full round did) by an
// offset of about off.
type offsetMatch struct {
	rec, live moment
	hr, hf    offsetPlace
	off       time.Duration
	// unread: a place could not be read (offsetPlaces.unread), and is the
	// reason (offsetWhy).
	unread string
}

// offsetOf reports whether some offset o puts hr+o in rec's unit and hf+o in
// live's (each moment the start of its unit, of its resolution), all read on
// one line, in nanoseconds (of a time of day, around the day: mod, else 0):
// about how much, and ok. About how much is the whole number of the coarser
// of the two resolutions among the offsets that do, where one is (an
// application adds a whole offset: five minutes), else the middle of them,
// to that resolution.
func offsetOf(rec, live, hr, hf int64, rres, lres time.Duration, mod int64) (time.Duration, bool) {
	if mod > 0 {
		// The places' moments, around the day, nearest the value's.
		near := func(h, v int64) int64 {
			d := ((v-h)%mod + mod) % mod
			if d > mod/2 {
				d -= mod
			}
			return v - d
		}
		hr, hf = near(hr, rec), near(hf, live)
	}
	lo := max(rec-hr, live-hf)
	hi := min(rec+int64(rres)-hr, live+int64(lres)-hf)
	if lo >= hi {
		return 0, false
	}
	res := int64(max(rres, lres))
	q := lo / res
	if q*res < lo {
		q++
	}
	if q*res < hi {
		return time.Duration(q * res), true
	}
	return time.Duration((lo + hi) / 2).Round(time.Duration(res)), true
}

// offsetFrom is the first of pairs (a value's changed instants) built on a
// moved place by an offset (offsetMatch): the test case's request's moved
// parts first, then the answers' places, in the order the places are read
// (by key, in keyLess's order, then by moment, then in the order the index
// took them), each place the recording gave the application (recorded)
// against each another recording of its call F gave it there (given), by
// moment, where not both are what both runs gave it in the order the
// recording did (inRecordedOrder). ok is false when none is.
//
// A pair holds a place of a recording that deviates (deviating): every
// other two places the call reads are what both runs gave it, in the
// recording's order. So the rule searches only from those places, each
// against its call's places at its key within the value's move (the rest of
// the set's places it never reads), and keeps the first pair in that order:
// a call that both runs gave the same recordings in one order reads none.
func (op *offsetPlaces) offsetFrom(pairs []driftedInstant) (offsetMatch, bool) {
	if op == nil {
		return offsetMatch{}, false
	}
	if op.unread != "" && len(pairs) > 0 {
		return offsetMatch{unread: op.unread}, true
	}
	for _, p := range pairs {
		for _, g := range op.given {
			if m, ok := offsetBetween(p, g[0], g[1]); ok {
				return m, true
			}
		}
		if m, ok := op.offsetAt(p); ok {
			return m, true
		}
	}
	return offsetMatch{}, false
}

// offsetPass reports whether hf is no place F gave the application in hr's
// place: hf given by F, an answer to hr's call, and not both what both runs
// gave the application in the order the recording did: two recordings F
// gave in the test and the recording gave before the call, in one order,
// are no place F gave in another's (and neither is one recording's two
// instants). A recording made at the call's own instant the recording may
// never have given before the call, so F's serving it after the other is no
// order both runs gave: the pair stays.
func offsetPass(hr, hf offsetPlace) bool {
	return !hf.given || hf.call != hr.call || hr.given && !hr.atCall && hf.recorded && !hf.atCall && inRecordedOrder(hr, hf)
}

// offsetPair is a pair offsetAt found: the place as the recording gave it
// (hr, at hrAt, of list li), and the place F gave (a place of the index, at
// hfAt, or the mj-th of the moves at the key: moved), in the order offsetFrom
// reads them (before).
type offsetPair struct {
	li         int
	hr, hf     offsetRef
	hrAt, hfAt int64
	moved      bool
	mj         int
	m          offsetMatch
}

func (a offsetPair) before(b offsetPair) bool {
	switch {
	case a.li != b.li:
		return a.li < b.li
	case a.hrAt != b.hrAt:
		return a.hrAt < b.hrAt
	case a.hr != b.hr:
		return a.hr.rec < b.hr.rec || a.hr.rec == b.hr.rec && a.hr.i < b.hr.i
	case a.moved != b.moved:
		return !a.moved
	case a.moved:
		return a.mj < b.mj
	case a.hfAt != b.hfAt:
		return a.hfAt < b.hfAt
	}
	return a.hf.rec < b.hf.rec || a.hf.rec == b.hf.rec && a.hf.i < b.hf.i
}

// offsetAt is the first pair of the answers' places built on by p
// (offsetFrom): from each place of a deviating recording, as the recording
// gave it against what F gave within p's move of it (the first of them), and
// as F gave it against what the recording gave (the first), and from each
// move F served at a late exchange's leaf against what the recording gave.
func (op *offsetPlaces) offsetAt(p driftedInstant) (offsetMatch, bool) {
	var best offsetPair
	found := false
	keep := func(c offsetPair) {
		if !found || c.before(best) {
			best, found = c, true
		}
	}
	dated := !p.rec.key.day
	d := p.live.key.at - p.rec.key.at
	rres, lres := int64(p.rec.res), int64(p.live.res)
	for _, r := range op.deviating() {
		rec := &op.snap.recs[r]
		for i := range rec.places {
			op.examined++
			kp := &rec.places[i]
			li, ok := op.snap.listOf(kp.k)
			if !ok || found && li > best.li {
				continue
			}
			g := op.snap.lists[li].group(rec.call)
			if g == nil {
				continue
			}
			at, ref := kp.m.key.at, offsetRef{rec: r, i: int32(i)}
			if hr, _ := op.placeOf(ref); hr.recorded {
				from, to := g.within(dated, at+d-rres, at+d+lres)
				pair, ok := op.firstGiven(p, hr, g, from, to)
				if !ok {
					moved := op.moves[kp.k]
					mfrom, mto := 0, len(moved)
					if dated {
						mfrom = sort.Search(len(moved), func(j int) bool { return moved[j].m.key.at > at+d-rres })
						mto = max(mfrom, sort.Search(len(moved), func(j int) bool { return moved[j].m.key.at >= at+d+lres }))
					}
					for j := mfrom; j < mto; j++ {
						op.examined++
						if offsetPass(hr, moved[j]) {
							continue
						}
						if m, hit := offsetBetween(p, hr, moved[j]); hit {
							pair, ok = offsetPair{moved: true, mj: j, m: m}, true
							break
						}
					}
				}
				if ok {
					pair.li, pair.hr, pair.hrAt = li, ref, at
					keep(pair)
				}
			}
			if hf, _ := op.placeOf(ref); hf.given {
				from, to := g.within(dated, at-d-lres, at-d+rres)
				if pair, ok := op.firstRecorded(p, hf, g, from, to); ok {
					pair.li, pair.hf, pair.hfAt = li, ref, at
					keep(pair)
				}
			}
		}
	}
	for k, moved := range op.moves {
		li, ok := op.snap.listOf(k)
		if !ok || found && li > best.li {
			continue
		}
		for j, hf := range moved {
			op.examined++
			g := op.snap.lists[li].group(hf.call)
			if g == nil {
				continue
			}
			from, to := g.within(dated, hf.m.key.at-d-lres, hf.m.key.at-d+rres)
			if pair, ok := op.firstRecorded(p, hf, g, from, to); ok {
				pair.li, pair.moved, pair.mj = li, true, j
				keep(pair)
			}
		}
	}
	return best.m, found
}

// firstGiven is the first place of g's from-to F gave in hr's place that p
// is built on with hr (offsetBetween): its ref, moment and match.
func (op *offsetPlaces) firstGiven(p driftedInstant, hr offsetPlace, g *offsetGroup, from, to int) (offsetPair, bool) {
	for j := from; j < to; j++ {
		op.examined++
		hf, ok := op.placeOf(g.refs[j])
		if !ok || offsetPass(hr, hf) {
			continue
		}
		if m, ok := offsetBetween(p, hr, hf); ok {
			return offsetPair{hf: g.refs[j], hfAt: g.at[j], m: m}, true
		}
	}
	return offsetPair{}, false
}

// firstRecorded is the first place of g's from-to the recording gave the
// application whose place F gave hf in, that p is built on with hf: its
// ref, moment and match.
func (op *offsetPlaces) firstRecorded(p driftedInstant, hf offsetPlace, g *offsetGroup, from, to int) (offsetPair, bool) {
	for j := from; j < to; j++ {
		op.examined++
		hr, ok := op.placeOf(g.refs[j])
		if !ok || !hr.recorded || offsetPass(hr, hf) {
			continue
		}
		if m, ok := offsetBetween(p, hr, hf); ok {
			return offsetPair{hr: g.refs[j], hrAt: g.at[j], m: m}, true
		}
	}
	return offsetPair{}, false
}

// deviating are the index's recordings the call reads whose places can be
// one of a pair offsetFrom reads (offsetPass), read once for the call from
// its view: one the recording gave before the call that F did not, one F
// gave that the recording did not give before it, one recorded at the
// call's own instant, one both gave that F served in an order keploy cannot
// tell, and the later recorded of two both gave whose serves F made in an
// order the recording did not give them in (inRecordedOrder: by when their
// requests were recorded, byAt), whatever call each answers. Two places of
// other recordings are both the recording's and F's, in the recording's
// order: offsetPass passes them.
func (op *offsetPlaces) deviating() []int32 {
	if op.devRead {
		return op.dev
	}
	op.devRead = true
	dev := make([]bool, len(op.view))
	var both []int32
	for _, r := range op.snap.byAt {
		v := op.view[r]
		if !v.in {
			continue
		}
		at := op.snap.recs[r].at
		recorded := !at.After(op.at)
		switch {
		case at.Equal(op.at), recorded != v.given, v.given && v.first == 0:
			dev[r] = true
		case v.given:
			both = append(both, r)
		}
	}
	// Of two recordings both gave in an order keploy cannot read as the
	// recording's (two of one instant, or one whose first serve came no
	// later than the last of one recorded before it), the one recorded
	// later deviates: the search reads the pair from it, both ways.
	var lastBefore int32
	for i := 0; i < len(both); {
		j := i + 1
		for j < len(both) && op.snap.recs[both[j]].at.Equal(op.snap.recs[both[i]].at) {
			j++
		}
		for _, r := range both[i:j] {
			if j-i > 1 || lastBefore >= op.view[r].first {
				dev[r] = true
			}
		}
		for _, r := range both[i:j] {
			lastBefore = max(lastBefore, op.view[r].last)
		}
		i = j
	}
	for r, ok := range dev {
		if ok {
			op.dev = append(op.dev, int32(r))
		}
	}
	return op.dev
}

// offsetBetween reports whether the changed instant p is built on the place
// hr (as the recording gave it) and hf (as record's full round did) by an
// offset: two moments, read as p's are (offsetOf), a dated value against
// dated instants, a time of day against times of day (an instant's in UTC,
// or as it writes it in its own zone).
func offsetBetween(p driftedInstant, hr, hf offsetPlace) (offsetMatch, bool) {
	if hr.m.key == hf.m.key {
		return offsetMatch{}, false
	}
	m := offsetMatch{rec: p.rec, live: p.live, hr: hr, hf: hf}
	if !p.rec.key.day {
		if hr.m.key.day || hf.m.key.day {
			return offsetMatch{}, false
		}
		off, ok := offsetOf(p.rec.key.at, p.live.key.at, hr.m.key.at, hf.m.key.at, p.rec.res, p.live.res, 0)
		m.off = off
		return m, ok
	}
	// A time of day: the places' times of day, each read one way on both
	// sides.
	readings := func(h moment) []int64 {
		if h.key.day {
			return []int64{h.key.at}
		}
		return []int64{timeOfDay(h.key.at), h.wall}
	}
	rr, fr := readings(hr.m), readings(hf.m)
	for i := range rr {
		if i >= len(fr) {
			break
		}
		if off, ok := offsetOf(p.rec.key.at, p.live.key.at, rr[i], fr[i], p.rec.res, p.live.res, nsPerDay); ok {
			m.off = off
			return m, true
		}
	}
	return offsetMatch{}, false
}

// offsetWhy is why a drifted value that m names is refused (read after
// "<field> changed, and "): where its recorded moment and the moment F saw
// in it stand against a moved place, by about how much, why that shows no
// read of the clock, and how to correct it. It names no value.
func offsetWhy(m offsetMatch) string {
	if m.unread != "" {
		return m.unread
	}
	rel := "at"
	off := m.off
	switch {
	case off > 0:
		rel = off.String() + " after"
	case off < 0:
		rel = (-off).String() + " before"
	}
	unit := unitName(max(m.rec.res, m.live.res))
	gave := " holds as the recording gave it"
	switch {
	case m.hr.noTime:
		gave += noTimeNote
	case m.hr.atCall:
		gave += atCallNote
	}
	why := "its moment, to the " + unit + ", stands " + rel + " the moment " + m.hr.where + gave + ", and record's full round saw it " + rel + " the moment " + m.hf.where + " holds, which record's full round gave the application in its place: a value built on what the application was given by an offset moves so, as a read of the clock does, so keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it; otherwise it is built on " + m.hf.mock + "'s instant, which keploy cannot follow with an offset yet"
	if m.hr.mock != m.hf.mock && (m.hr.given && m.hr.first == 0 || m.hf.given && m.hf.first == 0) {
		// Both runs gave both: the order F gave them in is what is not
		// known (Order.Of), most often two requests aloft at once.
		why += ". Record's full round gave the application " + m.hr.mock + " and " + m.hf.mock + " in an order keploy cannot tell: in requests the application had in flight at once, on parallel connections, or outside any request of its own"
	}
	return why
}

// unitName names a resolution as a reason reads it: "minute", "second",
// "millisecond", or the duration.
func unitName(res time.Duration) string {
	switch res {
	case 24 * time.Hour:
		return "day"
	case time.Hour:
		return "hour"
	case time.Minute:
		return "minute"
	case time.Second:
		return "second"
	case time.Millisecond:
		return "millisecond"
	case time.Microsecond:
		return "microsecond"
	case time.Nanosecond:
		return "nanosecond"
	}
	return res.String()
}

// offsetOfValue is offsetFrom of a drifted value's changed instants
// (valueDrift.instants), the places read only where it has one.
func offsetOfValue(v *valueDrift, places func() *offsetPlaces) (offsetMatch, bool) {
	if len(v.instants) == 0 {
		return offsetMatch{}, false
	}
	return places().offsetFrom(v.instants)
}
