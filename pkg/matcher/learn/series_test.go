package learn

import (
	"strconv"
	"testing"
	"time"
)

// pollLeaves are the leaves of a timer's claim (a findAndModify that takes
// one job marked at least 2 s ago, or first marked 30 s ago, and not taken
// in the last minute, stamping it taken now), its clock read at read, in
// untyped epoch seconds, as a Mongo request's typed reading has them.
func pollLeaves(read time.Time) map[string]Leaf {
	sec := func(off time.Duration) string {
		return strconv.FormatInt(read.Truncate(time.Second).Add(off).Unix(), 10)
	}
	ls := map[string]Leaf{}
	add := func(p string, t Type, v string) { ls[p] = Leaf{Path: p, Noise: p, Type: t, Value: v} }
	add("body.cmd.query.markedAt.$gt", Number, "0")
	add("body.cmd.query.$and.0.$or.0.markedAt.$lte", Number, sec(-2*time.Second))
	add("body.cmd.query.$and.0.$or.1.firstMarkedAt.$lte", Number, sec(-30*time.Second))
	add("body.cmd.query.$and.1.$or.0.takenAt.$exists", Bool, "false")
	add("body.cmd.query.$and.1.$or.1.takenAt.$lte", Number, sec(-60*time.Second))
	add("body.cmd.update.$set.takenAt", Number, sec(0))
	add("body.cmd.new", Bool, "true")
	return ls
}

var pollFixed = map[string]string{
	"body.cmd.query.$and's kind (an array or a document)":       "array",
	"body.cmd.query.$and.0.$or's kind (an array or a document)": "array",
	"body.cmd.query.$and.1.$or's kind (an array or a document)": "array",
}

const pollKey = "2013\x00findAndModify\x00jobs\x00app"

// pollSeries is n recordings of the timer's claim, a 2 s ticker's calls
// (pollAt), each read made just before its call reached keploy and after
// the previous call did.
func pollSeries(base time.Time, n int) []SeriesRead {
	reads := make([]SeriesRead, n)
	for i := range reads {
		at := pollAt(base, i)
		reads[i] = SeriesRead{
			Shape: Shape{Key: pollKey, Fixed: pollFixed, Leaves: pollLeaves(at.Add(-time.Millisecond))},
			Span:  Span{From: at.Add(-1950 * time.Millisecond), To: at},
		}
	}
	return reads
}

// livePoll is the claim made in a replay at at (its span from 1.5 s before
// to a millisecond after), its clock read moved by skew from the run's clock.
func livePoll(at time.Time, skew time.Duration) (Shape, Span) {
	return Shape{Key: pollKey, Fixed: pollFixed, Leaves: pollLeaves(at.Add(skew))},
		Span{From: at.Add(-1500 * time.Millisecond), To: at.Add(time.Millisecond)}
}

