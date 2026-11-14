package learn

import (
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
// Two of those are ids, not times, and keep the check learning gave them
// (refused, bound, or a read of the clock checked by its range), as under
// strict times:
//   - a time at a key (keyPlace): at a URL path segment or under an _id
//     key, it names what the call fetches or writes, and checked by its
//     form alone a replay would serve the recorded answer to a query for
//     another id. Its place in the call's structure says so, never its
//     text; a time a call carries in a query parameter or another body
//     field (since, expires_at, createdAt) is no key. A field a clock rule
//     declares is checked as the rule says, at a key too;
//   - a time-ordered UUID (a uuid* layout): its time is one part of the id,
//     and its form would pass any id of its version (another node's, or
//     another clock sequence's), so it has no form the default checks
//     (timeForms).
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
// bound value, which variables decide, never the time default. A value
// whose two runs write it in two forms of an instant (a zone offset in one
// run where the other wrote Z, an epoch in seconds in one and milliseconds
// in the other) is checked by its forms alone too: a replay passes a value
// of either form (models.ClockOffset's FormF), never its range. A fraction
// of a second is no part of a form (TimeForm), so a time a run wrote with
// its all-zero fraction dropped is of one form with the other. One a run
// writes in no form of an instant keeps its refusal: a time of another type
// or form is refused, as is anything that is no time.
//
// In keploy mock's client tests (T2; keploy record's server tests, T1, are
// unchanged), a value the classify path's source search found in what an
// earlier call of the test sent, inside a longer value where no variable
// binds it, is a read of the clock where, read with no source, its call's
// window holds it as one in both runs (classifyRead's ofTimePart): a
// heartbeat's epoch second that stands inside a name stamped with the test's
// clock in the same second, in both runs. Its digits matching the name is a
// coincidence of two reads in one second, which never turns the read into a
// bound value nor refuses it; a replay checks it in its call's window, as any
// read of the clock it learns. A value the test gave (a given) holds no read
// of the clock: the search reads a given's text alone. A place that binds the
// value whole keeps its bind, as below; one in an answer, a pick from what
// the application was given, keeps its reading; and keploy.yml's times
// strict, or a clock rule it declares at the field, decides the place's
// variable as before (Learned.strictRefusal, Learned.placeRefused). A real
// given that is no time is still followed whole, and a part of it refused.
//
// A time learning bound to an earlier place that holds its value (a Value,
// a variable bound whole) keeps that bind wherever the bind holds. Where it
// does not (round F's call carries another value than the place it was bound
// to; the place holds the moment in another form, which a variable cannot
// hold), two reads of the clock that fell in one second when recorded are
// no copy, and the field falls back to the time default: checked by its form
// alone (ByForm), as above, where both runs write it as a time: in one form,
// or in two, either of which a replay passes. A time here is a decoder-typed
// time in both runs, a field a clock rule declares, or a value bound to a
// place learning read as a plain read of the application's clock
// (Place.Clock): never a value's name or shape. Every other refusal of the
// bind stands.

// timeSettles are the refusals the time default settles on a time field.
var timeSettles = map[RefusalKind]bool{
	RefusalAnotherScope: true, RefusalNoClockRead: true, RefusalCannotCheck: true,
}

// epochNear is how near its call an epoch number's moment lies, in both
// runs, for the time default to check it by its form.
const epochNear = 366 * 24 * time.Hour

// keyPlace reports whether path, a leaf's path in its call, is a place
// whose value names what the call fetches or writes: a URL path segment
// (url.path.<n>), or a leaf under an _id key, at any depth (_id,
// filter._id, q._id, u.$set._id, docs.msgs.0._id, documents[0]._id,
// filter._id.$in[0], filter._id[0]). By the leaf's place in its call's
// structure alone: its keys, each after a '.' and before the next '.' or
// '[' (an index's element, "0]", is never one), never its value (a key that
// only holds the letters, last_idle_at, is no _id key).
func keyPlace(path string) bool {
	if strings.HasPrefix(path, "url.path.") {
		return true
	}
	for _, e := range strings.FieldsFunc(path, func(r rune) bool { return r == '.' || r == '[' }) {
		if e == "_id" {
			return true
		}
	}
	return false
}

// atKey reports whether path is a key (keyPlace) at which no clock rule
// keploy.yml declares (declared) names the field: a time there keeps the
// check learning gave it, never the time default's by form.
func atKey(path string, declared bool) bool {
	return keyPlace(path) && !declared
}

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
		if d.Clock.Claim != "" {
			// Its form is its claim's (claimForm).
			return d
		}
		if atKey(path, declared) {
			// A key's read keeps its range (see above).
			return d
		}
		if form, formF := dc.timeFormOf(path, c, nil); form != "" {
			off := *d.Clock
			off.Form, off.FormF = form, formF
			d.Clock = &off
		}
		return d
	}
	if d.Outcome == OutcomeValue {
		return dc.valueByForm(path, at, n, c, d, declared)
	}
	if d.Refusal == nil || c.text && c.noWord != "" {
		// A replay reads a time inside a text only as one word of it.
		if d.Refusal != nil && dc.aroundRead {
			dc.around = dc.amongBinds(path, at, n, c, d, declared)
		}
		return d
	}
	// A leaf typed Instant is decided here whole, never as a text's word,
	// and only where both runs' values read as instants (leaf).
	typed := dc.in.Rec.Leaves[path].Type == Instant
	switch k := d.Refusal.Kind; {
	case atKey(path, declared):
		return d
	case k == RefusalTwoSources && dc.readClock:
	case timeSettles[k] && (typed || declared || dc.readClock):
	default:
		return d
	}
	form, formF := dc.timeFormOf(path, c, nil)
	if form == "" {
		return d
	}
	off := models.ClockOffset{Form: form, FormF: formF}
	if c.text {
		off.Word = c.word
	}
	rule := "a time, checked by its form alone (" + off.Forms() + ", keploy.yml's times setting at its default); the clock checks would refuse it " + string(d.Refusal.Kind) + ": " + d.Refusal.Why
	nd := dc.learned(path, at, n, c.r, c.l, OutcomeClock, rule, nil)
	off.Why = nd.Line
	nd.Clock = &off
	return nd
}

