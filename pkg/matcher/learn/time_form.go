package learn

import (
	"cmp"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// The time default (Input.TimeForm; keploy.yml's times setting at its
// default). For T2 a test's assertions are the e2e test's own, and
// keploy's check of a time is a bonus over them: a changed time is no
// reason to set a test aside. A time field passes at replay with any value
// of the type and form it was recorded in (models.ClockOffset.Form), with
// no range, cut, zone, clock-read provenance or time-of-day echo checked.
//
// A time field is a changed instant (decider.instant) that is one of:
//   - a decoder-typed time: a leaf of type Instant in both runs (a BSON
//     datetime or timestamp, a typed Postgres column);
//   - a field a declared clock rule names (declaredClock ran first);
//   - a value learning read as the application's clock: a Clock it
//     learned; a read it refused as one it cannot check (cannot-check: an
//     offset the replay does not compare, or a read the call's own answer
//     holds); or a moment sent at two places, each its own call's read of
//     the clock (two-sources).
//
// A field is never made a time by its name or its value's shape: a text
// in a clock layout, or an epoch number, that is no decoder-typed time, no
// declared clock and no read learning found stays as learned, word by word
// (the founder's 10-23 guardrail). The layout and the epoch's nearness to
// its call (epochNear) are checks on a time field only: its form must read
// as an instant in both runs, and an epoch number's moment lie near its call
// in both.
//
// On a time field the time default settles the refusals the clock checks
// give of which read, or which scope, a moment is (timeSettles); a two-sources
// refusal only where both sources are reads of the clock. A Value, a Fresh
// value and every other refusal stay as learned: a moment an earlier answer
// or request also holds (pick, unknown, two sources that are no reads) is a
// bound value, which variables decide, never the time default. A value whose
// two runs write it in two forms, or in no form keploy reads, keeps its
// refusal: a time of another type or form is refused, as is anything that
// is no time.
//
// A time learning bound to an earlier place that holds its value (a Value,
// a variable bound whole) keeps that bind wherever the bind holds. Where it
// does not (round F's call carries another value than the place it was bound
// to; the place holds the moment in another form, which a variable cannot
// hold), two reads of the clock that fell in one second when recorded are
// no copy, and the field falls back to the time default: checked by its form
// alone (ByForm), as above. A time here is a decoder-typed time in both runs,
// a field a clock rule declares, or a value bound to a place learning read
// as a plain read of the application's clock (Place.Clock): never a value's
// name or shape. Every other refusal of the bind stands.

// timeSettles are the refusals the time default settles on a time field.
var timeSettles = map[RefusalKind]bool{
	RefusalAnotherScope: true, RefusalNoClockRead: true, RefusalCannotCheck: true,
}

// epochNear is how near its call an epoch number's moment lies, in both
// runs, for the time default to check it by its form.
const epochNear = 366 * 24 * time.Hour

// heldRead is d, a refusal of a value learning read as the application's
// clock (see above), marked so for timeDefault.
func (dc *decider) heldRead(d Decision) Decision {
	dc.readClock = true
	return d
}

// timeDefault is d, the decision of the changed instant c at path (declared:
// a clock rule names it), under the time default (see above); d itself
// where Input.TimeForm is off.
func (dc *decider) timeDefault(path string, at variables.Address, n int, c changedInstant, d Decision, declared bool) Decision {
	if !dc.in.TimeForm {
		return d
	}
	if d.Outcome == OutcomeClock && d.Clock != nil {
		if form := dc.timeFormOf(path, c); form != "" {
			off := *d.Clock
			off.Form = form
			d.Clock = &off
		}
		return d
	}
	if d.Outcome == OutcomeValue {
		return dc.valueByForm(path, at, n, c, d, declared)
	}
	if d.Refusal == nil || c.text && c.noWord != "" {
		// A replay reads a time inside a text only as one word of it.
		return d
	}
	// A leaf typed Instant is decided here whole, never as a text's word,
	// and only where both runs' values read as instants (leaf).
	typed := dc.in.Rec.Leaves[path].Type == Instant
	switch k := d.Refusal.Kind; {
	case k == RefusalTwoSources && dc.readClock:
	case timeSettles[k] && (typed || declared || dc.readClock):
	default:
		return d
	}
	form := dc.timeFormOf(path, c)
	if form == "" {
		return d
	}
	off := models.ClockOffset{Form: form}
	if c.text {
		off.Word = c.word
	}
	rule := "a time, checked by its form alone (" + form + ", keploy.yml's times setting at its default); the clock checks would refuse it " + string(d.Refusal.Kind) + ": " + d.Refusal.Why
	nd := dc.learned(path, at, n, c.r, c.l, OutcomeClock, rule, nil)
	off.Why = nd.Line
	nd.Clock = &off
	return nd
}

// valueByForm is d, a Value (the changed instant c at path, bound to an
// earlier place), with its time default's check by form (Decision.ByForm)
// where its field is a time (boundTime; declared: a clock rule names it),
// read as a replay reads it (a word of a text where it stands in one), and
// both runs write it in one form (timeFormOf). d's outcome stands: the
// adapter takes ByForm only where it cannot hold the bind.
func (dc *decider) valueByForm(path string, at variables.Address, n int, c changedInstant, d Decision, declared bool) Decision {
	why := boundTime(dc.in.Rec.Leaves[path], d.From, declared)
	if why == "" || c.text && c.noWord != "" {
		return d
	}
	form := dc.timeFormOf(path, c)
	if form == "" {
		return d
	}
	off := models.ClockOffset{Form: form}
	if c.text {
		off.Word = c.word
	}
	off.Why = dc.learned(path, at, n, c.r, c.l, OutcomeClock, byFormRule(form, why), nil).Line
	d.ByForm = &off
	return d
}

// boundTime is why a value learning bound to the earlier place from is a
// time the time default checks by its form where the bind does not hold
// (see above): a decoder-typed time (rec, its field's recorded leaf; the
// form check holds round F's value to the same typed form), a field a clock
// rule declares (declared), or a value bound to a place learning read as a
// plain read of the application's clock (Place.Clock). "" for any other
// value: never by its name or its shape.
func boundTime(rec Leaf, from Place, declared bool) string {
	switch {
	case rec.Type == Instant:
		return "a decoder-typed time"
	case declared:
		return "a field a clock rule in keploy.yml declares"
	case from.Clock:
		return "bound to " + placeAt(from) + ", where keploy learned a read of the application's clock"
	}
	return ""
}

// byFormRule is the rule a time field (why: boundTime) checked by its form
// alone (form) is learned by where the bind keploy found does not hold.
func byFormRule(form, why string) string {
	return "a time (" + why + "), checked by its form alone (" + form + ", keploy.yml's times setting at its default) where the value it was bound to does not hold"
}

// BoundTimeLearned is ln, a drifted leaf the classify path learned (the HTTP
// adapter's learn step), with its time default's check by form
// (Learned.ByForm) where it is a variable bound whole whose field is a time
// (boundTime; declared: a clock rule keploy.yml declares names it), under
// the time default (timeForm), and both runs write it as an instant of one
// form, an epoch number near its call in both runs (w), as
// DeclaredClockLearned checks a declared time by its form. ln as it is
// otherwise: the caller takes ByForm only where round F's call does not carry
// the bound place's value.
func BoundTimeLearned(ln Learned, declared bool, w *Window, timeForm bool) Learned {
	if !timeForm || ln.Kind != KindVariable || !ln.From.Whole || ln.From.Echo {
		return ln
	}
	why := boundTime(ln.Recorded, ln.From, declared)
	if why == "" {
		return ln
	}
	form, no := formIn(ln.Recorded, ln.Live, w)
	if no != "" {
		return ln
	}
	ln.ByForm = &models.ClockOffset{Form: form, Why: byFormRule(form, why)}
	return ln
}

// HoldsByForm reports whether live, the value a replay's call carries at a
// use place of a variable bound whole where it does not hold the variable's
// value, passes that place's check by form (models.MockVariable.ByForm, o):
// under the time default (timeForm) alone, a whole value (no word), and a
// time of o's form (TimeForm). HTTP and MongoDB replays both check a use's
// fallback with it, so the two agree.
func HoldsByForm(o models.ClockOffset, live Leaf, timeForm bool) bool {
	return timeForm && o.Form != "" && o.Word.IsZero() && TimeForm(live) == o.Form
}

// formIn is the form rec and live, a time field's leaf as recorded and in
// round F, are both written in (TimeForm), as the time default checks it;
// else "" and why not, after "; and": its runs write no one form of an
// instant, or, an epoch number, it names no moment near its call in both
// runs (nearSpan, in w's spans).
func formIn(rec, live Leaf, w *Window) (form, no string) {
	form = TimeForm(rec)
	switch {
	case TimeForm(live) != form || !isInstant(rec) || !isInstant(live):
		return "", "; and its two runs write no one form of an instant (" + cmp.Or(form, "none") + " recorded, " + cmp.Or(TimeForm(live), "none") + " in round F)"
	case strings.HasPrefix(leafLayout(rec), "epoch-") && (w == nil || !nearSpan(rec, w.Rec) || !nearSpan(live, w.F)):
		return "", "; and, an epoch number, it names no moment near its call in both runs"
	}
	return form, ""
}

// timeFormOf is the form of the changed instant c, a time field at path, in
// both runs: "" where its runs write two forms or one keploy reads as no
// instant, where a run's value names no moment, and where an epoch number's
// moment lies far from its call in a run (nearCall).
func (dc *decider) timeFormOf(path string, c changedInstant) string {
	rl, ll := dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path]
	rec, live := rl, ll
	if c.text {
		// A word of the text: its form, of the leaf's own type.
		rec, live = Leaf{Type: String, Value: c.r}, Leaf{Type: String, Value: c.l}
	}
	form := timeFormAs(rl.Type, rec)
	switch {
	case form == "" || timeFormAs(ll.Type, live) != form:
		return ""
	case c.rm == nil || c.lm == nil:
		return ""
	case strings.HasPrefix(leafLayout(rec), "epoch-") && !dc.nearCall(rec, live):
		return ""
	}
	return form
}

