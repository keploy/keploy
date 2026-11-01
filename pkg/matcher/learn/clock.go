package learn

import (
	"encoding/hex"
	"regexp"
	"regexp/syntax"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

// FullRoundLead is how far past each recorded instant record's full round F
// runs every test (design v14 §3.4): second- and minute-resolution values the
// app reads from its clock drift there, by this much, and are learned. It
// moves a value's minute, second and millisecond, and it is a whole number of
// no step a cut is read at above a millisecond (models.CutOf): 5 ms past a
// whole 10 ms, 45 ms past a whole 100 ms, 545 ms past a whole second, 1.545 s
// past a whole minute. Let r be the resolution a field's values carry: the
// clock's or the writer's, the coarser. Two values of a writer that does not
// cut both fall on a step only where F's read moved, against the
// recording's and its call, by more than: 5 ms less r either way (a whole
// 10 ms); 45 ms back or 55 ms on, less r (100 ms); 455 ms on or 545 ms back,
// less r (a second: 355 ms on or 445 ms back for a writer of tenths);
// 1.545 s back or 58.455 s on, less r (a minute: 545 ms back for a writer
// of seconds). So the learn step, which learns a cut both values show
// (readRes) and checks a replay value at it (CheckClockRead), never learns
// the pair of a fast call at a cut by chance, which would loosen its check:
// one whose read in F lies within 5 ms less r of the recording's (4 ms on a
// clock of milliseconds, 4.99 ms for five fraction digits, 4.999 ms for six
// or on a clock of microseconds; a writer of 10 ms or coarser has no step nearer
// than 355 ms). A lead that is a whole number of a step (61.5 s, of 10 and
// 100 ms) kept a millisecond clock's value on a whole 10 ms on one in F:
// one recording in twelve with F's read within 0.3 ms of the recording's,
// one in 80 within 4 ms.
const FullRoundLead = 61545 * time.Millisecond

// clockSet is what of a drifted value (recorded, live) is a clock read that
// moved by FullRoundLead: the ASCII words of such reads, recorded (rec) and
// live (live), whether every word of the value that changed is one (whole),
// and each such read as recorded, in the order they stand in it (reads).
type clockSet struct {
	rec, live map[string]bool
	whole     bool
	reads     []models.ClockRead
}

// A clock layout is a way a value can carry an instant, as a pattern of the
// recorded text and how to read it. timeWord, when not nil, says which words
// of a match hold the instant (a UUID's time groups); every word of the match
// does otherwise.
type clockLayout struct {
	re       *regexp.Regexp
	read     func(s string) (t time.Time, res time.Duration, ok bool)
	timeWord func(match string, i int) bool
	// wraps: the instant is a time of day, compared modulo a day.
	wraps bool
}

// Every layout reads its text in UTC where the text names no offset, and an
// abbreviated zone ("GMT", "UTC") at its own offset, never at the host's
// (time.ParseInLocation in UTC): what an instant is, and the time of day its
// text writes (instant.wall), are the text's, whatever zone keploy runs in.
var clockLayouts = []clockLayout{
	{re: regexp.MustCompile(`(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun), \d{2} (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4} \d{2}:\d{2}:\d{2} (?:GMT|UTC|[+-]\d{4})`), read: readRFC1123},
	// ISO 8601 at the day, the hour ("2026-10-22T10Z", its reduced
	// precision, after a T only), the minute, the second and a fraction.
	{re: regexp.MustCompile(`\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:[.,]\d{1,9})?)?|T\d{2})?(?:Z|[+-]\d{2}(?::?\d{2})?| ?(?:UTC|GMT))?`), read: readISO},
	{re: regexp.MustCompile(`\d{8}T\d{6}Z?`), read: readCompact},
	{re: regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[17][0-9a-fA-F]{3}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), read: readUUIDTime, timeWord: uuidTimeWord},
	{re: regexp.MustCompile(`\d{10}\.\d{1,9}|\d{19}|\d{16}|\d{13}|\d{10}`), read: readEpoch},
	{re: regexp.MustCompile(`\d{2}:\d{2}(?::\d{2}(?:\.\d{1,9})?)?`), read: readTimeOfDay, wraps: true},
}

// clockWordMax is the most bytes an instant of any layout spans
// (maxMatchLen), or -1 where one layout's has no bound; clockAnchored is
// each layout's pattern matched at the start of a text alone.
var (
	clockWordMax  = clockLayoutsMax()
	clockAnchored = func() []*regexp.Regexp {
		out := make([]*regexp.Regexp, len(clockLayouts))
		for k, lay := range clockLayouts {
			out[k] = regexp.MustCompile(`^(?:` + lay.re.String() + `)`)
		}
		return out
	}()
)

func clockLayoutsMax() int {
	n := 0
	for _, lay := range clockLayouts {
		re, err := syntax.Parse(lay.re.String(), syntax.Perl)
		if err != nil {
			return -1
		}
		m, ok := maxMatchLen(re)
		if !ok {
			return -1
		}
		n = max(n, m)
	}
	return n
}

