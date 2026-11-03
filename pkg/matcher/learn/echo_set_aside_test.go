package learn

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// echoSetAside is what an echo refused past places the rule a copy follows
// its source showed are no source of it says after naming the first.
const echoSetAside = "; but the call's recorded answer holds it, an echo keploy cannot answer here: "

// echoLearned is what an echo learned past such places says after naming
// the first.
const echoLearned = ", and keploy learns it as an echo of the call's answer"

// namesInOrder reports whether why holds each of names, each after the one
// before it.
func namesInOrder(why string, names []string) bool {
	for _, n := range names {
		i := strings.Index(why, n)
		if i < 0 {
			return false
		}
		why = why[i+len(n):]
	}
	return true
}

// An echo of the call's answer (its recorded answer holds the recorded value
// whole) whose search for a source went past places the rule a copy follows
// its source showed are no source of it (follows.echoed) is learned as that
// echo, naming the first place, on the terms noise is learned past them: the
// recording and F gave the value's candidate sources before the call in the
// same order, and F gave none of them only in an older test of the set.
// Otherwise it is refused, naming the echo, the place, and why, never with
// the word rule's own reason ("keploy cannot tell whether it came from
// there"), which says the opposite of what the rule showed.
//
// node-jwt's signup: Sequelize INSERTs the user with the bcrypt hash of the
// password in a bind, RETURNING the row, so the call's recorded answer holds
// the hash whole. bcrypt's salt is random, so the recorded hash and the one
// record's full round saw differ, and by chance the recorded one holds a
// one-character word ("4" between "$" and "/") that the start-up gave the
// application in both runs (PostgreSQL 16.4) and the hash F saw lacks: an
// application that copied the word would have copied it in both runs. The
// verdict must not depend on that chance: without the echo the value is
// noise; with it, it is the echo it is in a run whose salt holds no such
// word.
func TestAnEchoPastPlacesTheCopyRuleShowedNoSourceIsLearnedAsTheEcho(t *testing.T) {
	const (
		rec  = "$2a$08$4/mVkg0pHoYeBxeXUi5k9.kpzNGWz6pixOn81Cn4NoDZCzP8Zb/H2"
		live = "$2a$08$Qx7mUu3rY1oT9pLkWw2zEe.aBcDeFgHiJkLmNoPqRsTuVwXyZ01234"
	)
	echo := func(v string) []Leaf {
		return []Leaf{{Path: "rows[0][2]", Noise: "rows[0][2]", Type: String, Value: v}}
	}
	if got := classifyF(t, rec, live, nil, eStartup(), nil); got.Kind != KindNoise {
		t.Errorf("a hash beside a chance word, not echoed: %s (%q); want noise", got.Kind, got.Why)
	}
	got := classifyF(t, rec, live, nil, eStartup(), echo(rec))
	switch {
	case got.Kind != KindEcho || len(got.Echo) != 1 || got.Echo[0].Path != "rows[0][2]":
		t.Errorf("a hash beside a chance word, echoed: %s (%q) echo %+v; want an echo of rows[0][2]", got.Kind, got.Why, got.Echo)
	case !strings.Contains(got.Why, "a part of its recorded value") || !strings.Contains(got.Why, "stands in mock-2") || !strings.HasSuffix(got.Why, echoLearned):
		t.Errorf("a hash beside a chance word, echoed: the reason %q; want it to name the part set aside in mock-2, then %q", got.Why, echoLearned)
	case strings.Contains(got.Why, "cannot tell whether it came from there"):
		t.Errorf("a hash beside a chance word, echoed: the reason %q says keploy cannot tell what the rule told", got.Why)
	}

	// Flyway's execution_time, 82 at record, 1 in F beside SELECT 1, that
	// the call's answer echoes: the word the rule let go is named.
	got = classifyF(t, "82", "1", nil, eStartup(), echo("82"))
	if got.Kind != KindEcho || !strings.Contains(got.Why, "stands in mock-1") || !strings.Contains(got.Why, "only one of the two values holds") || !strings.HasSuffix(got.Why, echoLearned) {
		t.Errorf("82 echoed, 1 in F beside SELECT 1: %s (%q); want an echo naming the word let go in mock-1", got.Kind, got.Why)
	}

	// The order of the candidate sources: mock-a answers F's 7, mock-b the
	// recorded 82. An application that copies the last answer took b's 82
	// at record; F gave b first, so it would have taken a's 7: no echo
	// stands on an order neither run shows the same.
	a := eAnswer("mock-a", 300*time.Millisecond, "7")
	b := eAnswer("mock-b", 200*time.Millisecond, "82")
	got = classifyF(t, "slot 82", "slot 7", nil, []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}, echo("slot 82"))
	if got.Kind != KindEcho || !strings.HasSuffix(got.Why, echoLearned) {
		t.Errorf("slot 82 echoed, slot 7 in F, F served a then b: %s (%q); want an echo", got.Kind, got.Why)
	}
	got = classifyF(t, "slot 82", "slot 7", nil, []Raw{a.ServedAs(2).ServedAsRecorded(), b.ServedAs(1).ServedAsRecorded()}, echo("slot 82"))
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, echoSetAside+"the recording and record's full round gave the application the earlier answers in a different order") || !strings.Contains(got.Why, " in mock-") {
		t.Errorf("FALSE GREEN? slot 82 echoed, slot 7 in F, F served b then a: %s (%q); want refused for the order, naming the echo", got.Kind, got.Why)
	}

	// A place F gave the application only in an older test of the set
	// shows nothing of what the application did with the word since.
	older := eStartup()
	for i := range older {
		older[i] = older[i].ServedInAnOlderTest()
	}
	got = classifyF(t, "82", "1", nil, older, echo("82"))
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "stands in mock-1") || !strings.Contains(got.Why, echoSetAside+"record's full round gave the application mock-1 "+olderSetAside) {
		t.Errorf("FALSE GREEN? 82 echoed, 1 in F beside SELECT 1 F gave only in an older test: %s (%q); want refused naming mock-1 and why", got.Kind, got.Why)
	}
	// Both: the recorded value holds b's word, and F gave b only in an older
	// test, so the call may be a late call of that test. The copy rule sets
	// b aside as no source of it at no point, and the value is refused as
	// that late call, naming b, before any order is read
	// (TestALateCallsValueAnOlderServeGaveIsNeverLearnedAsNoise).
	got = classifyF(t, "slot 82", "slot 7", nil, []Raw{a.ServedAs(2).ServedAsRecorded().ServedInAnOlderTest(), b.ServedAs(1).ServedAsRecorded().ServedInAnOlderTest()}, echo("slot 82"))
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "record's full round gave the application mock-b only in an older test than the call's: the call may be a late call of that test") || strings.Contains(got.Why, olderSetAside) || strings.Contains(got.Why, echoLearned) {
		t.Errorf("FALSE GREEN? slot 82 echoed, slot 7 in F, F served b then a, both only in an older test: %s (%q); want refused as a late call of b's test", got.Kind, got.Why)
	}
	// Only b, in an older test: the recorded value holds b's 82 and c's 31,
	// each a place both runs gave unchanged, and the value F saw (7 46)
	// holds no word of any place, so no late call of b's test is read. The
	// recording gave c then b, F b (in the older test) then c: the echo is
	// refused for that order, which refuses it whatever test b was in, and
	// the reason names the order, never b's older test.
	c := eAnswer("mock-c", 300*time.Millisecond, "31")
	got = classifyF(t, "slot 82 31", "slot 7 46", nil, []Raw{c.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}, echo("slot 82 31"))
	if got.Kind != KindEcho || !strings.HasSuffix(got.Why, echoLearned) {
		t.Errorf("slot 82 31 echoed, slot 7 46 in F, F served c then b: %s (%q); want an echo", got.Kind, got.Why)
	}
	got = classifyF(t, "slot 82 31", "slot 7 46", nil, []Raw{c.ServedAs(2).ServedAsRecorded(), b.ServedAs(1).ServedAsRecorded().ServedInAnOlderTest()}, echo("slot 82 31"))
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, echoSetAside+"the recording and record's full round gave the application the earlier answers in a different order") || strings.Contains(got.Why, olderSetAside) || strings.Contains(got.Why, "late call") {
		t.Errorf("FALSE GREEN? slot 82 31 echoed, slot 7 46 in F, F served b (only in an older test) then c: %s (%q); want refused for the order, never naming b's older test", got.Kind, got.Why)
	}

	// A word that is a source in both runs (both values hold it) is no
	// place the rule set aside: the echo beside it stays refused, naming
	// it, as the word rule refuses it.
	got = classifyF(t, "82 1", "4 1 1", nil, eStartup(), echo("82 1"))
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "the recorded value and the value record's full round saw both hold that word") || !strings.Contains(got.Why, "in mock-1") {
		t.Errorf("FALSE GREEN? 82 1 echoed, 4 1 1 in F beside SELECT 1: %s (%q); want refused naming the word both hold", got.Kind, got.Why)
	}
}

