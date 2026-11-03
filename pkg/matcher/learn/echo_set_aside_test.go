package learn

import (
	"strings"
	"testing"
	"time"
)

// echoSetAside is what an echo refused past places the rule a copy follows
// its source showed are no source of it says after naming the first.
const echoSetAside = "; but the call's recorded answer holds it, an echo keploy cannot answer here: "

// echoLearned is what an echo learned past such places says after naming
// the first.
const echoLearned = ", and keploy learns it as an echo of the call's answer"

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