// maxMatchLen is the most bytes a text that re matches can span, at least
// as many as it can; ok is false where they have no bound (a star or a
// plus). A rune is one byte where it is ASCII and re folds no case, and
// utf8.UTFMax otherwise.
func maxMatchLen(re *syntax.Regexp) (n int, ok bool) {
	runeLen := func(r rune) int {
		if r < utf8.RuneSelf && re.Flags&syntax.FoldCase == 0 {
			return 1
		}
		return utf8.UTFMax
	}
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpNoMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return 0, true
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			n += runeLen(r)
		}
		return n, true
	case syntax.OpCharClass:
		for _, r := range re.Rune {
			n = max(n, runeLen(r))
		}
		return n, true
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return utf8.UTFMax, true
	case syntax.OpCapture, syntax.OpQuest:
		return maxMatchLen(re.Sub[0])
	case syntax.OpRepeat:
		if re.Max < 0 {
			return 0, false
		}
		n, ok = maxMatchLen(re.Sub[0])
		return n * re.Max, ok
	case syntax.OpConcat, syntax.OpAlternate:
		for _, sub := range re.Sub {
			m, ok := maxMatchLen(sub)
			if !ok {
				return 0, false
			}
			if re.Op == syntax.OpConcat {
				n += m
			} else {
				n = max(n, m)
			}
		}
		return n, true
	}
	return 0, false
}

// ClockOf reports whether a value drifted only with F's clock: every word
// that changed between its recording (recorded) and record's full round
// (live) is a part of a clock read that moved by FullRoundLead, and one did.
// It is the evidence Classify reads (clockOf), read at learn time: live is
// the value F saw, its clock set past each recorded instant by the lead. An
// adapter asks it, and has no rule of its own of what a time looks like: a
// value that only looks like one ("2025551234") and did not move by the
// lead is no clock read.
func ClockOf(recorded, live string) bool {
	return clockOf(recorded, live).whole
}

// ClockWordsOf is that evidence word by word: the ASCII words that changed
// between a value's recording (recorded) and record's full round (live) and
// are parts of a clock read that moved by FullRoundLead, those it lost (of
// the recorded value), then those it gained (of the live one), each in the
// order it stands in its value, as often as it stands there; nil where none
// did. A value that holds
// such a read beside words that changed otherwise (an event id built on the
// time and a random suffix, a UUID v7's random groups) has some, though
// ClockOf is false for it; one whose changed words are no such read (a
// counter, a value moved by another amount) has none. Its words need not
// line up between the two values, and no count of them bounds the search. An
// adapter that refuses a value holding a read of the clock, whole or beside
// other words, asks it, and needs no clock search of its own. Like ClockOf,
// it reads the lead alone, never a call's window.
func ClockWordsOf(recorded, live string) []string {
	c := clockOf(recorded, live)
	r, l := tokensBy(recorded, isWord), tokensBy(live, isWord)
	var out []string
	for _, side := range []struct {
		text         string
		clock, other map[string]bool
	}{{recorded, c.rec, l}, {live, c.live, r}} {
		for _, w := range wordsBy(side.text, isWord) {
			if side.clock[w] && !side.other[w] {
				out = append(out, w)
			}
		}
	}
	return out
}

// ClockReads is that evidence as the learn step records it on a variable it
// binds (models.MockVariable.Clock): for a value ClockOf says drifted only
// with F's clock, each clock read it held when recorded that moved by the
// lead, the instant and resolution its recorded text says, in the order they
// stand in it; nil for any other value. A replay lets two variables recorded
// with two values share one live value only when both hold reads made in
// one second (models.ReadInOneSecond).
func ClockReads(recorded, live string) []models.ClockRead {
	c := clockOf(recorded, live)
	if !c.whole {
		return nil
	}
	return c.reads
}

// Window is when one call's clock reads can have been made, in each run
// (Tracker.CallWindow): in the recording (Rec) and in record's full round
// (F), each on that run's app clock, after the run's previous data-plane call
// reached keploy and no later than this call did (in the recording, the
// previous call is one answered before this call was sent: PreviousCall).
// A value is in a request
// only once the app read it, so a read of the clock is no later than the
// call that carries it; one made after the previous call is no earlier than
// that. An instant that falls in its run's span in both runs moved with the
// clock between them by the call's own lead (the lead's difference to the
// value's is how much longer the app took, in one run than in the other,
// from the read to the call, each at least nothing), however far F's clock
// ran ahead while the app booted: a constant does not move, nor does an
// instant an answer the replay serves from its recording carries, so
// neither falls in both. An app may still pick another of a served answer's
// instants by its clock (a timetable's next departure), which can fall in
// both: an earlier place then holds the moment F saw (a payload, or a leaf of
// an exchange the learn step served, as recorded or as F saw it), and the
// classifier follows the value as a variable of the place holding its
// recorded moment, or refuses it, before any window or lead excuses it
// (instants.go, echoedIn). A read made before the previous call falls in
// neither. It is no tolerance: the spans are what each run's calls show.
type Window struct {
	Rec, F Span
	// offsets: the caller's replay compares a read of the clock plus a
	// constant offset (ComparesOffsets).
	offsets bool
	// begun: w is a call's window read from each run's test begin
	// (sinceBegin), the reading in which the word rule lets go every word
	// of a text's runs (valueDrift.clockChanged), since only sinceWord
	// takes what it learns there as noise, and only as a checked read of
	// one word.
	begun bool
}

