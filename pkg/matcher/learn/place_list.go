package learn

import (
	"cmp"
	"iter"
	"sort"
	"sync/atomic"
)

// placeList is a call's places (placesOf), in its order: each earlier
// exchange's leaves, then the recordings. A leaf's place is built, with
// its texts (withTexts), at the call's first read of a place of its
// exchange (at), never before: a call of a long test has thousands of
// earlier leaves, and a word's check reads only the few the set's postings
// name (placeIndex), so building every one of them at every call, a place
// and two text handles each, was most of what the learn step allocated on
// a long test. The place index lists a leaf from its exchange's Seen
// (decider.places), which is what its place would be built from, so it
// builds none. A recording's place is built with the list: what it holds
// is read through its payload (rawState), and a call reads each one.
//
// ex is the exchanges that list places, in order; leaves is the number of
// their places, the index of the first recording's; rest is the
// recordings' places, built, their texts given by the call's index x
// (newDecider; a list placesFrom made: every place). A nil list has no
// place, as a decider a row makes with none.
type placeList struct {
	in     Input
	x      *readerIndex
	trails map[string]bool
	ex     []exPlaces
	leaves int
	rest   []evPlace
}

// exPlaces is one earlier exchange's places in a call's list: e, its index
// in Input.Earlier; off, its first place's index in the list; ps, its
// leaves' places, nil until the call reads one (placeList.build).
type exPlaces struct {
	e, off int
	ps     []evPlace
}

// placesFrom is a list of ps, each already built (a row's decider).
func placesFrom(ps []evPlace) *placeList { return &placeList{rest: ps} }

// len is the number of the call's places.
func (l *placeList) len() int {
	if l == nil {
		return 0
	}
	return l.leaves + len(l.rest)
}

// at is the call's i-th place, built where the call had not read a place
// of its exchange yet.
func (l *placeList) at(i int) *evPlace {
	if i >= l.leaves {
		return &l.rest[i-l.leaves]
	}
	k := sort.Search(len(l.ex), func(k int) bool { return l.ex[k].off > i }) - 1
	x := &l.ex[k]
	if x.ps == nil {
		l.build(x)
	}
	return &x.ps[i-x.off]
}

// all is every place of the call with its index, in order, each built (a
// check that reads every place).
func (l *placeList) all() iter.Seq2[int, evPlace] {
	return func(yield func(int, evPlace) bool) {
		if l == nil {
			return
		}
		for k := range l.ex {
			x := &l.ex[k]
			if x.ps == nil {
				l.build(x)
			}
			for n, p := range x.ps {
				if !yield(x.off+n, p) {
					return
				}
			}
		}
		for n, p := range l.rest {
			if !yield(l.leaves+n, p) {
				return
			}
		}
	}
}

// any reports whether f holds at some place of the call, read in order.
func (l *placeList) any(f func(evPlace) bool) bool {
	for _, p := range l.all() {
		if f(p) {
			return true
		}
	}
	return false
}

// mocks is the recording or exchange each place of the call is of, with
// the place's index, in order, building none: a leaf's is its exchange's
// (Earlier.Mock), as its place names it.
func (l *placeList) mocks() iter.Seq2[int, string] {
	return func(yield func(int, string) bool) {
		if l == nil {
			return
		}
		for _, x := range l.ex {
			m := l.in.Earlier[x.e].Mock
			for n := range len(l.in.Earlier[x.e].Seen) {
				if !yield(x.off+n, m) {
					return
				}
			}
		}
		for n, p := range l.rest {
			if !yield(l.leaves+n, p.mock) {
				return
			}
		}
	}
}

// build builds x's leaves' places for the call, with their texts: what
// placesOf built for each leaf of every exchange at every call.
func (l *placeList) build(x *exPlaces) {
	in, e := l.in, l.in.Earlier[x.e]
	st, why := earlierState(in, e)
	startUp, other, scope := inScope(in, e.Mock, e.StartUp, e.ofEarlierTest())
	// A place the test the call trails left the run is the call's own
	// (Input.Trails), and so is one that test was served in its own
	// window where the call lands in it too (the application answered
	// the test before its clean-up ran, but the round ran it sooner):
	// bound there, a value is the run's variable, so a replay binds it
	// in whichever window the call lands.
	trailed := l.trails[e.Mock] && !startUp && (other && e.ofEarlierTest() || !other && in.Test == in.Trails.Test)
	if trailed {
		other = false
	}
	// Of the exchange, not of a leaf: built once for its leaves.
	outsideTest, lateCall := earlierOutsideTest(e), earlierLateCall(e)
	// rBefore of its leaves: a request leaf by the request's time, an
	// answer leaf by the answer's (none where keploy was given no
	// payload: the zero time).
	sentBefore := e.alwaysEarlier() || !e.noTime && e.At.Before(in.At)
	answeredBefore := e.alwaysEarlier() || sentBefore && answeredBy(e.At, e.Payload.answered, in.At)
	ps := make([]evPlace, 0, len(e.Seen))
	for _, s := range e.Seen {
		path := cmp.Or(s.Leaf.Path, s.Place)
		ps = append(ps, evPlace{
			name: e.Mock + " " + path, mock: e.Mock, path: path,
			rec: s.Leaf.Value, live: s.Live, side: s.Side, leaf: s.Leaf, post: s.post,
			given: e.Given, gave: e.Gave, undeclared: s.Undeclared, clock: s.Clock, offset: s.Offset, startUp: startUp, otherTest: other, trailed: trailed, scope: scope,
			at: e.At, state: st, why: why, fGave: !e.Payload.servedAfter, outsideTest: outsideTest, lateCall: lateCall,
			recAfter: !e.alwaysEarlier() && !e.noTime && e.At.After(in.At),
			rBefore:  s.Side != Response && sentBefore || s.Side == Response && answeredBefore,
		})
		// What the application got in an answer it got at the answer's
		// time (answerState), its request before or at the call's instant.
		if s.Side == Response && (st == stateBefore || why == atCallWhy) && e.Payload.isRead() {
			p := &ps[len(ps)-1]
			if ast, awhy := answerState(e.At, e.Payload.answered, in.At); ast != stateBefore {
				p.state, p.why, p.recAfter = ast, awhy, ast == stateNotBefore && !e.alwaysEarlier()
			}
		}
	}
	x.ps = withTexts(l.x, ps)
	placesBuilt.Add(int64(len(ps)))
}

// placesBuilt counts the earlier leaves' places the calls built
// (placeList.build), for the rows that bound a call's work: a call builds
// the places of the exchanges it reads, not every one of its test's.
var placesBuilt atomic.Int64
