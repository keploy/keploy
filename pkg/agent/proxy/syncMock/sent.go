package manager

import (
	"slices"
	"sort"
	"time"
)

// sentTC is a test case the recording's stream sent, by name and its
// window (SendingTestCase).
type sentTC struct {
	name       string
	start, end time.Time
}

// maxSentTCs bounds the test cases a recording names as sent
// (SendingTestCase), so a long recording's list stays in memory's bound
// (no more than a few hundred KB): past it the half that started first is
// let go, so it names between maxSentTCs/2 and maxSentTCs of them.
const maxSentTCs = maxDroppedTCNames

// sentList is the test cases a recording's stream sent (SendingTestCase)
// and still names, sorted by window start, so a loss finds those its call
// reaches by a search, not a walk of them all: one whose window ends at or
// after an instant t started at or after t less the longest window noted
// (longest). Those revoked are taken out. Past maxSentTCs, the half that
// started first is let go, and a test case sent with no name is never
// named: unnamedFirst and unnamedThrough keep their earliest start and
// latest end, which a loss says it cannot revoke past. Guarded by the
// manager's sharedLossMu; the zero value is empty.
type sentList struct {
	tcs     []sentTC
	byName  map[string]time.Time
	longest time.Duration
	// letGo is set once some were let go or sent with no name, with
	// unnamedFirst and unnamedThrough; halved once some were let go.
	letGo, halved                bool
	unnamedFirst, unnamedThrough time.Time
	// firstLetGo is the name of the test case at unnamedFirst, the one it
	// does not name that started first: "" for one sent with no name, which
	// no revoke names. Only its revoke can move the first of those it does
	// not name.
	firstLetGo string
	// firstUnknown is set once a revoke the client applies took out the
	// test case at unnamedFirst (forget): the first of those it does not
	// name is no longer known, so the recording's first test case is known
	// only to be no later than the first it still names (first).
	firstUnknown bool
	// keptFirst is the earliest start of a test case taken out by a revoke
	// the client does not apply (it cannot delete): it stays in the
	// recording, so it still counts toward the first. Zero when none.
	keptFirst time.Time
}

// add notes the test case named name, whose window is [start, end] (start
// no later than end), as sent. One with no name, which no revoke can name,
// is one it does not name (unnamed): a loss over its window says so.
func (l *sentList) add(name string, start, end time.Time) {
	if name == "" {
		l.noteUnnamed("", start, end)
		return
	}
	if len(l.tcs) == maxSentTCs {
		l.letGoHalf()
	}
	if d := end.Sub(start); d > l.longest {
		l.longest = d
	}
	i := sort.Search(len(l.tcs), func(k int) bool { return l.tcs[k].start.After(start) })
	l.tcs = slices.Insert(l.tcs, i, sentTC{name: name, start: start, end: end})
	if l.byName == nil {
		l.byName = make(map[string]time.Time)
	}
	l.byName[name] = start
}

// letGoHalf lets go the half of the test cases named that started first,
// at once, so the cap costs a copy per maxSentTCs/2 test cases, not one per
// test case.
func (l *sentList) letGoHalf() {
	half := len(l.tcs) / 2
	for _, tc := range l.tcs[:half] {
		l.noteUnnamed(tc.name, tc.start, tc.end)
		delete(l.byName, tc.name)
	}
	l.halved = true
	k := copy(l.tcs, l.tcs[half:])
	clear(l.tcs[k:])
	l.tcs = l.tcs[:k]
}

// noteUnnamed notes the test case named name ("" for none), sent with the
// window [start, end], as one it does not name: let go, or sent with no
// name.
func (l *sentList) noteUnnamed(name string, start, end time.Time) {
	if !l.letGo || start.Before(l.unnamedFirst) {
		l.unnamedFirst, l.firstLetGo = start, name
	}
	if !l.letGo || end.After(l.unnamedThrough) {
		l.unnamedThrough = end
	}
	l.letGo = true
}