// ComparesOffsets is w for a caller whose replay compares a value the learn
// step reads as a read of the clock plus a constant offset
// (KindClockOffset), as models.ClockOffset.Holds does, against the value's
// own call's window: the learn step then learns such an instant, which it
// otherwise refuses naming its offset. nil for a nil w (no window: no
// offset is shown).
func (w *Window) ComparesOffsets() *Window {
	if w == nil {
		return nil
	}
	c := *w
	c.offsets = true
	return &c
}

// Span is one run's part of a Window: From, when its previous data-plane
// call reached keploy, and To, when the call itself did, on its app clock.
// Zone is the zone the application writes a wall clock of no zone in
// (Tracker.SetAppZone), as a replay's span carries it (Tracker.LiveSpan),
// which a replay reads a learned read of the clock in the application's
// zone in (models.ZoneApp, and models.ZoneEither as one of its two
// readings: CheckClockRead); nil where the tracker was told none: such a
// read is then refused. A Window's spans carry none: the learn
// step reads the zone of its DriftCache.
//
// Began is where the running test began on the run's clock, in a client
// test (SetClientTests) where the app's clock ran as one from there to the
// call (zero otherwise, and in keploy test). Where the hooks tell where the
// app's clock's run to the call began (SetAppClockRun) after the test's
// begin, Began is that instead: two reads of the clock at the begin and at
// the call agree across a stretch it left and came back to, so only the
// run tells the app's clock ran as one. A read made before the call's
// window and after the test began is a read of the clock all the same (an
// application that reads its clock once for a request and writes it at a
// later call), which the learn step learns only where both runs' spans from
// their test's begin hold it (Window.sinceBegin), and a replay checks from
// its own test's begin (models.SinceTestBegin), never from an earlier start.
//
// TestFrom is where the running test began on the app's clock, or where the
// app's clock's run to the call began after it (SetAppClockRun), in a
// replay's span (LiveSpan) whose From a serve made after the test began
// set: zero otherwise, and in a Window's spans. Another goroutine of the
// application (a queue's consumer beside the test's handler) can make a
// call keploy serves between a read of the clock and the call that carries
// the read, so a serve made after the test began bounds no read of the
// call: a replay passes a value that holds in the span from TestFrom too
// (CheckClockRead), and so never refuses a read made in the test because
// of when keploy served another call.
type Span struct {
	From, To time.Time
	Zone     *time.Location
	// Own: Zone was read from the process that made the call (ProcZone,
	// ClockSpanFrom): the zone the application writes its clock in in this
	// run, in which a replay checks a learned read whatever zone the learn
	// step named (CheckClockRead). false where Zone is the agent's own.
	Own      bool
	Began    time.Time
	TestFrom time.Time
}

// sinceBegin is s from the running test's begin (Began) to its call: false
// where s knows no begin. A begin is never after its call (the tracker sets
// none there: CallWindowFrom, liveClock.span).
func (s Span) sinceBegin() (Span, bool) {
	if s.Began.IsZero() {
		return Span{}, false
	}
	return Span{From: s.Began, To: s.To, Zone: s.Zone, Own: s.Own}, true
}

// sinceBegin is w, the window of a client test's call, from each run's test
// begin to the call (Span.sinceBegin): the window in which the learn step
// reads a read of the clock made before the call's window, earlier in the
// test. nil where either run knows no begin (keploy test's calls: none).
func (w *Window) sinceBegin() *Window {
	if w == nil {
		return nil
	}
	rec, ok := w.Rec.sinceBegin()
	if !ok {
		return nil
	}
	f, ok := w.F.sinceBegin()
	if !ok {
		return nil
	}
	return &Window{Rec: rec, F: f, offsets: w.offsets, begun: true}
}

