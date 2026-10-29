package learn

import (
	"time"
)

// AN EPOCH NUMBER THAT TRACKS ITS CALLS.
//
// A bare number can be a read of the application's clock: a timer's "older
// than now" filter written in epoch seconds ({"$lte": now-60}), a claim's
// stamp ({"$set": {"claimedAt": now}}). Its text names no type, so a number
// that only looks like one ("2025551234", a phone number) is no read of the
// clock, and ClockOnly reads an epoch only in a typed date. The recordings
// of one call tell the two apart: a timer makes its call again and again,
// and a value it writes from its clock lies one constant offset from the
// moment of each call, however far apart the calls are, which a number the
// application keeps or draws does not.
//
// EpochTrack learns that offset from the recordings of one call's leaf, and
// EpochOffset.Holds checks a live call's leaf against it on the call's own
// span of the run's clock, so a leaf learned this way is a read of the clock
// checked against the run clock, never a value left unchecked.

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
// did, as Tracker.CallWindow gives it).
type EpochRead struct {
	Value string
	Span  Span
}

// EpochOffset is what EpochTrack learned of a leaf: the application wrote it
// as its clock's reading moved by an offset in [Lo, Hi] (the value less the
// moment of the read: 0 for a stamp of now, -60s for "now less a minute"), at
// the layout's resolution Res (a second for ten digits, a millisecond for
// thirteen). Lo and Hi bound the offsets every recording allows (a read can
// have been made anywhere in its call's span), so the more recordings, the
// narrower they are.
type EpochOffset struct {
	Lo, Hi time.Duration
	Res    time.Duration
}

// EpochTrack reports whether reads, the recordings of one leaf of one call
// (its values and spans), are reads of the application's clock moved by one
// offset, and the offsets they allow. Every value must be a bare epoch number
// whole (no other text: "id-1761700000" is not one), all at one resolution,
// each with a known span; at least MinEpochReads of them must differ (a value
// that never moved is a constant, however near now it sits); and one offset
// must place every recording's read in its own span (the offsets each read
// allows, value less span, must meet). A value the application keeps or
// draws (a phone number, a counter, a random id) meets none of that across
// calls made at different moments.
func EpochTrack(reads []EpochRead) (EpochOffset, bool) {
	rs := make([]clockReading, len(reads))
	for i, r := range reads {
		v, res, ok := readEpoch(r.Value)
		if !ok {
			return EpochOffset{}, false
		}
		rs[i] = clockReading{at: v, res: res, span: r.Span}
	}
	return trackOffset(rs)
}

// clockReading is one recording's read of the clock as its leaf reads (a
// bare epoch number's instant, readEpoch, or a typed instant's, InstantOf):
// at, at resolution res, made in span (EpochRead.Span).
type clockReading struct {
	at   time.Time
	res  time.Duration
	span Span
}

// trackOffset is EpochTrack of readings already read.
func trackOffset(rs []clockReading) (EpochOffset, bool) {
	var e EpochOffset
	seen := map[time.Time]bool{}
	for i, r := range rs {
		// A span with no start (no call before this one) is no span: the
		// read can have been made at any earlier moment. One with no end
		// places the read before every instant, which no offset meets.
		if r.span.From.IsZero() || i > 0 && r.res != e.Res {
			return EpochOffset{}, false
		}
		lo, hi := r.at.Sub(r.span.To.Truncate(r.res)), r.at.Sub(r.span.From.Truncate(r.res))
		if i == 0 {
			e = EpochOffset{Lo: lo, Hi: hi, Res: r.res}
		} else {
			e.Lo, e.Hi = max(e.Lo, lo), min(e.Hi, hi)
		}
		seen[r.at] = true
	}
	if len(seen) < MinEpochReads || e.Lo > e.Hi {
		return EpochOffset{}, false
	}
	return e, true
}

// Holds reports whether live, the leaf's text in a call made now, is the
// read of the clock e learned, made in s, the call's span on the run's app
// clock (Window.F's meaning for the run that makes it: after the data-plane
// call before it reached keploy, no later than this one did): a bare epoch
// number whole, at e's resolution, whose moment (live less an offset e
// allows) falls in s, each bound read at the resolution. A live value that
// does not track the run's clock (a constant left over from the recording, a
// value an hour off) does not hold.
func (e EpochOffset) Holds(live string, s Span) bool {
	v, res, ok := readEpoch(live)
	return ok && e.holds(v, res, s)
}

// holds is Holds of a live read already read: v at resolution res.
func (e EpochOffset) holds(v time.Time, res time.Duration, s Span) bool {
	if res != e.Res || s.From.IsZero() {
		return false // a span with no end meets no offset (EpochTrack)
	}
	return v.Sub(s.To.Truncate(res)) <= e.Hi && e.Lo <= v.Sub(s.From.Truncate(res))
}
