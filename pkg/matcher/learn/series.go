package learn

import (
	"sort"
	"strings"
	"time"
)

// A RECURRING CALL'S SERIES.
//
// A timer makes one call again and again, the same request each time but
// for the reads of its clock it writes into it: a field its decoder types
// as an instant ({"$date": …}), or a bare epoch number EpochTrack reads as
// the clock ({"$lte": now-60}). Each recording of such a call is a request
// of its own to exact serving, so a replay that makes the call at other
// moments, or once more, misses it. The series is the call's shape modulo
// those reads: what every recording of it holds, with each read of the
// clock known by the offset it keeps from its call. Judging marks the
// recordings of a series as the application's background work set-wide,
// and keploy answers a call of a marked series (Series.Holds) from the
// newest recording of it: every value as recorded, every read of the clock
// checked against the run's clock on the call's own span, so no value is
// left unchecked and a read off the run's clock is a miss.

// SeriesRead is one recording of a call: Shape, its request as the learn
// step reads it, its values typed (an Instant leaf where the decoder types
// the field as one), and Span, when the application can have read its
// clock for it on the recording's app clock (Window.Rec, EpochRead.Span).
type SeriesRead struct {
	Shape Shape
	Span  Span
}

// ClockLeaf is a leaf of a series that reads the application's clock: Leaf,
// as the series' last recording holds it (its Type says how it is written:
// a Number is a bare epoch number, an Instant a typed one), and the offset
// from its call that every recording of the series kept (EpochTrack).
type ClockLeaf struct {
	Leaf   Leaf
	Offset EpochOffset
}

// Series is one call's shape modulo its reads of the clock, as SeriesOf
// learned it from the recordings of the call (Members): the operation Key
// and Fixed parts they share, Values, the leaves every recording holds with
// one value (by Path), and Clock, the leaves each recording holds as a read
// of its clock moved by one offset (by Path). Members are the recordings'
// indexes in the reads SeriesOf was given, in that order: the last is the
// newest when the reads are in the order of their calls.
type Series struct {
	Key     string
	Fixed   map[string]string
	Values  map[string]Leaf
	Clock   map[string]ClockLeaf
	Members []int
}