// clockWordIn finds the one instant that changed in a text, recorded as rec
// and seen in record's full round as live, read in w in both runs: the word
// rw of rec and lw of live, found as clockAt finds them (the first layout
// of clockLayouts that matches them, at ASCII word boundaries), each a read
// of its run's clock in w (Window.holds), with the text around it the same
// in both runs, before it and after it. ok is false where no instant
// changed, where more than one did, where the one that did is no read in w,
// or where the text around it changed too: then no one word of the text is
// the read, and the text is no read of the clock plus a text that stayed.
//
// It reads the layouts over the bytes that differ and a bounded
// neighbourhood of them, never over a whole text, and its outcome is the
// one a reading of both whole texts gives (clockWordWindow): its cost is
// that of finding where two texts differ, whatever their length.
func clockWordIn(rec, live string, w *Window) (before, rw, lw, after string, ok bool) {
	if w == nil || rec == live {
		return "", "", "", "", false
	}
	q, er, el, near := clockWordWindow(rec, live)
	if !near {
		return "", "", "", "", false
	}
	takenR, takenL := make([]bool, er-q), make([]bool, el-q)
	for _, lay := range clockLayouts {
		rm, lm := clockMatches(lay, rec, q, er, takenR), clockMatches(lay, live, q, el, takenL)
		for j := 0; j < len(rm) && j < len(lm); j++ {
			rs, ls := rec[rm[j][0]:rm[j][1]], live[lm[j][0]:lm[j][1]]
			if rs == ls {
				continue
			}
			for i := rm[j][0]; i < rm[j][1]; i++ {
				takenR[i-q] = true
			}
			for i := lm[j][0]; i < lm[j][1]; i++ {
				takenL[i-q] = true
			}
			if ok {
				// A second instant that changed.
				return "", "", "", "", false
			}
			before, rw, lw, after = rec[:rm[j][0]], rs, ls, rec[rm[j][1]:]
			if live[:lm[j][0]] != before || live[lm[j][1]:] != after || !w.holds(lay, rs, ls) {
				return "", "", "", "", false
			}
			ok = true
		}
		// One text's window holds more of the layout's instants than the
		// other's: a reading of the whole texts pairs its last ones with
		// the instants after the window, the same in both texts, as it
		// pairs those with each other. Any such pair that differs refuses
		// the text (the text before its later instant can never be the
		// other's), and pairs that are the same are passed over.
		var extra [][]int
		s := rec
		if len(rm) > len(lm) {
			extra = rm[len(lm):]
		} else if len(lm) > len(rm) {
			extra, s = lm[len(rm):], live
		}
		if len(extra) > 0 && !clockPairsAfter(lay, rec, er, s, extra) {
			return "", "", "", "", false
		}
	}
	return before, rw, lw, after, ok
}

// clockWordWindow is the part of rec and of live that clockWordIn reads,
// rec[q:er] and live[q:el]: the bytes where the two texts differ, and on
// each side of them a seam no layout's instant runs across, so a reading of
// a text's part finds the instants a reading of the whole text finds there
// (clockSeamBefore, clockSeamAfter), with the instants before q the same in
// both texts, as are those from er in rec and el in live. ok is false where
// a differing part spans more bytes than any layout's instant can
// (clockWordMax): no one instant changed there. Where the layouts' instants
// have no bound, where no seam lies near, or where the side is short
// (clockSeamMin), the part is the whole text on that side.
func clockWordWindow(rec, live string) (q, er, el int, ok bool) {
	if clockWordMax < 0 {
		return 0, len(rec), len(live), true
	}
	p, s := commonEnds(rec, live)
	if len(rec)-p-s > clockWordMax || len(live)-p-s > clockWordMax {
		return 0, 0, 0, false
	}
	// A seam's own reading reads up to clockWordMax bytes past each place
	// before it: it is read where both texts are the same. A side shorter
	// than clockSeamMin past that margin is read whole: its seam would
	// cost more to find than the bytes it leaves out, and the whole side
	// is what a reading of the whole texts reads.
	q, er = 0, len(rec)
	if p-clockWordMax >= clockSeamMin {
		q = clockSeamBefore(rec, p-clockWordMax)
	}
	if s-clockWordMax >= clockSeamMin {
		er = clockSeamAfter(rec, len(rec)-s+clockWordMax)
	}
	return q, er, er + len(live) - len(rec), true
}

// commonEnds is how many bytes a and b share at their start (p), and then
// at their end (s), the two never overlapping in either.
func commonEnds(a, b string) (p, s int) {
	n := min(len(a), len(b))
	for p+64 <= n && a[p:p+64] == b[p:p+64] {
		p += 64
	}
	for p < n && a[p] == b[p] {
		p++
	}
	n -= p
	for s+64 <= n && a[len(a)-s-64:len(a)-s] == b[len(b)-s-64:len(b)-s] {
		s += 64
	}
	for s < n && a[len(a)-s-1] == b[len(b)-s-1] {
		s++
	}
	return p, s
}

// clockCutSearch is how far clockSeamBefore and clockSeamAfter look for a
// seam before they take the text's own end as one; clockSeamMin is how
// long a side of the two texts' differing part must be, past the margin a
// seam is read in, for clockWordWindow to look for a seam on it at all.
const (
	clockCutSearch = 256
	clockSeamMin   = 32
)

// clockSeamBefore is the last seam of s at or before hi, and at most
// clockCutSearch before it: a place q no instant of any layout a reading of
// s finds starts before and ends after (clockReach). 0, s's start, where
// there is none. A reading of s from q then finds what one of the whole of
// s finds from there: no layout anchors, so where a match starts, and where
// it ends, are read from the bytes it can span alone.
func clockSeamBefore(s string, hi int) int {
	for q := hi; q > 0 && hi-q <= clockCutSearch; {
		a := q - 1
		for a >= 0 && a >= q-clockWordMax && clockReach(s, a) <= q {
			a--
		}
		if a < 0 || a < q-clockWordMax {
			return q
		}
		// An instant from a runs across q, and so across every place
		// after a.
		q = a
	}
	return 0
}