// amongBinds is d, the refusal of c, a read of the application's clock its
// call's window holds at a word of a text that changed around it
// (noWordAround), as the time default checks it where the adapter puts the
// text's other bound words back first (Caps.ClockAmongBinds): a Clock
// checked by its form alone (TimeForm: the epoch's unit, or the layout) at
// the word it stands at in the recorded text (changedInstant.around), and
// nothing else. text keeps it only where every other changed word of the
// text is a Value or Fresh word (allPutBack): put back, the text around the
// read is the recording's, which the replay compares by its hashes. nil
// where the adapter does not, at a key, or where the two runs write no one
// form of an instant (timeFormOf).
func (dc *decider) amongBinds(path string, at variables.Address, n int, c changedInstant, d Decision, declared bool) *Decision {
	if !dc.in.Caps.ClockAmongBinds || c.around.IsZero() || atKey(path, declared) {
		return nil
	}
	form, formF := dc.timeFormOf(path, c, nil)
	if form == "" {
		return nil
	}
	off := models.ClockOffset{Form: form, FormF: formF, Word: c.around}
	rule := "a time, checked by its form alone (" + off.Forms() + ", keploy.yml's times setting at its default) at its word, every other changed word of the text put back at replay; the clock checks would refuse it " + string(d.Refusal.Kind) + ": " + d.Refusal.Why
	nd := dc.learned(path, at, n, c.r, c.l, OutcomeClock, rule, nil)
	off.Why = nd.Line
	nd.Clock = &off
	return &nd
}

