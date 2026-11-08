package learn

import (
	"math"
	"time"
)

// A BARE EPOCH NUMBER THAT TRACKS ITS CALLS.
//
// The learn step reads a value as a read of the application's clock from a
// replay's call paired with its recording, in the two runs' windows. A call
// no replay was paired with, such as a timer's poll a set recorded outside
// every test's window (models.Mapping.Worker), has only its recordings. A
// bare number names no type, so one that only looks like an epoch
// ("2025551234", a phone number) is no read of the clock by its text. The
// recordings of one recurring call tell the two apart: a value the
// application writes from its clock ({"$lte": now-60} in epoch seconds, a
// claim's stamp) lies one constant offset from the moment of each call,
// however far apart the calls are, and a number the application keeps or
// draws does not.
//
// EpochTrack learns that offset from the recordings of one leaf of one
// recurring call, and EpochOffset.Holds checks a live call's leaf against it
// on the call's own span of the run's clock (ClockSpanOf), so a leaf read
// this way is a read of the clock checked against the run's clock, never a
// value left unchecked. It is no learned noise: the learn step's reads of
// the clock (models.MockSpec.Clocks, CheckClockRead) keep their own rules.
// A parser reads it where a call is told apart from one of the set's
// recordings by its reads of the clock alone (a background miss:
// models.UnmatchedCall.Background).

// MinEpochReads is how many recordings of a leaf, each with its own value,
// EpochTrack needs before it reads the leaf as a read of the clock: two
// values that each sit near their call's moment can do so by chance (a
// counter that started near now), three that keep one offset across calls
// made at three moments do not.
const MinEpochReads = 3

// EpochRead is one recording's evidence of a leaf that holds a bare epoch
// number: Value, the leaf's text as recorded, and Span, when the application
// can have read its clock for it on the recording's app clock (Window.Rec:
// after the data-plane call before it reached keploy, no later than this call
// did).
type EpochRead struct {
	Value string
	Span  Span
}

// EpochOffset is what EpochTrack learned of a leaf: the application wrote it
// as its clock's reading moved by an offset in [Lo, Hi] (the value less the
// moment of the read: 0 for a stamp of now, -60s for "now less a minute"),
// at the layout's resolution Res (a second for ten digits, a millisecond for
// thirteen). Lo and Hi bound the offsets every recording allows (a read can
// have been made anywhere in its call's span), so the more recordings, the
// narrower they are.
type EpochOffset struct {
	Lo, Hi time.Duration
	Res    time.Duration
}

// EpochTrack reports whether reads, the recordings of one leaf of one call
// (its values and spans), are reads of the application's clock moved by one
// offset, and the offsets they allow. Every value must be a bare epoch
// number whole (no other text: "id-1761700000" is none), all at one
// resolution; at least MinEpochReads of them, each in a span that starts,
// must differ (a value that never moved is a constant, however near now it
// sits); and one offset must place every recording's read in its own span
// (the offsets each read allows, value less span, must meet), a span with no
// start (the set's first call) holding the read no later than its end. A
// value the application keeps or draws (a phone number, a counter, a random
// id) meets none of that across calls made at different moments.
func EpochTrack(reads []EpochRead) (EpochOffset, bool) {
	e := EpochOffset{Lo: math.MinInt64, Hi: math.MaxInt64}
	moved := map[time.Time]bool{}
	for i, r := range reads {
		at, res, ok := readEpoch(r.Value)
		if !ok || i > 0 && res != e.Res {
			return EpochOffset{}, false
		}
		e.Res = res
		// A span with no end raises the offsets' low end to the largest
		// there is (the value less the zero time), which no span's start
		// meets: such a recording is never learned.
		e.Lo = max(e.Lo, at.Sub(r.Span.To.Truncate(res)))
		// A span with no start (no call before this one: the set's first)
		// bounds the read only at its end: the application read its clock
		// for the call no later than it sent it. It narrows the offsets'
		// low end alone, and is no evidence that the leaf moves with the
		// clock: the read can have been made at any earlier moment.
		if r.Span.From.IsZero() {
			continue
		}
		e.Hi = min(e.Hi, at.Sub(r.Span.From.Truncate(res)))
		moved[at] = true
	}
	if len(moved) < MinEpochReads || e.Lo > e.Hi {
		return EpochOffset{}, false
	}
	return e, true
}

// Holds reports whether live, the leaf's text in a call made now, is the
// read of the clock e learned, made in s, the call's span on the run's app
// clock (ClockSpanOf: after the data-plane call before it reached keploy, no
// later than this one did): a bare epoch number whole, at e's resolution,
// whose moment (live less an offset e allows) falls in s, each bound read at
// the resolution. A live value that does not track the run's clock (a
// constant left over from the recording, a value an hour off) does not
// hold, and neither does any value on a span with no start.
func (e EpochOffset) Holds(live string, s Span) bool {
	v, res, ok := readEpoch(live)
	if !ok || res != e.Res || s.From.IsZero() {
		return false
	}
	return v.Sub(s.To.Truncate(res)) <= e.Hi && e.Lo <= v.Sub(s.From.Truncate(res))
}