// clockSeamAfter is the first seam of s at or after lo, and at most
// clockCutSearch after it (clockSeamBefore); len(s), s's end, where there
// is none.
func clockSeamAfter(s string, lo int) int {
	if lo >= len(s) {
		return len(s)
	}
	e := lo
	for a := max(0, lo-clockWordMax); a < e; a++ {
		if r := clockReach(s, a); r > e {
			if r-lo > clockCutSearch {
				return len(s)
			}
			e = r
		}
	}
	return e
}

// clockReach is where the instant that a reading of s finds starting at a
// ends, the farthest of every layout's; a where none starts there. A match
// spans at most clockWordMax bytes, so it is read from those alone: a
// match a regexp prefers there is one in them.
func clockReach(s string, a int) int {
	end := a
	t := s[a:min(len(s), a+clockWordMax)]
	for _, re := range clockAnchored {
		if m := re.FindStringIndex(t); m != nil && a+m[1] > end {
			end = a + m[1]
		}
	}
	return end
}

// clockPairsAfter reports whether every pair a reading of the whole texts
// makes after clockWordIn's window, of one layout, is of one instant: the
// layout's instants of s's window left over (extra, of s, rec or live),
// then those of rec from er, paired in order with those of rec from er (the
// same as live's from its window's end).
func clockPairsAfter(lay clockLayout, rec string, er int, s string, extra [][]int) bool {
	waiting := make([]string, 0, len(extra))
	for _, m := range extra {
		waiting = append(waiting, s[m[0]:m[1]])
	}
	for at := er; at < len(rec); {
		m := lay.re.FindStringIndex(rec[at:])
		if m == nil {
			return true
		}
		a, b := at+m[0], at+m[1]
		at = b
		if !atWordBounds(rec, a, b) {
			continue
		}
		if waiting[0] != rec[a:b] {
			return false
		}
		waiting = append(waiting[1:], rec[a:b])
	}
	return true
}

// oneRunSinceBegin is w with one run's half from its test's begin
// (Span.sinceBegin) and the other's as w has it, each way round: the
// windows that hold a read of the clock the application made for the call
// in one run and earlier in its test in the other. A full round that ran
// slower than the recording (or faster) shows so a read made at the start
// of a handler several calls before the call: in the recording those calls
// took a millisecond and the call's own window holds the read; in the full
// round they took most of a second, and only the window since the test
// began does. Either way one run shows it a read made for the call, so its
// learning does not hang on the full round's speed. nil where either run
// knows no begin; each window holds all w holds.
func (w *Window) oneRunSinceBegin() []*Window {
	wb := w.sinceBegin()
	if wb == nil {
		return nil
	}
	return []*Window{{Rec: w.Rec, F: wb.F, offsets: w.offsets}, {Rec: wb.Rec, F: w.F, offsets: w.offsets}}
}

// holds reports whether t, an instant read at resolution res, is a read of
// the clock within s: t stands for the unit [t, t+res) its read was made in
// (a value in seconds is the second its read was made in), and some instant
// of s lies in it. A unit that starts off UTC's grid of res (a wall clock of
// no zone read in the application's zone: the day of a date in IST starts at
// 18:30 UTC) is read so too, never by cutting s's bounds to that grid. A
// res of none is a nanosecond's: t itself lies in s.
func (s Span) holds(t time.Time, res time.Duration) bool {
	if s.From.IsZero() || s.To.IsZero() {
		return false
	}
	if res <= 0 {
		res = time.Nanosecond
	}
	return !t.After(s.To) && t.Add(res).After(s.From)
}

// holds reports whether recorded and live, an instant in layout lay each,
// are reads of their runs' clocks within w (Window). A time of day names no
// day, so its instant falls in no span: only the lead reads it.
func (w *Window) holds(lay clockLayout, recorded, live string) bool {
	if w == nil {
		return false
	}
	rt, rres, ok := lay.read(recorded)
	if !ok {
		return false
	}
	lt, lres, ok := lay.read(live)
	if !ok {
		return false
	}
	return w.Rec.holds(rt, rres) && w.F.holds(lt, lres)
}

// Holds reports whether rec and live, instants a value carried in the
// recording and in this run, read at resolution res, are reads of their
// runs' clocks within w (nil: none is).
func (w *Window) Holds(rec, live time.Time, res time.Duration) bool {
	return w != nil && w.Rec.holds(rec, res) && w.F.holds(live, res)
}

// key is w as a map key, the zero key for none: its spans' bounds, and 1
// last for a reading from each run's test begin (begun), which reads a
// value's words otherwise than its call's own window does even where the
// two windows' bounds are the same.
func (w *Window) key() [5]int64 {
	if w == nil {
		return [5]int64{}
	}
	var begun int64
	if w.begun {
		begun = 1
	}
	return [5]int64{w.Rec.From.UnixNano(), w.Rec.To.UnixNano(), w.F.From.UnixNano(), w.F.To.UnixNano(), begun}
}

// clockOf is clockIn with no call's window: the clock reads that moved by
// FullRoundLead.
func clockOf(recorded, live string) clockSet { return clockIn(recorded, live, nil, true) }

