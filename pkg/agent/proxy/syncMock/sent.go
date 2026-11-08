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
// started first is let go: unnamedFirst and unnamedThrough keep their
// earliest start and latest end, which a loss says it can no longer revoke
// past. Guarded by the manager's sharedLossMu; the zero value is empty.
type sentList struct {
	tcs     []sentTC
	byName  map[string]time.Time
	longest time.Duration
	// letGo is set once some were let go, with unnamedFirst and
	// unnamedThrough.
	letGo                        bool
	unnamedFirst, unnamedThrough time.Time
	// firstUnknown is set when a test case it no longer names may have been
	// revoked (forget): it may have been the earliest of those let go, so the
	// recording's first test case is no longer known (first).
	firstUnknown bool
}

// add notes the test case named name, whose window is [start, end] (start
// no later than end), as sent.
func (l *sentList) add(name string, start, end time.Time) {
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
		if !l.letGo || tc.start.Before(l.unnamedFirst) {
			l.unnamedFirst = tc.start
		}
		if !l.letGo || tc.end.After(l.unnamedThrough) {
			l.unnamedThrough = tc.end
		}
		l.letGo = true
		delete(l.byName, tc.name)
	}
	k := copy(l.tcs, l.tcs[half:])
	clear(l.tcs[k:])
	l.tcs = l.tcs[:k]
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
func (l *sentList) revokeReaching(from, to time.Time, revoke func(name string)) int {
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
// of its own (its own mock dropped, its window given up): it is not sent,
// so it is not the recording's first. One it no longer names may have been
// let go, which it cannot tell from one never sent: then the recording's
// first test case is no longer known.
func (l *sentList) forget(name string) {
	start, ok := l.byName[name]
	if !ok {
		if l.letGo {
			l.firstUnknown = true
		}
		return
	}
	delete(l.byName, name)
	for i := sort.Search(len(l.tcs), func(k int) bool { return !l.tcs[k].start.Before(start) }); i < len(l.tcs) && l.tcs[i].start.Equal(start); i++ {
		if l.tcs[i].name == name {
			l.tcs = slices.Delete(l.tcs, i, i+1)
			return
		}
	}
}

// first reports the earliest window start among the test cases sent and
// not revoked, and false when it knows none: none was sent, or one let go
// may have been revoked since.
func (l *sentList) first() (time.Time, bool) {
	switch {
	case l.firstUnknown:
		return time.Time{}, false
	case l.letGo && (len(l.tcs) == 0 || l.unnamedFirst.Before(l.tcs[0].start)):
		return l.unnamedFirst, true
	case len(l.tcs) > 0:
		return l.tcs[0].start, true
	}
	return time.Time{}, false
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
