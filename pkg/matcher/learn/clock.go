package learn

import (
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// FullRoundLead is how far past each recorded instant record's full round F
// runs every test (design v14 §3.4): second- and minute-resolution values the
// app reads from its clock drift there, by this much, and are learned.
const FullRoundLead = 61500 * time.Millisecond

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
// zone in (models.ZoneApp, CheckClockRead); nil where the tracker was told
// none: such a read is then refused. A Window's spans carry none: the learn
// step reads the zone of its DriftCache.
type Span struct {
	From, To time.Time
	Zone     *time.Location
}

// holds reports whether t, an instant read at resolution res, is a read of
// the clock within s: each bound is read at the instant's own resolution (a
// value in seconds is the second its read was made in).
func (s Span) holds(t time.Time, res time.Duration) bool {
	if s.From.IsZero() || s.To.IsZero() {
		return false
	}
	return !t.Before(s.From.Truncate(res)) && !t.After(s.To.Truncate(res))
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

// key is w as a map key, the zero key for none.
func (w *Window) key() [4]int64 {
	if w == nil {
		return [4]int64{}
	}
	return [4]int64{w.Rec.From.UnixNano(), w.Rec.To.UnixNano(), w.F.From.UnixNano(), w.F.To.UnixNano()}
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
		rm, lm := clockMatches(lay, recorded, takenR), clockMatches(lay, live, takenL)
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

// clockMatches are the places of s that hold an instant in layout lay, at
// ASCII word boundaries, and that no earlier layout took (taken).
func clockMatches(lay clockLayout, s string, taken []bool) [][]int {
	var out [][]int
	for _, m := range lay.re.FindAllStringIndex(s, -1) {
		if m[0] > 0 && isWord(s[m[0]-1]) && isWord(s[m[0]]) || m[1] < len(s) && isWord(s[m[1]-1]) && isWord(s[m[1]]) {
			continue
		}
		free := true
		for i := m[0]; i < m[1]; i++ {
			if taken[i] {
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
		digits = s[:i]
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return time.Time{}, 0, false
		}
		t, res = time.Unix(0, int64(f*1e9)).UTC(), fractionRes(len(s)-i-1)
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