// clockIn is clockAt of a call whose recorded instant is not known: a move by
// the lead is bounded by no call (readNoLaterThan).
func clockIn(recorded, live string, w *Window, lead bool) clockSet {
	return clockAt(recorded, live, w, lead, time.Time{}, nil)
}

// clockAt finds in a drifted value the clock reads that moved by
// FullRoundLead, or that are reads of each run's clock within the window w
// of the call that carries the value (nil: none): the recorded and the live
// value are each searched for instants in clockLayouts, at ASCII word
// boundaries (so one glued to text
// of another script, "创建于2026-10-14 10:00:41", is found), and the k-th
// of a layout in one is read against the k-th in the other: where the live
// one is the recorded one later by the lead, the words that changed between
// them are clock words. Nothing else of the values need line up (a fresh
// signature beside the read may change their number of words). The lead is
// compared at the instant's own resolution, and to the second for finer
// ones: F's clock is set to each recorded instant plus the lead when the
// test begins, and how long the app then takes to read its clock is its own
// (a millisecond read moves by the lead plus that). Without lead (keploy
// mock's client tests: DriftCache.ClientTests) only the window shows a
// read: a value the test harness gave the application may move by the lead.
// A move by the lead is a read only where a read made no later than the call
// that carries the value, recorded at call (zero: not known), makes it
// (readNoLaterThan): one at the minute, the hour or the day moves by one
// unit or two all the same where it was built on another instant, and only
// its call bounds when it was read. A wall clock whose text names no zone is
// read in UTC and in zone, the application's (nil: UTC alone).
func clockAt(recorded, live string, w *Window, lead bool, call time.Time, zone *time.Location) clockSet {
	c := clockSet{rec: map[string]bool{}, live: map[string]bool{}}
	takenR, takenL := make([]bool, len(recorded)), make([]bool, len(live))
	var at []int
	for k, lay := range clockLayouts {
		rm, lm := clockMatches(lay, recorded, 0, len(recorded), takenR), clockMatches(lay, live, 0, len(live), takenL)
		for j := 0; j < len(rm) && j < len(lm); j++ {
			rs, ls := recorded[rm[j][0]:rm[j][1]], live[lm[j][0]:lm[j][1]]
			if rs == ls {
				continue
			}
			// An instant that changed is read once, in the first layout that
			// finds it: one that is no read there is none in a coarser
			// layout read of its words either (the time of day of a date
			// and time read after its call, which names no day a call
			// bounds), so its words are taken whether it is a read or not.
			for i := rm[j][0]; i < rm[j][1]; i++ {
				takenR[i] = true
			}
			for i := lm[j][0]; i < lm[j][1]; i++ {
				takenL[i] = true
			}
			if !(lead && moved(k, rs, ls, call, zone)) && !w.holds(lay, rs, ls) {
				continue
			}
			rt, rres, _ := lay.read(rs)
			c.reads = append(c.reads, models.ClockRead{At: rt.UTC(), Res: rres, Day: lay.wraps})
			at = append(at, rm[j][0])
			rw, lw := wordsBy(rs, isWord), wordsBy(ls, isWord)
			for i, w := range rw {
				if lay.timeWord == nil || lay.timeWord(rs, i) {
					c.rec[w] = true
				}
			}
			for i, w := range lw {
				if lay.timeWord == nil || lay.timeWord(ls, i) {
					c.live[w] = true
				}
			}
		}
	}
	// The reads in the order they stand in the recorded value: a layout's
	// are found after an earlier layout's.
	sort.Sort(byPlace{at, c.reads})
	// The whole value is a clock read when every word that changed is a
	// part of one, and some did.
	r, l := tokensBy(recorded, isWord), tokensBy(live, isWord)
	changed := false
	c.whole = true
	for w := range r {
		if !l[w] {
			changed = true
			c.whole = c.whole && c.rec[w]
		}
	}
	for w := range l {
		if !r[w] {
			changed = true
			c.whole = c.whole && c.live[w]
		}
	}
	c.whole = c.whole && changed
	return c
}

// byPlace sorts reads by where each stands in its value (at).
type byPlace struct {
	at    []int
	reads []models.ClockRead
}

func (b byPlace) Len() int           { return len(b.at) }
func (b byPlace) Less(i, j int) bool { return b.at[i] < b.at[j] }
func (b byPlace) Swap(i, j int) {
	b.at[i], b.at[j] = b.at[j], b.at[i]
	b.reads[i], b.reads[j] = b.reads[j], b.reads[i]
}

// clockMatches are the places of s from from to to that hold an instant in
// layout lay, at ASCII word boundaries of s (atWordBounds), and that no
// earlier layout took (taken, of those bytes alone). A reading of s[from:to]
// finds what one of the whole of s does there only where no instant runs
// across from or to (clockWordWindow).
func clockMatches(lay clockLayout, s string, from, to int, taken []bool) [][]int {
	var out [][]int
	for _, m := range lay.re.FindAllStringIndex(s[from:to], -1) {
		m[0], m[1] = m[0]+from, m[1]+from
		if !atWordBounds(s, m[0], m[1]) {
			continue
		}
		free := true
		for i := m[0]; i < m[1]; i++ {
			if taken[i-from] {
				free = false
				break
			}
		}
		if free {
			out = append(out, m)
		}
	}
	return out
}