// SeriesOf parts reads, the recordings of a set (in the order of their
// calls), into series: recordings of one operation key, with the same fixed
// parts and the same leaves (by path and type), are one series when each
// leaf either holds one value in all of them or, written as a bare epoch
// number or a typed instant, tracks every recording's call by one offset
// (EpochTrack: MinEpochReads values at least, every read inside its own
// span). A leaf that varies but tracks no offset (a phone number, a counter,
// an id the application draws, two reads only) is a value: the recordings
// part by it, and each part is learned again, so a call that only looks
// like a read of the clock is never answered as one. A recording no other
// recording shares a shape with is a series of its own, all its leaves
// values. known excuses leaves (keploy.yml's noise, by the recorded leaf):
// an excused leaf is no value of a series, and only its type is kept where
// known says so (typed). Every read is in exactly one series.
func SeriesOf(reads []SeriesRead, known Known) []Series {
	if known == nil {
		known = None
	}
	b := seriesBuilder{reads: reads, known: known}
	groups := map[string][]int{}
	var order []string
	for i, r := range reads {
		k := b.shapeKey(r.Shape)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	var out []Series
	for _, k := range order {
		out = append(out, b.split(groups[k], b.candidates(groups[k]))...)
	}
	return out
}

// seriesBuilder holds what SeriesOf reads its series from.
type seriesBuilder struct {
	reads []SeriesRead
	known Known
}

// compared reports whether l, a recorded leaf, is compared at all (not
// excused, or excused typed: its type is still compared), and by value.
func (b seriesBuilder) compared(l Leaf) (present, byValue bool) {
	excused, typed := b.known.Excused(l)
	if !excused {
		return true, true
	}
	return typed, false
}

// shapeKey is what recordings of one shape share: the operation key, the
// fixed parts, and each compared leaf's path and type, all in one order.
func (b seriesBuilder) shapeKey(s Shape) string {
	var parts []string
	for n, v := range s.Fixed {
		parts = append(parts, "f\x00"+n+"\x00"+v)
	}
	for p, l := range s.Leaves {
		if present, _ := b.compared(l); present {
			parts = append(parts, "l\x00"+p+"\x00"+string(l.Type))
		}
	}
	sort.Strings(parts)
	return s.Key + "\x01" + strings.Join(parts, "\x01")
}

// candidates are the paths of the leaves of members (recordings of one
// shape) that may be reads of the clock: compared by value, named alone,
// and a bare epoch number or a typed instant in every one of them.
func (b seriesBuilder) candidates(members []int) map[string]bool {
	out := map[string]bool{}
	for p, l := range b.reads[members[0]].Shape.Leaves {
		if _, byValue := b.compared(l); !byValue || l.Unnamed {
			continue
		}
		all := true
		for _, i := range members {
			if _, _, ok := clockReadingOf(b.reads[i].Shape.Leaves[p]); !ok {
				all = false
				break
			}
		}
		if all {
			out[p] = true
		}
	}
	return out
}

// split parts members (recordings of one shape) by the values of their
// leaves that are no candidate (cands), and learns each part: a part whose
// candidate leaf varies but tracks no offset is split again with that leaf
// a value.
func (b seriesBuilder) split(members []int, cands map[string]bool) []Series {
	parts := map[string][]int{}
	var order []string
	for _, i := range members {
		k := b.valuesKey(b.reads[i].Shape, cands)
		if _, ok := parts[k]; !ok {
			order = append(order, k)
		}
		parts[k] = append(parts[k], i)
	}
	var out []Series
	for _, k := range order {
		s, demote, ok := b.learn(parts[k], cands)
		if ok {
			out = append(out, s)
			continue
		}
		fewer := make(map[string]bool, len(cands)-1)
		for p := range cands {
			if p != demote {
				fewer[p] = true
			}
		}
		out = append(out, b.split(parts[k], fewer)...)
	}
	return out
}

// valuesKey is s's compared values, but for the candidates' (cands).
func (b seriesBuilder) valuesKey(s Shape, cands map[string]bool) string {
	var parts []string
	for p, l := range s.Leaves {
		if _, byValue := b.compared(l); byValue && !cands[p] {
			parts = append(parts, p+"\x00"+l.Value)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}

// learn is the series of part (recordings that share every value but the
// candidates'): each candidate leaf that holds one value is a value, and
// each that varies must track one offset. ok is false, with the first such
// leaf (by path) that tracks none, when one does not.
func (b seriesBuilder) learn(part []int, cands map[string]bool) (s Series, demote string, ok bool) {
	last := b.reads[part[len(part)-1]].Shape
	s = Series{Key: last.Key, Fixed: last.Fixed, Values: map[string]Leaf{}, Clock: map[string]ClockLeaf{}, Members: part}
	paths := make([]string, 0, len(last.Leaves))
	for p := range last.Leaves {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		l := last.Leaves[p]
		if present, _ := b.compared(l); !present {
			continue
		}
		if !cands[p] || oneValue(b.reads, part, p) {
			s.Values[p] = l
			continue
		}
		rs := make([]clockReading, len(part))
		for j, i := range part {
			at, res, _ := clockReadingOf(b.reads[i].Shape.Leaves[p])
			rs[j] = clockReading{at: at, res: res, span: b.reads[i].Span}
		}
		e, tracks := trackOffset(rs)
		if !tracks {
			return Series{}, p, false
		}
		s.Clock[p] = ClockLeaf{Leaf: l, Offset: e}
	}
	return s, "", true
}

// oneValue reports whether every recording of part holds one value at p.
func oneValue(reads []SeriesRead, part []int, p string) bool {
	v := reads[part[0]].Shape.Leaves[p].Value
	for _, i := range part[1:] {
		if reads[i].Shape.Leaves[p].Value != v {
			return false
		}
	}
	return true
}

// clockReadingOf reads l as a read of the clock: a Number that is a bare
// epoch number whole (readEpoch), or an Instant (InstantOf). ok is false
// for any other leaf, a String that reads as one among them.
func clockReadingOf(l Leaf) (time.Time, time.Duration, bool) {
	switch l.Type {
	case Number:
		return readEpoch(l.Value)
	case Instant:
		return InstantOf(l)
	}
	return time.Time{}, 0, false
}

// Holds reports whether live, a call made now (its request as the learn
// step reads it, typed), is a call of s made in span, the call's span on the
// run's app clock (Window.F): the same operation key and fixed parts, the
// same leaves, each value as s holds it, and each read of the clock of the
// same type, made in span by the offset s learned (EpochOffset). A leaf
// known excuses (by the leaf s holds, or the live one where s holds none) is
// not compared by value, and only its type where known says so (typed). A
// live call with no span reads no clock: it holds only where s has no read
// of the clock.
func (s Series) Holds(live Shape, span Span, known Known) bool {
	if known == nil {
		known = None
	}
	if live.Key != s.Key || len(live.Fixed) != len(s.Fixed) {
		return false
	}
	for n, v := range s.Fixed {
		if lv, ok := live.Fixed[n]; !ok || lv != v {
			return false
		}
	}
	held := 0
	for p, l := range live.Leaves {
		rec, inValues := s.Values[p]
		clock, inClock := s.Clock[p]
		if inClock {
			rec = clock.Leaf
		}
		if !inValues && !inClock {
			excused, typed := known.Excused(l)
			if !excused || typed {
				return false // a leaf the series does not hold
			}
			continue
		}
		held++
		excused, _ := known.Excused(rec)
		switch {
		case l.Type != rec.Type:
			return false
		case excused:
		case inValues && l.Value != rec.Value:
			return false
		case inClock:
			at, res, ok := clockReadingOf(l)
			if !ok || !clock.Offset.holds(at, res, span) {
				return false
			}
		}
	}
	if held == len(s.Values)+len(s.Clock) {
		return true
	}
	// A leaf the series holds that live does not is excused only untyped.
	for p, l := range s.Values {
		if _, ok := live.Leaves[p]; !ok && !excusedUntyped(known, l) {
			return false
		}
	}
	for p, c := range s.Clock {
		if _, ok := live.Leaves[p]; !ok && !excusedUntyped(known, c.Leaf) {
			return false
		}
	}
	return true
}

// excusedUntyped reports whether known excuses l without its type: a leaf
// that may then be missing from either side.
func excusedUntyped(known Known, l Leaf) bool {
	excused, typed := known.Excused(l)
	return excused && !typed
}