// nearCall reports whether rec and live, an epoch number as recorded and in
// round F, each name a moment within epochNear of its run's span of the
// call; false where keploy knows no span.
func (dc *decider) nearCall(rec, live Leaf) bool {
	w := dc.in.Window
	if w == nil {
		return false
	}
	return nearSpan(rec, w.Rec) && nearSpan(live, w.F)
}

// nearSpan reports whether l, an epoch number, names a moment within
// epochNear of s, its run's span of the call; false where s is unknown.
func nearSpan(l Leaf, s Span) bool {
	at, _, _, ok := clockReadOf(l)
	return ok && !s.From.IsZero() && !at.Before(s.From.Add(-epochNear)) && !at.After(s.To.Add(epochNear))
}

// TimeForm is the form the time default checks a time by
// (models.ClockOffset.Form): its leaf's type, then its text with every
// digit written 9 and every letter a, so that a form keeps an epoch's unit
// (its count of digits), a text's field order, separators and zone form
// ("Z", "+99:99", a zone's name), and whether it writes a fraction of a
// second, written f whatever its digits (a Go application's RFC 3339 drops
// a fraction's trailing zeros). A UUID's time is its layout ("uuid1").
// "" for a leaf keploy reads as no instant and no time of day.
func TimeForm(l Leaf) string { return timeFormAs(l.Type, l) }