// atWordBounds reports whether s[a:b] stands at ASCII word boundaries of s:
// no letter or digit of s runs into its first or its last byte.
func atWordBounds(s string, a, b int) bool {
	return (a <= 0 || !isWord(s[a-1]) || !isWord(s[a])) && (b >= len(s) || !isWord(s[b-1]) || !isWord(s[b]))
}

// moved reports whether the live text is the recorded one's instant moved by
// FullRoundLead, read in layout k of clockLayouts, as a read made no later
// than the call recorded at call (zero: not known) moves (readNoLaterThan),
// a wall clock of no zone in UTC or in zone, the application's.
func moved(k int, recorded, live string, call time.Time, zone *time.Location) bool {
	lay := clockLayouts[k]
	rt, rres, ok := lay.read(recorded)
	if !ok {
		return false
	}
	lt, lres, ok := lay.read(live)
	if !ok {
		return false
	}
	if !leadMoved(rt, lt, rres, lres, lay.wraps) {
		return false
	}
	// A time of day names no day, and so no moment a call bounds.
	return lay.wraps || readNoLaterThan(rt, lt, rres, call, namesZone(k, strings.ToUpper(recorded)) && namesZone(k, strings.ToUpper(live)), zone)
}

// readNoLaterThan reports whether rt, an instant as recorded (at resolution
// rres), and lt, as record's full round saw it, that the lead moved
// (leadMoved), can be a read of the application's clock made no later than
// the call that carries it, recorded at call: a value is in a request only
// once the application read it. The recorded read then is no later than
// call (rt, the start of its unit, is no later), and F's read no later than
// that read moved by the lead, as leadMoved compares the two, to the second:
// from the latest the recorded read can be, the end of its unit or the call,
// whichever is first. A value of a minute, an hour or a day that the lead
// moved by a unit or two, or that a read after its call would give, is no
// read of the clock by this: a value built on another instant (a served
// message's time and five minutes) moves so as well. A wall clock whose text
// names no zone (zoned false) is read in UTC and in zone, the one the
// application writes such a clock in (Tracker.SetAppZone; nil: UTC alone),
// and is a read where it is one in either: no other zone, since some zone
// ahead of UTC would put any value up to fourteen hours after its call no
// later than it. A zero call is not known, and bounds nothing.
func readNoLaterThan(rt, lt time.Time, rres time.Duration, call time.Time, zoned bool, zone *time.Location) bool {
	if call.IsZero() {
		return true
	}
	fits := func(r, l time.Time) bool {
		if r.After(call) {
			return false
		}
		latest := r.Add(rres - time.Nanosecond)
		if latest.After(call) {
			latest = call
		}
		return l.Truncate(time.Second).Sub(latest.Truncate(time.Second)) < FullRoundLead+time.Second
	}
	if fits(rt, lt) {
		return true
	}
	if zoned || zone == nil {
		return false
	}
	return fits(wallIn(rt.UnixNano(), zone), wallIn(lt.UnixNano(), zone))
}

// leadMoved reports whether lt, read at resolution lres, is rt, read at
// rres, moved by FullRoundLead: compared at the coarser of the two
// resolutions, and to the second for finer ones (F's clock is set to each
// recorded instant plus the lead when the test begins, and how long the app
// then takes to read its clock is its own). Of a time of day (wraps), the
// move is read modulo a day.
func leadMoved(rt, lt time.Time, rres, lres time.Duration, wraps bool) bool {
	res := rres
	if lres > res {
		res = lres
	}
	if res < time.Second {
		res = time.Second
	}
	d := lt.Truncate(res).Sub(rt.Truncate(res))
	if wraps {
		for d <= -12*time.Hour {
			d += 24 * time.Hour
		}
		for d > 12*time.Hour {
			d -= 24 * time.Hour
		}
	}
	return d > FullRoundLead-res && d < FullRoundLead+res
}

// An instant before 2000 or after 2100 is no clock read of a recording.
var (
	clockFrom = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	clockTo   = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

func inClockRange(t time.Time) bool { return !t.Before(clockFrom) && t.Before(clockTo) }

func readRFC1123(s string) (time.Time, time.Duration, bool) {
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, "Mon, 02 Jan 2006 15:04:05 UTC"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil && inClockRange(t) {
			return t, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

func readISO(s string) (time.Time, time.Duration, bool) {
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "UTC"), "GMT"), " ")
	n := strings.Replace(strings.Replace(s, " ", "T", 1), ",", ".", 1)
	res := 24 * time.Hour
	if i := strings.Index(n, "T"); i >= 0 {
		// The time's own colons, not its zone's ("+05:30").
		clock := n[i+1:]
		if z := strings.IndexAny(clock, "Z+-"); z >= 0 {
			clock = clock[:z]
		}
		res = time.Minute
		if !strings.Contains(clock, ":") {
			res = time.Hour
		}
		if strings.Count(clock, ":") >= 2 {
			res = time.Second
			if f := strings.IndexByte(clock, '.'); f >= 0 {
				res = fractionRes(len(clock) - f - 1)
			}
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07", "2006-01-02T15:04:05",
		"2006-01-02T15:04Z07:00", "2006-01-02T15:04Z0700", "2006-01-02T15:04",
		"2006-01-02T15Z07:00", "2006-01-02T15Z0700", "2006-01-02T15Z07", "2006-01-02T15", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, n, time.UTC); err == nil && inClockRange(t) {
			return t, res, true
		}
	}
	return time.Time{}, 0, false
}