// valueByForm is d, a Value (the changed instant c at path, bound to an
// earlier place), with its time default's check by form (Decision.ByForm)
// where its field is a time (boundTime; declared: a clock rule names it),
// read as a replay reads it (a word of a text where it stands in one), and
// both runs write it as a time (timeFormOf): in one form, or in two, either
// of which a replay passes (FormF, as a Clock's). d's outcome stands: the
// adapter takes ByForm only where it cannot hold the bind.
func (dc *decider) valueByForm(path string, at variables.Address, n int, c changedInstant, d Decision, declared bool) Decision {
	why := boundTime(path, dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path], d.From, declared)
	if why == "" || c.text && c.noWord != "" {
		return d
	}
	form, formF := dc.timeFormOf(path, c, d.From.Offset)
	if form == "" {
		return d
	}
	off := models.ClockOffset{Form: form, FormF: formF}
	if c.text {
		off.Word = c.word
	}
	off.Why = dc.learned(path, at, n, c.r, c.l, OutcomeClock, byFormRule(off.Forms(), why), nil).Line
	d.ByForm = &off
	return d
}

// boundTime is why a value learning bound to the earlier place from is a
// time the time default checks by its form where the bind does not hold
// (see above): a decoder-typed time (rec and live, its field's leaf as
// recorded and in round F, both of type Instant; the form check holds a
// replay's value to that typed form), a field a clock rule declares
// (declared), or a value bound to a place learning read as a read of the
// application's clock: a plain one (Place.Clock), or one plus a constant
// offset (Place.Offset: another test's key expiry, a year after its call,
// stamped in the second this call's was when recorded). "" for any other
// value: never by its name or its shape; and at a key (path, keyPlace) no
// clock rule declares, which keeps its bind.
func boundTime(path string, rec, live Leaf, from Place, declared bool) string {
	switch {
	case atKey(path, declared):
		return ""
	case rec.Type == Instant && live.Type == Instant:
		return "a decoder-typed time"
	case declared:
		return "a field a clock rule in keploy.yml declares"
	case from.Clock:
		return "bound to " + placeAt(from) + ", where keploy learned a read of the application's clock"
	case from.Offset != nil:
		return "bound to " + placeAt(from) + ", where keploy learned a read of the application's clock plus an offset"
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
// the time default (timeForm), and both runs write it as an instant
// (timeForms: of one form, or of two, either of which a replay passes; an
// epoch number near its call in its run, w), as Decide's valueByForm checks
// one. ln as it is otherwise: the caller takes ByForm only where round F's
// call does not carry the bound place's value.
func BoundTimeLearned(ln Learned, declared bool, w *Window, timeForm bool) Learned {
	if !timeForm || ln.Kind != KindVariable || !ln.From.Whole || ln.From.Echo {
		return ln
	}
	why := boundTime(ln.Recorded.Path, ln.Recorded, ln.Live, ln.From, declared)
	if why == "" || !isInstant(ln.Recorded) || !isInstant(ln.Live) {
		return ln
	}
	form, formF := timeForms(TimeForm(ln.Recorded), TimeForm(ln.Live), ln.Recorded, ln.Live, w, ln.From.Offset)
	if form == "" {
		return ln
	}
	off := models.ClockOffset{Form: form, FormF: formF}
	off.Why = byFormRule(off.Forms(), why)
	ln.ByForm = &off
	return ln
}

// HoldsByForm reports whether live, the value a replay's call carries at a
// use place of a variable bound whole where it does not hold the variable's
// value, passes that place's check by form (models.MockVariable.ByForm, o):
// under the time default (timeForm) alone, a whole value (no word), and a
// time of a form o checks by (TimeForm: its Form, or FormF beside it,
// WrittenAs). HTTP and MongoDB replays both check a use's fallback with it,
// so the two agree.
func HoldsByForm(o models.ClockOffset, live Leaf, timeForm bool) bool {
	return timeForm && o.Form != "" && o.Word.IsZero() && o.WrittenAs(TimeForm(live))
}

// AnsweredWhole reports whether mock, an earlier call of the test (one of
// earlier), answered r's recorded value whole at any leaf of its answer
// (WholeEcho), placed or not: a later call may have copied the value from
// that answer, which is recorded and holds in every replay, so a use of it
// keeps its exact bind and gets no check by form (models.MockVariable.ByForm).
// HTTP and MongoDB learning both ask it, so the two agree.
func AnsweredWhole(earlier []Earlier, mock string, r Leaf) bool {
	for _, e := range earlier {
		if e.Mock != mock {
			continue
		}
		for _, s := range e.Seen {
			if s.Side == Response && WholeEcho(r, s.Leaf) {
				return true
			}
		}
	}
	return false
}

// claimForm is the form of c, a read of the application's clock that is the
// value whole of one claim of the JWT at path (changedInstant.claim), that a
// replay checks it by (ClaimTimeForm) where the time default is on and the
// call's adapter checks a claim at replay (Caps.ClaimByForm): its claim's
// form in both runs, and formF the second where the two runs wrote the claim
// in two (models.FormsOf: a replay passes either), where c is a time
// (timeFormOf). "" where any of that does not hold: the read is refused as
// before.
func (dc *decider) claimForm(path string, c changedInstant) (form, formF string) {
	if !dc.in.TimeForm || !dc.in.Caps.ClaimByForm || c.claim == "" {
		return "", ""
	}
	if f, _ := dc.timeFormOf(path, c, nil); f == "" {
		return "", ""
	}
	rec, live := c.leaf()
	r, l := ClaimTimeForm(rec, c.claim), ClaimTimeForm(live, c.claim)
	if r == "" || l == "" {
		return "", ""
	}
	return models.FormsOf(r, l)
}

// ClaimTimeForm is the form (TimeForm) of the value of claim in the JWT
// text holds (a token, or "Bearer <token>"), of its JSON type: a number's a
// Number's, a string's a String's. "" where text holds no JWT, the JWT no
// such claim, or its value is no string or number keploy reads as an
// instant.
func ClaimTimeForm(text, claim string) string {
	_, claims, ok := jwtClaims(text)
	if !ok || claim == "" {
		return ""
	}
	raw, ok := claims[claim]
	if !ok {
		return ""
	}
	v, ok := variables.Decode(variables.StepJSON, string(raw))
	if !ok || v == "" {
		return ""
	}
	t := String
	if raw[0] != '"' {
		t = Number
	}
	return timeFormAs(t, Leaf{Type: t, Value: v})
}

// HoldsClaimByForm reports whether live, the value a replay's call carries
// at a place learned as a time in one claim of a JWT (o, with
// models.ClockOffset.Claim), passes that check: under the time default
// (timeForm) alone, the claim's value in live's JWT a time of a form o
// checks by (its Form, or FormF beside it: WrittenAs), of its JSON type
// (ClaimTimeForm). HTTP and MongoDB replays both check such a
// place with it, so the two agree; every other part of the token is
// checked as its own learning says.
func HoldsClaimByForm(o models.ClockOffset, live string, timeForm bool) bool {
	return timeForm && o.Form != "" && o.Claim != "" && o.Word.IsZero() && o.WrittenAs(ClaimTimeForm(live, o.Claim))
}

// timeFormOf is the form of the changed instant c, a time field at path,
// and formF the second where its two runs write two (timeForms): "" where
// a run's value is in no form keploy reads as an instant or names no
// moment, and where an epoch number's moment lies far from its call in a
// run (or, by: the offset a bound place's read of the clock stands at, far
// from both its call and its call moved by that offset: timeForms).
func (dc *decider) timeFormOf(path string, c changedInstant, by *models.ClockOffset) (form, formF string) {
	rl, ll := dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path]
	rec, live := rl, ll
	if c.text {
		// A word of the text: its form, of the leaf's own type.
		rec, live = Leaf{Type: String, Value: c.r}, Leaf{Type: String, Value: c.l}
	}
	if c.rm == nil || c.lm == nil {
		return "", ""
	}
	return timeForms(timeFormAs(rl.Type, rec), timeFormAs(ll.Type, live), rec, live, dc.in.Window, by)
}

// timeForms is the forms the time default checks a time by, of rec and live,
// its leaf as recorded and in round F, written recForm and liveForm
// (TimeForm): the one form where the two runs wrote one, else the two
// (models.FormsOf: a replay passes a value of either, FormF). "" where a
// run's leaf has no form (no time keploy reads), and where a run's epoch
// number names no moment within epochNear of its run's span of the call
// (w: Rec and F; nil knows no span), nor, where the value is bound whole to
// a place learning read as the clock plus an offset (by, Place.Offset; nil
// for none), of that span moved by the offset (nearSpan).
func timeForms(recForm, liveForm string, rec, live Leaf, w *Window, by *models.ClockOffset) (form, formF string) {
	switch {
	case recForm == "" || liveForm == "":
		return "", ""
	case uuidLeaf(rec) || uuidLeaf(live):
		// A time-ordered UUID is an id (see above): no form of it is
		// checked alone.
		return "", ""
	case !epochNearIn(rec, w, false, by) || !epochNearIn(live, w, true, by):
		return "", ""
	}
	return models.FormsOf(recForm, liveForm)
}

// uuidLeaf reports whether l, a run's leaf (or a word of one), is a
// time-ordered UUID (a uuid* layout: clockTextLayout).
func uuidLeaf(l Leaf) bool { return strings.HasPrefix(leafLayout(l), "uuid") }

// epochNearIn reports whether l, a run's leaf (round F's: f), is no epoch
// number, or one that names a moment near that run's span of the call in w
// (nearSpan, by its offset by); false for an epoch number where w is nil.
func epochNearIn(l Leaf, w *Window, f bool, by *models.ClockOffset) bool {
	if !strings.HasPrefix(leafLayout(l), "epoch-") {
		return true
	}
	if w == nil {
		return false
	}
	if f {
		return nearSpan(l, w.F, by)
	}
	return nearSpan(l, w.Rec, by)
}

// nearSpan reports whether l, an epoch number, names a moment within
// epochNear of s, its run's span of the call, or of s moved by by, the
// offset learning read a bound place's value at from the clock
// (Place.Offset: an expiry two years after its call lies near that call
// moved by two years, not near the call); false where s is unknown. by is
// nil for no such place. The offset is the learn step's own, never one
// read from the field's name.
func nearSpan(l Leaf, s Span, by *models.ClockOffset) bool {
	at, _, _, ok := clockReadOf(l)
	if !ok || s.From.IsZero() {
		return false
	}
	near := func(from, to time.Time) bool { return !at.Before(from.Add(-epochNear)) && !at.After(to.Add(epochNear)) }
	return near(s.From, s.To) || by != nil && near(s.From.Add(by.Min), s.To.Add(by.Max))
}

// TimeForm is the form the time default checks a time by
// (models.ClockOffset.Form): its leaf's type, then its text with every
// digit written 9 and every letter a, so that a form keeps an epoch's unit
// (its count of digits), a text's field order, separators and zone form
// ("Z", "+99:99", a zone's name). A fraction of a second is no part of it:
// a Go application's RFC 3339 drops a fraction's trailing zeros, and the
// whole fraction where every digit is 0, so one application writes a time
// with a fraction or without as its moment falls (a time read at the
// millisecond is whole one time in a thousand). A UUID's time is its
// layout ("uuid1"), which no learning checks a value by alone (timeForms:
// a UUID is an id). "" for a leaf keploy reads as no instant and no time
// of day.
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

// timeSkeleton is s, an instant or a time of day keploy reads
// (clockLayouts), with every digit written 9, every ASCII letter a, and a
// fraction of a second, a '.' or ',' and the run of digits after it, left
// out. In every such layout a '.' or ',' before a digit begins a fraction:
// of an ISO time's or a time of day's seconds, or of an epoch number; an
// RFC 1123 date's comma has a space after it.
func timeSkeleton(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case (ch == '.' || ch == ',') && i+1 < len(s) && isDigit(s[i+1]):
			for i+1 < len(s) && isDigit(s[i+1]) {
				i++
			}
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