// timeFormAs is TimeForm of l, a word of a leaf of type t, read as t.
func timeFormAs(t Type, l Leaf) string {
	_, _, _, inst := clockReadOf(l)
	_, _, _, tod := timeOfDayOf(l)
	if !inst && !tod {
		return ""
	}
	kind := "text"
	switch t {
	case Instant:
		kind = "typed"
	case Number:
		kind = "number"
	}
	if lay := leafLayout(l); strings.HasPrefix(lay, "uuid") {
		return kind + " " + lay
	}
	return kind + " " + timeSkeleton(l.Value)
}

// timeSkeleton is s with every digit written 9, every ASCII letter a, and
// a run of digits after a '.' or ',' that follows a digit written f.
func timeSkeleton(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case isDigit(ch) && i > 1 && (s[i-1] == '.' || s[i-1] == ',') && isDigit(s[i-2]):
			for i+1 < len(s) && isDigit(s[i+1]) {
				i++
			}
			b = append(b, 'f')
		case isDigit(ch):
			b = append(b, '9')
		case ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z':
			b = append(b, 'a')
		default:
			b = append(b, ch)
		}
	}
	return string(b)
}

// formWhy is how a refusal names f, the form of the value a replay's call
// carries at a place learned as a time (TimeForm).
func formWhy(f string) string {
	if f == "" {
		return "no instant keploy reads"
	}
	return "written as " + f + ", another type or form"
}