// from returns the index of the first test case named that may end at or
// after t: none before it does.
func (l *sentList) from(t time.Time) int {
	lo := t.Add(-l.longest)
	return sort.Search(len(l.tcs), func(k int) bool { return !l.tcs[k].start.Before(lo) })
}

// revokeReaching takes out every test case named whose window ends at or
// after from, and those whose window starts after to, when to is not zero,
// are kept: it hands each one taken out to revoke, and returns how many.
// applied says whether the client deletes them: one that cannot keeps them
// in the recording, so they still count toward its first (keptFirst).
func (l *sentList) revokeReaching(from, to time.Time, applied bool, revoke func(name string)) int {
	lo, hi := l.from(from), len(l.tcs)
	if !to.IsZero() {
		hi = sort.Search(len(l.tcs), func(k int) bool { return l.tcs[k].start.After(to) })
	}
	k := lo
	for _, tc := range l.tcs[lo:hi] {
		if tc.end.Before(from) {
			l.tcs[k] = tc
			k++
			continue
		}
		delete(l.byName, tc.name)
		if !applied {
			l.keep(tc.start)
		}
		revoke(tc.name)
	}
	n := hi - k
	if n > 0 {
		k += copy(l.tcs[k:], l.tcs[hi:])
		clear(l.tcs[k:])
		l.tcs = l.tcs[:k]
	}
	return n
}

// forget takes out the test case named name, revoked by name for a cause
// of its own (its own mock dropped, its window given up). When the client
// deletes it (applied), it is not in the recording, so it is not its first;
// a client that cannot keeps it, and it still counts toward the first
// (keptFirst). A name it does not hold was let go or never sent, which it
// cannot tell apart: it moves the first only when it is the one let go
// that started first (firstLetGo), and the first is then no longer known.
func (l *sentList) forget(name string, applied bool) {
	start, ok := l.byName[name]
	if !ok {
		if applied && name != "" && name == l.firstLetGo {
			l.firstUnknown = true
		}
		return
	}
	delete(l.byName, name)
	if !applied {
		l.keep(start)
	}
	for i := sort.Search(len(l.tcs), func(k int) bool { return !l.tcs[k].start.Before(start) }); i < len(l.tcs) && l.tcs[i].start.Equal(start); i++ {
		if l.tcs[i].name == name {
			l.tcs = slices.Delete(l.tcs, i, i+1)
			return
		}
	}
}

// keep notes start as that of a revoked test case the client keeps
// (keptFirst).
func (l *sentList) keep(start time.Time) {
	if l.keptFirst.IsZero() || start.Before(l.keptFirst) {
		l.keptFirst = start
	}
}

// first reports the earliest window start among the test cases sent and
// still in the recording (not revoked, or revoked and kept by the client),
// with ok false when it knows none: none was sent, or the first of those it
// does not name was revoked (firstUnknown) and it holds no other. exact is
// false when the first is known only to be no later than at: the earliest
// test case it still names or the client kept, which is sent and still in
// the recording, so a mock requested at or after at is not before the
// first, and one requested before it may be.
func (l *sentList) first() (at time.Time, ok, exact bool) {
	pick := func(t time.Time) {
		if !ok || t.Before(at) {
			at, ok = t, true
		}
	}
	if l.letGo && !l.firstUnknown {
		pick(l.unnamedFirst)
	}
	if len(l.tcs) > 0 {
		pick(l.tcs[0].start)
	}
	if !l.keptFirst.IsZero() {
		pick(l.keptFirst)
	}
	return at, ok, !l.firstUnknown
}

// unnamedReaching reports the latest window end of a test case let go that
// reaches from or after it, which a loss from from cannot revoke, and the
// zero time when there is none (unnamedThrough is zero until one is let go,
// and from never is).
func (l *sentList) unnamedReaching(from time.Time) time.Time {
	if l.unnamedThrough.Before(from) {
		return time.Time{}
	}
	return l.unnamedThrough
}