func readCompact(s string) (time.Time, time.Duration, bool) {
	for _, layout := range []string{"20060102T150405Z", "20060102T150405"} {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil && inClockRange(t) {
			return t, time.Second, true
		}
	}
	return time.Time{}, 0, false
}

// readTimeOfDay reads a time of day, to the second or the minute, or to its
// fraction of a second ("10:00:41.250").
func readTimeOfDay(s string) (time.Time, time.Duration, bool) {
	if t, err := time.ParseInLocation("15:04:05", s, time.UTC); err == nil {
		if f := strings.IndexByte(s, '.'); f >= 0 {
			return t, fractionRes(len(s) - f - 1), true
		}
		return t, time.Second, true
	}
	if t, err := time.ParseInLocation("15:04", s, time.UTC); err == nil {
		return t, time.Minute, true
	}
	return time.Time{}, 0, false
}

// readEpoch reads seconds (10 digits, with or without a fraction),
// milliseconds (13), microseconds (16) or nanoseconds (19) since the epoch,
// each to its own resolution.
func readEpoch(s string) (time.Time, time.Duration, bool) {
	var t time.Time
	var res time.Duration
	digits := s
	if i := strings.IndexByte(s, '.'); i >= 0 {
		// Its seconds and its fraction's digits read exactly, never through
		// a float64, which holds a second's fraction near 2026 only to
		// about 240 ns: a value the application cut to the millisecond
		// must read as a whole millisecond (readRes tells the cut by it).
		digits = s[:i]
		n, err := strconv.ParseInt(digits, 10, 64)
		frac := s[i+1:]
		if err != nil || frac == "" || strings.Trim(frac, "0123456789") != "" {
			return time.Time{}, 0, false
		}
		ns, _ := strconv.ParseInt((frac + "00000000")[:9], 10, 64)
		t, res = time.Unix(n, ns).UTC(), fractionRes(len(frac))
	} else {
		n, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return time.Time{}, 0, false
		}
		switch len(digits) {
		case 10:
			t, res = time.Unix(n, 0).UTC(), time.Second
		case 13:
			t, res = time.UnixMilli(n).UTC(), time.Millisecond
		case 16:
			t, res = time.UnixMicro(n).UTC(), time.Microsecond
		case 19:
			t, res = time.Unix(0, n).UTC(), time.Nanosecond
		default:
			return time.Time{}, 0, false
		}
	}
	if !inClockRange(t) {
		return time.Time{}, 0, false
	}
	return t, res, true
}

// fractionRes is the resolution of a fraction of a second written with n
// digits: a tenth of a second for one, a nanosecond for nine or more.
func fractionRes(n int) time.Duration {
	res := time.Second
	for ; n > 0 && res > time.Nanosecond; n-- {
		res /= 10
	}
	return res
}

// readUUIDTime reads the instant of a time-ordered UUID, each to its own
// resolution: version 7 (its first 48 bits, milliseconds since the epoch) or
// version 1 (60 bits of 100-nanosecond intervals since 1582-10-15).
func readUUIDTime(s string) (time.Time, time.Duration, bool) {
	h := strings.ReplaceAll(s, "-", "")
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != 16 {
		return time.Time{}, 0, false
	}
	var t time.Time
	var res time.Duration
	switch b[6] >> 4 {
	case 7:
		ms := int64(b[0])<<40 | int64(b[1])<<32 | int64(b[2])<<24 | int64(b[3])<<16 | int64(b[4])<<8 | int64(b[5])
		t, res = time.UnixMilli(ms).UTC(), time.Millisecond
	case 1:
		ts := (int64(b[6]&0x0f)<<56 | int64(b[7])<<48 | int64(b[4])<<40 | int64(b[5])<<32 | int64(b[0])<<24 | int64(b[1])<<16 | int64(b[2])<<8 | int64(b[3]))
		const gregorianToUnix = 122192928000000000 // 100 ns intervals from 1582-10-15 to 1970-01-01
		t, res = time.Unix(0, (ts-gregorianToUnix)*100).UTC(), 100*time.Nanosecond
	default:
		return time.Time{}, 0, false
	}
	if !inClockRange(t) {
		return time.Time{}, 0, false
	}
	return t, res, true
}

// uuidTimeWord reports whether word i (0-based, of the five groups) of a
// time-ordered UUID holds its instant: the first two groups of version 7,
// the first three of version 1. The others are random or a node.
func uuidTimeWord(s string, i int) bool {
	if len(s) < 15 {
		return false
	}
	switch s[14] {
	case '7':
		return i < 2
	case '1':
		return i < 3
	}
	return false
}