// A leaf set aside (follows.refuteSeen) is named after the first place an
// echo's reason names, where that is a place the search for the value's
// source went past, a place F gave another value at, or a word held by
// chance (follows.setAside), not only after an unserved sibling
// (TestAnEqualStampBesideASiblingFHadNotServedIsAnEcho): here a place the
// word rule let a word of the value go at, where F gave another value that
// the value took none of the words of (moved), and a recording of another
// test that both runs gave the word at, which the value holds by chance
// (chance). The leaf is an answer F served before the call that held the
// recorded value whole, where F gave the recorded value with another word
// after it: the word rule lets the value's words go there, as F gave them as
// recorded, and the leaf is no source, as F saw it hold another value than
// the value's. The echo is learned where both runs gave the places in one
// order, and refused where they did not, naming the first place, then the
// leaf, either way.
func TestAnEchoNamesALeafSetAsideAfterAPlaceTheValueMovedFromOrHoldsAWordOfByChance(t *testing.T) {
	const asideWhy = "; and its recorded value stands whole in mock-9 at body.at, where record's full round gave the application another value than the value's: a copy moves with its source, so the value is no copy of it"
	const order = echoSetAside + "the recording and record's full round gave the application the earlier answers in a different order"
	check := func(what string, got Learned, kind Kind, names []string) {
		t.Helper()
		if got.Kind != kind || !namesInOrder(got.Why, names) {
			lead := ""
			if kind == KindUnaccounted && got.Kind != KindUnaccounted {
				lead = "FALSE GREEN? "
			}
			t.Errorf("%s%s: %s (%q); want %s naming, in this order, %q", lead, what, got.Kind, got.Why, kind, names)
		}
		if !slices.Contains(names, asideWhy) && strings.Contains(got.Why, "; and ") {
			t.Errorf("%s: %s (%q); want no leaf set aside named where none was", what, got.Kind, got.Why)
		}
	}
	aside := func(at time.Time, n int, recorded, live string) Earlier {
		payload := RawOf(&models.Mock{Name: "mock-9", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}, true)
		return Earlier{Mock: "mock-9", At: at, Payload: payload.ServedAs(n), Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "body.at", Type: String, Value: recorded}, Live: live},
		}}
	}

	// Moved: mock-8's request id held "48d1" of the recorded value, and F
	// gave another id there, which the value took none of the words of
	// (TestAWordAtAPlaceTheFullRoundGaveAnotherValueAtIsNoCopyWhereTheValueTookNoneOfIt).
	const moved = "a word of its recorded value (its characters 20 to 23) stands in mock-8 at body.cmd.documents.0._id, where record's full round gave the application another value, which the value took none of the words of: a copy moves with its source, so the value is no copy of it"
	before := eRecAt.Add(-50 * time.Millisecond)
	movedAt := func(n int) Earlier {
		return Earlier{Mock: "mock-8", At: before, Payload: eLeaf(before, n, "", "").Payload, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.cmd.documents.0._id", Type: String, Value: chanceSrcRec}, Live: chanceSrcLive},
		}}
	}
	echo := []Leaf{{Path: "rows[0][2]", Noise: "rows[0][2]", Type: String, Value: chanceValRec}}
	// F gave the leaf the recorded value with another word after it.
	const grown = chanceValRec + " zzzz"
	for _, row := range []struct {
		what    string
		earlier []Earlier
		kind    Kind
		names   []string
	}{
		{"moved, then the leaf, served in that order", []Earlier{movedAt(6), aside(before.Add(10*time.Millisecond), 7, chanceValRec, grown)}, KindEcho, []string{moved, asideWhy, echoLearned}},
		{"moved, then the leaf, F serving the leaf first", []Earlier{movedAt(7), aside(before.Add(10*time.Millisecond), 6, chanceValRec, grown)}, KindUnaccounted, []string{moved, asideWhy, order}},
		{"moved alone", []Earlier{movedAt(6)}, KindEcho, []string{moved, echoLearned}},
	} {
		check(row.what, classifyF(t, chanceValRec, chanceValLive, row.earlier, nil, echo), row.kind, row.names)
	}

	// Chance: "48d1" of another test's message id, which both runs gave the
	// application as recorded (TestAChanceWordOfAnotherTestsAnswerIsLearnedAsVarying).
	const (
		p        = "body.appId"
		srcID    = "bb7ed9ee-670e-48d1-982f-b54b2b73982f"
		chance   = "5c0e9a7b-1f2d-48d1-9a3b-c4d5e6f70812" // the recorded appId: "48d1" by chance
		other    = "0c1d2e3f-4a5b-4c7d-8e9f-a0b1c2d3e4f5" // the appId F saw
		grownBy  = chance + " zzzz"                       // the leaf as F gave it
		byChance = "a word of its recorded value (its characters 15 to 18) stands in mock-31, which the recording and record's full round both gave the application at the same fields before the call, and the value record's full round saw lacks that word, and no word of it that changed, but for a read of the clock, stands in any place either run gave the application before the call: an application that copied the word would have copied it in both runs, so the value holds it by chance"
	)
	src := receiveOf("mock-31", srcID, r1Insert.Add(-time.Minute))
	near := r1Insert.Add(-50 * time.Millisecond)
	for _, row := range []struct {
		what    string
		n       int // mock-31's serve
		earlier []Earlier
		kind    Kind
		names   []string
	}{
		{"by chance, then the leaf, served in that order", 3, []Earlier{aside(near, 4, chance, grownBy)}, KindEcho, []string{byChance, asideWhy, echoLearned}},
		{"by chance, then the leaf, F serving the leaf first", 5, []Earlier{aside(near, 4, chance, grownBy)}, KindUnaccounted, []string{byChance, asideWhy, order}},
		{"by chance alone", 3, nil, KindEcho, []string{byChance, echoLearned}},
	} {
		dc := clockedDrifts()
		x := dc.RawOf(src, true).ServedAs(row.n).ServedAsRecorded().OfAnotherTest()
		resp := []Leaf{{Path: "body.id", Noise: "body.id", Type: String, Value: chance}}
		check(row.what, ClassifyFollowing(dc, []string{p}, strLeaf(p, other), strLeaf(p, chance), r1Insert, row.earlier, []Raw{x}, resp, nil, nil)[0], row.kind, row.names)
	}
}
