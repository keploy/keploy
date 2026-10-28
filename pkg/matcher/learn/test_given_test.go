package learn

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A given's why names the variable, where the test gave it, both values it
// carried, and how to undo it; a long value is cut, never whole twice.
func TestGivenWhyIsLegible(t *testing.T) {
	why := GivenWhy("given:app", "", "a", "b")
	for _, part := range []string{"the test gave given:app: \"a\" when recorded, \"b\" in record's full round", "remove the test's call that gives it and re-record"} {
		if !strings.Contains(why, part) {
			t.Fatalf("why %q lacks %q", why, part)
		}
	}
	long := strings.Repeat("é", 100)
	why = GivenWhy("given:tok", "x_test.go:3", long, "b")
	if strings.Contains(why, long) || !strings.Contains(why, "... (200 bytes)") || !strings.Contains(why, `"`+strings.Repeat("é", givenWhyValue/2)+`"`) {
		t.Fatalf("a long value is not cut on a rune: %q", why)
	}
	if GivenNamed("given:app") != "the value the test gave as given:app" || GivenNamed("userId") != "the test case's userId" {
		t.Fatal("GivenNamed")
	}
}

// A value the test gave keploy is an earlier place of its calls once given
// (Tracker.GiveValue), after its test case's request, until the next test
// begins or the set changes. One given while no test is open, and one the
// recording lacks, is no place.
func TestTheTrackerHoldsTheTestsGivensForItsCalls(t *testing.T) {
	const rec, full = "1761609600123456789", "1761700000000000001"
	tr := NewTracker()
	tr.GiveValue(TestGiven("test o::TestApps", "given:nonce", rec, full))
	tr.Begin()
	if p := tr.Placed(); len(p.Earlier) != 0 {
		t.Fatalf("a given while no test was open is a place of the next test: %+v", p.Earlier)
	}
	tr.Give(GivenRequest(&models.TestRequest{Test: "TestApps"}))
	tr.GiveValue(TestGiven("test o::TestApps", "given:unrecorded", "", full))
	tr.GiveValue(TestGiven("test o::TestApps", "given:nonce", rec, full))
	p := tr.Placed()
	if len(p.Earlier) != 2 || p.Earlier[0].Gave || !p.Earlier[0].Given {
		t.Fatalf("places %+v: want the test case's request, then the one recorded given", p.Earlier)
	}
	g := p.Earlier[1]
	if !g.Given || !g.Gave || g.Mock != "test o::TestApps" || len(g.Seen) != 1 || g.Seen[0].Leaf.Path != "given:nonce" || g.Seen[0].Leaf.Value != rec || g.Seen[0].Live != full || g.Seen[0].Side != Request {
		t.Fatalf("the given's place %+v", g)
	}
	tr.End()
	if p := tr.Placed(); len(p.Earlier) != 2 {
		t.Fatalf("an ended test's calls still in flight read its givens: %+v", p.Earlier)
	}
	// A given after the test ended, before the next begins, is no test's.
	tr.GiveValue(TestGiven("test o::TestApps", "given:late", rec, full))
	if p := tr.Placed(); len(p.Earlier) != 2 || p.Earlier[1].Seen[0].Leaf.Path != "given:nonce" {
		t.Fatalf("a given after the test ended is a place: %+v", p.Earlier)
	}
	tr.Begin()
	if p := tr.Placed(); len(p.Earlier) != 0 {
		t.Fatalf("the next test reads the previous test's givens: %+v", p.Earlier)
	}
	tr.GiveValue(TestGiven("test o::TestApps", "given:nonce", rec, full))
	if p := tr.Placed(); len(p.Earlier) != 1 || !p.Earlier[0].Gave {
		t.Fatalf("the open test's given is no place: %+v", p.Earlier)
	}
	// A new set forgets the open test's givens at once, not at the next Begin.
	tr.NewSet()
	if p := tr.Placed(); len(p.Earlier) != 0 {
		t.Fatalf("the next set reads the previous set's givens: %+v", p.Earlier)
	}
}