// The timer's claim recorded six times is one series: its four epoch leaves
// are reads of the clock (offsets 0, -2 s, -30 s, -60 s), the rest values.
// A replay's claim made a day later holds; one whose clock read is an hour
// off, or a minute, does not (a miss, never served), nor one with a value
// changed, a leaf more or less, another operation, or no span.
func TestSeriesOf_ATimersClaimIsOneSeriesOfItsClockReads(t *testing.T) {
	reads := pollSeries(epochBase, 6)
	ss := SeriesOf(reads, nil)
	if len(ss) != 1 || len(ss[0].Members) != 6 {
		t.Fatalf("the claim's six recordings learned %d series %+v, want one of six", len(ss), ss)
	}
	s := ss[0]
	want := map[string]time.Duration{
		"body.cmd.query.$and.0.$or.0.markedAt.$lte":      -2 * time.Second,
		"body.cmd.query.$and.0.$or.1.firstMarkedAt.$lte": -30 * time.Second,
		"body.cmd.query.$and.1.$or.1.takenAt.$lte":       -60 * time.Second,
		"body.cmd.update.$set.takenAt":                   0,
	}
	if len(s.Clock) != len(want) || len(s.Values) != 3 {
		t.Fatalf("clock leaves %v, values %v: want the four epoch leaves as reads of the clock", s.Clock, s.Values)
	}
	for p, off := range want {
		c, ok := s.Clock[p]
		if !ok || c.Offset.Lo > off || c.Offset.Hi < off || c.Offset.Res != time.Second {
			t.Fatalf("%s: learned %+v %v, want a read of the clock at %v", p, c, ok, off)
		}
	}
	if s.Members[len(s.Members)-1] != 5 {
		t.Fatalf("members %v, want the reads in order, the newest last", s.Members)
	}
	day := time.Date(2026, 10, 30, 8, 0, 7, 700*int(time.Millisecond), time.UTC)
	live, span := livePoll(day, -time.Millisecond)
	if !s.Holds(live, span, nil) {
		t.Fatal("a replay's claim a day later, its clock reads on the run's clock, does not hold")
	}
	for _, skew := range []time.Duration{time.Hour, -time.Hour, -time.Minute, 3 * time.Second} {
		if l, sp := livePoll(day, skew); s.Holds(l, sp, nil) {
			t.Fatalf("a claim whose clock reads are %v off the run's clock holds", skew)
		}
	}
	// One read alone off the run's clock is a miss too.
	off, sp := livePoll(day, -time.Millisecond)
	l := off.Leaves["body.cmd.query.$and.1.$or.1.takenAt.$lte"]
	l.Value = strconv.FormatInt(day.Unix()-3600, 10)
	off.Leaves["body.cmd.query.$and.1.$or.1.takenAt.$lte"] = l
	if s.Holds(off, sp, nil) {
		t.Fatal("a claim with one clock read an hour off the run's clock holds")
	}
	changed := func(edit func(*Shape)) Shape {
		c, _ := livePoll(day, -time.Millisecond)
		leaves := make(map[string]Leaf, len(c.Leaves))
		for p, l := range c.Leaves {
			leaves[p] = l
		}
		c.Leaves = leaves
		edit(&c)
		return c
	}
	for name, edit := range map[string]func(*Shape){
		"a value changed": func(c *Shape) {
			c.Leaves["body.cmd.query.markedAt.$gt"] = Leaf{Path: "body.cmd.query.markedAt.$gt", Type: Number, Value: "1"}
		},
		"a leaf more": func(c *Shape) {
			c.Leaves["body.cmd.query.owner"] = Leaf{Path: "body.cmd.query.owner", Type: String, Value: "a"}
		},
		"a leaf less":        func(c *Shape) { delete(c.Leaves, "body.cmd.new") },
		"a clock leaf less":  func(c *Shape) { delete(c.Leaves, "body.cmd.update.$set.takenAt") },
		"another collection": func(c *Shape) { c.Key = "2013\x00findAndModify\x00runs\x00app" },
		"a fixed part less":  func(c *Shape) { c.Fixed = map[string]string{} },
		"a fixed part changed": func(c *Shape) {
			c.Fixed = map[string]string{}
			for n, v := range pollFixed {
				c.Fixed[n] = v
			}
			c.Fixed["body.cmd.query.$and's kind (an array or a document)"] = "document"
		},
		"a fixed part more": func(c *Shape) {
			c.Fixed = map[string]string{"body.cmd.query's dotted key \"a.b\"": "dotted"}
			for n, v := range pollFixed {
				c.Fixed[n] = v
			}
		},
		"a clock read typed": func(c *Shape) {
			// On the run's clock at the epoch's resolution: only its type
			// tells it from the recorded one.
			c.Leaves["body.cmd.update.$set.takenAt"] = InstantLeaf("body.cmd.update.$set.takenAt", "", day.Add(-time.Millisecond), time.Second)
		},
	} {
		if s.Holds(changed(edit), span, nil) {
			t.Fatalf("a claim with %s holds", name)
		}
	}
	if s.Holds(live, Span{To: span.To}, nil) {
		t.Fatal("a claim with no span holds: no read of the clock can be checked")
	}
	// keploy.yml's noise on a value excuses it; a typed noise keeps its type.
	ex := changed(func(c *Shape) {
		c.Leaves["body.cmd.query.markedAt.$gt"] = Leaf{Path: "body.cmd.query.markedAt.$gt", Noise: "body.cmd.query.markedAt.$gt", Type: Number, Value: "9"}
	})
	if !s.Holds(ex, span, known{"body.cmd.query.markedAt.$gt": true}) {
		t.Fatal("a value keploy.yml's noise excuses refuses the claim")
	}
	if s.Holds(ex, span, known{"body.cmd.query.other": true}) {
		t.Fatal("noise on another leaf excuses a changed value")
	}
}