// A value made from one the test gave (the given inside a longer value) is a
// variable of the given's place, which the adapter refuses naming the given,
// never noise; the given whole is too (the adapter binds it as the given's
// variable first). A moment the given holds that a typed value was recorded
// with, picked by a rule the given as given in record's full round does not
// explain, is refused naming the given.
func TestAValueMadeFromAGivenIsRefusedNamingIt(t *testing.T) {
	at := time.Date(2026, 10, 22, 11, 13, 40, 0, time.UTC)
	const rec, full = "1761609600123456789", "1761700000000000001"
	const p = "body.name"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	gave := []Earlier{TestGiven("test o::TestApps", "given:nonce", rec, full)}
	for _, f := range [][2]string{{"app-" + rec, "app-" + full}, {"app-" + rec + "-x", "app-" + full + "-x"}, {rec, full}} {
		ln := ClassifyIn(NewDriftCache(), []string{p}, shape(f[1]), shape(f[0]), at, gave, nil, nil, nil)[0]
		if ln.Kind != KindVariable || !ln.From.Given || !ln.From.Gave || ln.From.Mock != "test o::TestApps" || ln.From.Path != "given:nonce" || ln.From.Whole != (f[0] == rec) {
			t.Errorf("%q -> %q, made from the given: %s from %+v (%s); want a variable of given:nonce", f[0], f[1], ln.Kind, ln.From, ln.Why)
		}
	}

	// A typed instant whose text is the given whole is the given's place; one
	// that only names a moment the given holds is not (no moment rule reads
	// a given): it learns as it does with no given.
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 152e6, time.UTC)
	typed := func(at time.Time) Shape {
		return Shape{Leaves: map[string]Leaf{p: InstantLeaf(p, p, at, time.Millisecond)}}
	}
	recStamp, fullStamp := typed(recAt).Leaves[p].Value, typed(recAt.Add(FullRoundLead)).Leaves[p].Value
	gave = []Earlier{TestGiven("test o::TestApps", "given:at", recStamp, fullStamp)}
	if ln := ClassifyIn(NewDriftCache(), []string{p}, typed(recAt.Add(FullRoundLead)), typed(recAt), at, gave, nil, nil, nil)[0]; ln.Kind != KindVariable || !ln.From.Gave || !ln.From.Whole || ln.From.Path != "given:at" {
		t.Errorf("a typed value that is the given's text: %s from %+v (%s); want a variable of given:at", ln.Kind, ln.From, ln.Why)
	}
}

// A given never makes a read of the application's own clock a place of it:
// the test makes a nonce from its clock (UnixNano) and the application
// stamps createdAt from its own in the same second. createdAt learns as it
// does with no given (a read of the clock), whether the test's clock runs
// on real time in record's full round or moves with the application's; a
// value made from the nonce in the same call is still refused, naming the
// given.
func TestAGivenLeavesTheApplicationsClockReadsAsTheyAre(t *testing.T) {
	recAt := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	at := recAt.Add(300 * time.Millisecond)
	const stamp, name = "body.createdAt", "body.name"
	nonceRec := strconv.FormatInt(recAt.Add(-40*time.Millisecond).UnixNano(), 10)
	shape := func(created time.Time, nonce string) Shape {
		return Shape{Leaves: map[string]Leaf{
			stamp: InstantLeaf(stamp, stamp, created, time.Second),
			name:  {Path: name, Noise: name, Type: String, Value: "app-" + nonce},
		}}
	}
	alone := ClassifyIn(NewDriftCache(), []string{stamp}, shape(recAt.Add(FullRoundLead), ""), shape(recAt, ""), at, nil, nil, nil, nil)[0]
	if alone.Kind != KindClockRead {
		t.Fatalf("with no given, the application's clock read is %s (%s); want a read of the clock", alone.Kind, alone.Why)
	}
	for regime, nonceFull := range map[string]string{
		"test clock on real time":              strconv.FormatInt(recAt.Add(26*time.Hour+3*time.Second).UnixNano(), 10),
		"test clock moved with the app's":      strconv.FormatInt(recAt.Add(-40*time.Millisecond+FullRoundLead+5*time.Millisecond).UnixNano(), 10),
		"test clock moved exactly by the lead": strconv.FormatInt(recAt.Add(-40*time.Millisecond+FullRoundLead).UnixNano(), 10),
	} {
		gave := []Earlier{TestGiven("test o::TestApps", "given:nonce", nonceRec, nonceFull)}
		got := ClassifyIn(NewDriftCache(), []string{stamp, name}, shape(recAt.Add(FullRoundLead), nonceFull), shape(recAt, nonceRec), at, gave, nil, nil, nil)
		if got[0].Kind != alone.Kind || got[0].Why != alone.Why || got[0].From != alone.From {
			t.Errorf("%s: the application's clock read beside a given nonce is %s from %+v (%s); want it as with no given: %s", regime, got[0].Kind, got[0].From, got[0].Why, alone.Kind)
		}
		if got[1].Kind != KindVariable || !got[1].From.Gave || got[1].From.Path != "given:nonce" || got[1].From.Whole {
			t.Errorf("%s: a value made from the nonce is %s from %+v (%s); want the given's part", regime, got[1].Kind, got[1].From, got[1].Why)
		}
	}
}

// A refusal of a value made from a given names the leaf, the given, where
// the test gave it, and what to do.
func TestGivenDerivedWhyIsLegible(t *testing.T) {
	why := GivenDerivedWhy("body.name", "given:nonce", "apps_test.go:20")
	for _, part := range []string{"body.name carries a value made from the value the test gave as given:nonce (apps_test.go:20)", "keploy follows a given only where a call carries it whole", "Give the value the application sends on as a given of its own"} {
		if !strings.Contains(why, part) {
			t.Fatalf("why %q lacks %q", why, part)
		}
	}
	if strings.Contains(GivenDerivedWhy("body.name", "given:nonce", ""), "()") {
		t.Fatal("an unknown source is named empty")
	}
}