// A number that looks like an epoch but does not track its calls stays a
// value: the timer's claims carry an account number in the clock's range
// (ten digits, three of them, each made in turn), and the recordings part by
// it, each part a series whose account number is a value and whose epoch
// leaves still read the clock; a replay's call with a fourth number holds
// in no series. Two recordings of a call whose epoch differs (fewer than
// MinEpochReads values) are two series, their epochs values.
func TestSeriesOf_AnEpochLikeNumberThatDoesNotTrackItsCallsStaysAValue(t *testing.T) {
	accounts := []string{"2025551234", "1761700000", "2025559876"}
	reads := pollSeries(epochBase, 9)
	for i := range reads {
		reads[i].Shape.Leaves["body.cmd.query.account"] = Leaf{Path: "body.cmd.query.account", Type: Number, Value: accounts[i%3]}
	}
	ss := SeriesOf(reads, nil)
	if len(ss) != 3 {
		t.Fatalf("learned %d series %+v, want one per account number", len(ss), ss)
	}
	for _, s := range ss {
		a, ok := s.Values["body.cmd.query.account"]
		if !ok || len(s.Members) != 3 || len(s.Clock) != 4 {
			t.Fatalf("series %+v: want the account number a value, three members, the four epoch leaves clock", s)
		}
		if _, clock := s.Clock["body.cmd.query.account"]; clock || a.Value != accounts[s.Members[0]%3] {
			t.Fatalf("series %+v: the account number is no value of its own part", s)
		}
	}
	day := time.Date(2026, 10, 30, 8, 0, 7, 0, time.UTC)
	for k, acct := range append([]string{"2025550000"}, accounts...) {
		live, span := livePoll(day, -time.Millisecond)
		live.Leaves["body.cmd.query.account"] = Leaf{Path: "body.cmd.query.account", Type: Number, Value: acct}
		held := 0
		for _, s := range ss {
			if s.Holds(live, span, nil) {
				held++
			}
		}
		if want := min(k, 1); held != want {
			t.Fatalf("account %s: held in %d series, want %d", acct, held, want)
		}
	}
	// A leaf no path names alone (a key holding '.') is never learned as a
	// read of the clock, though it tracks its calls: it is a value, so each
	// recording is a shape of its own, all its leaves values.
	unnamed := pollSeries(epochBase, 4)
	for i := range unnamed {
		l := unnamed[i].Shape.Leaves["body.cmd.update.$set.takenAt"]
		l.Unnamed = true
		unnamed[i].Shape.Leaves["body.cmd.update.$set.takenAt"] = l
	}
	if us := SeriesOf(unnamed, nil); len(us) != 4 || len(us[0].Clock)+len(us[3].Clock) != 0 {
		t.Fatalf("an unnamed epoch leaf learned %+v, want four series of one, all values", us)
	}
	two := SeriesOf(pollSeries(epochBase, 2), nil)
	if len(two) != 2 || len(two[0].Clock) != 0 || len(two[1].Clock) != 0 {
		t.Fatalf("two recordings learned %+v, want two series of one, their epochs values", two)
	}
}

// A field its decoder types as an instant tracks its calls as an epoch
// does; a typed instant left as recorded in a replay a day later is off the
// run's clock and does not hold. A recording of another shape is a series
// of its own: another operation, a clock read written as a typed instant
// where others wrote an epoch number (both tracking their calls), or a leaf
// keploy.yml's typed noise excuses that others do not hold.
func TestSeriesOf_ATypedInstantReadsTheClockAsAnEpochDoes(t *testing.T) {
	reads := pollSeries(epochBase, 4)
	for i := range reads {
		read := reads[i].Span.To.Add(-time.Millisecond)
		reads[i].Shape.Leaves["body.cmd.update.$set.seenAt"] = InstantLeaf("body.cmd.update.$set.seenAt", "", read, time.Millisecond)
	}
	other := pollSeries(epochBase, 1)[0]
	other.Shape.Key = "2013\x00findAndModify\x00runs\x00app"
	ss := SeriesOf(append(reads, other), nil)
	if len(ss) != 2 || len(ss[0].Clock) != 5 || len(ss[1].Members) != 1 || ss[1].Members[0] != 4 {
		t.Fatalf("learned %+v, want the claim's four with five clock leaves and the other shape alone", ss)
	}
	typed := pollSeries(epochBase, 12)
	for i := range typed {
		if i%2 == 1 {
			read := typed[i].Span.To.Add(-time.Millisecond)
			typed[i].Shape.Leaves["body.cmd.update.$set.takenAt"] = InstantLeaf("body.cmd.update.$set.takenAt", "", read, time.Second)
		}
		if i >= 6 {
			typed[i].Shape.Leaves["body.cmd.query.owner"] = Leaf{Path: "body.cmd.query.owner", Noise: "body.cmd.query.owner", Type: String, Value: strconv.Itoa(i)}
		}
	}
	ts := SeriesOf(typed, known{"body.cmd.query.owner": true})
	if len(ts) != 4 {
		t.Fatalf("epochs and typed instants, with and without a noise leaf, learned %d series %+v, want four", len(ts), ts)
	}
	for _, s := range ts {
		if len(s.Members) != 3 || s.Members[1]-s.Members[0] != 2 || s.Members[2]-s.Members[1] != 2 || len(s.Clock) != 4 {
			t.Fatalf("series of members %v, want one type and one noise leaf presence each", s.Members)
		}
	}
	if c := ss[0].Clock["body.cmd.update.$set.seenAt"]; c.Offset.Res != time.Millisecond || c.Leaf.Type != Instant {
		t.Fatalf("the typed instant learned %+v, want a read of the clock in milliseconds", c)
	}
	day := time.Date(2026, 10, 30, 8, 0, 7, 0, time.UTC)
	live, span := livePoll(day, -time.Millisecond)
	live.Leaves["body.cmd.update.$set.seenAt"] = InstantLeaf("body.cmd.update.$set.seenAt", "", day.Add(-time.Millisecond), time.Millisecond)
	if !ss[0].Holds(live, span, nil) {
		t.Fatal("a replay's claim with its typed instant on the run's clock does not hold")
	}
	live.Leaves["body.cmd.update.$set.seenAt"] = reads[3].Shape.Leaves["body.cmd.update.$set.seenAt"]
	if ss[0].Holds(live, span, nil) {
		t.Fatal("a replay's claim with its typed instant as recorded a day before holds")
	}
}
