package learn

// Rows of the rule a copy follows its source (follows.go), the recording
// being the second observation: record's full round (F) decides alone. From
// the Flyway item's probe: Flyway's start-up INSERT into
// flyway_schema_history binds execution_time, the milliseconds the migration
// took (82 at record, a digit or two in F), whose word is a word of an
// earlier start-up recording's payload that does not drift ("1" of "SELECT
// 1", "16" of "PostgreSQL 16.4").

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// eRecAt is the recorded instant of the INSERT (the lane's recording).
var eRecAt = time.Date(2026, 10, 24, 9, 55, 53, 938177550, time.UTC)

// ePg is a Postgres query recording made before eRecAt, answering rows and
// tag a millisecond after its request: a start-up place whose words the
// application may copy.
func ePg(name string, before time.Duration, sql string, rows []models.PostgresV3Cells, tag string) Raw {
	return ePgAnswered(name, before, eRecAt.Add(-before+time.Millisecond), sql, rows, tag)
}

// ePgAnswered is ePg answered at answered (the zero time: its mock holds no
// answer time).
func ePgAnswered(name string, before time.Duration, answered time.Time, sql string, rows []models.PostgresV3Cells, tag string) Raw {
	return RawOf(&models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: eRecAt.Add(-before),
		ResTimestampMock: answered,
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: sql,
			Response:      &models.PostgresV3Response{Rows: rows, CommandComplete: tag},
		}},
	}}, false)
}

// eStartup is what the application received before the INSERT, at record
// and, served from these recordings in the order they were recorded, in F:
// the JDBC session set-up, HikariCP's validation query, Flyway's server
// probe, the schema-history table, and the migration's own seed row. None of
// it drifts.
func eStartup() []Raw {
	raw := []Raw{
		ePg("mock-0", 900*time.Millisecond, "SET application_name = 'PostgreSQL JDBC Driver'", nil, "SET"),
		ePg("mock-1", 850*time.Millisecond, "SELECT 1", []models.PostgresV3Cells{{{Value: "1"}}}, "SELECT 1"),
		ePg("mock-2", 800*time.Millisecond, "SELECT version()", []models.PostgresV3Cells{{{Value: "PostgreSQL 16.4 (Debian 16.4-1.pgdg120+2) on x86_64-pc-linux-gnu, compiled by gcc (Debian 12.2.0-14) 12.2.0, 64-bit"}}}, "SELECT 1"),
		ePg("mock-3", 300*time.Millisecond, `CREATE TABLE "public"."flyway_schema_history" ("installed_rank" INT NOT NULL, "execution_time" INTEGER NOT NULL)`, nil, "CREATE TABLE"),
		ePg("mock-4", 200*time.Millisecond, "INSERT INTO customer (name, tier) VALUES ('Acme', 'gold')", nil, "INSERT 0 1"),
	}
	for i := range raw {
		raw[i] = raw[i].ServedAs(i + 1).ServedAsRecorded()
	}
	return raw
}

// classifyF is record's full round's verdict on the call, recorded with
// recorded at path and seen with live, under the rule a copy follows its
// source.
func classifyF(t *testing.T, recorded, live string, earlier []Earlier, raw []Raw, resp []Leaf) Learned {
	t.Helper()
	const path = "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: recorded}}}
	got := ClassifyFollowing(NewDriftCache(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: live}}}, rec, eRecAt, earlier, raw, resp, nil, nil)
	if len(got) != 1 {
		t.Fatalf("%q recorded, %q in F: %+v", recorded, live, got)
	}
	return got[0]
}

// with is raw with its i-th place replaced by x.
func with(raw []Raw, i int, x Raw) []Raw {
	out := append([]Raw(nil), raw...)
	out[i] = x
	return out
}

// Flyway's execution_time, 82 at record, a start-up word in F: the recording
// gave the application that word at the same place before the call and its
// value lacked it, so the value is no copy of it, and it is learned as
// varying (noise) in F alone. ClassifyGiven, the rule's absence, still
// refuses it.
func TestEFlywayExecutionTimeBesideStartupWordsIsLearnedVarying(t *testing.T) {
	for _, tc := range []struct{ f, matched string }{
		{"1", `"1" of SELECT 1 and INSERT 0 1`},
		{"16", `"16" of PostgreSQL 16.4`},
		{"4", `"4" of PostgreSQL 16.4`},
		{"0", `"0" of INSERT 0 1`},
		{"64", `"64" of 64-bit`},
	} {
		if got := classifyF(t, "82", tc.f, nil, eStartup(), nil); got.Kind != KindNoise {
			t.Errorf("execution_time 82 at record, %s in F (%s): %s (%q), want noise", tc.f, tc.matched, got.Kind, got.Why)
		}
		path := "bind.$8"
		today := ClassifyGiven(NewDriftCache(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.f}}}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}, eRecAt, nil, eStartup(), nil, nil, nil)
		if today[0].Kind != KindUnaccounted {
			t.Errorf("ClassifyGiven, execution_time 82 at record, %s in F: %s, want refused (the rule is ClassifyFollowing's)", tc.f, today[0].Kind)
		}
	}
}

// eLeaf is an exchange record's full round served before the call, recorded
// at at and answered a millisecond later, served as the n-th serve of the
// set, whose leaf body.retries held recorded at record and was served as
// live.
func eLeaf(at time.Time, n int, recorded, live string) Earlier {
	payload := RawOf(&models.Mock{Name: "mock-8", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond)}}, true)
	return Earlier{Mock: "mock-8", At: at, Payload: payload.ServedAs(n), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.retries", Type: Number, Value: recorded}, Live: live},
	}}
}

// The rule holds for every kind of word the word rule finds: one the value
// F saw holds (found as F gave it) or one the recorded value held (found as
// the recording gave it), at a recording's payload or at a leaf of an
// exchange F served. Each is noise where both runs gave the word there
// before the call and the value of one of them lacks it; and refused, saying
// which condition failed, where the recording did not give it (made at or
// after the call, or the leaf's recorded text lacks it: a real echo, whose
// source moved with it), where F did not (not served, or served drifted),
// where both values hold it, or where keploy cannot read when the recording
// gave it.
func TestACopyFollowsItsSourceForEveryKindOfPlace(t *testing.T) {
	before := eRecAt.Add(-50 * time.Millisecond)
	const (
		noRec   = "the recording did not give the application that word there before the call"
		noF     = "record's full round did not give the application that word there before the call"
		both    = "the recorded value and the value record's full round saw both hold that word"
		untimed = "keploy cannot read when the recording gave the application mock-1 (its mock holds no recorded time)"
	)
	mock1 := func(at time.Time) Raw {
		return RawOf(&models.Mock{Name: "mock-1", Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: "SELECT 1", Response: &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: "1"}}}, CommandComplete: "SELECT 1"},
			}},
		}}, false)
	}
	for _, tc := range []struct {
		name           string
		recorded, live string
		earlier        []Earlier
		raw            []Raw
		want           string // "" noise; else the refusal holds it
		at             string // the place the refusal names
	}{
		{"F's word, at a payload both gave", "82", "1", nil, eStartup(), "", ""},
		{"F's word, at a leaf both gave", "took 82 ms", "took 7 ms", []Earlier{eLeaf(before, 6, "7", "7")}, eStartup(), "", ""},
		{"the recorded word, at a leaf both gave", "took 82 ms", "took 5 ms", []Earlier{eLeaf(before, 6, "82", "82")}, eStartup(), "", ""},
		{"F's word, at the test case's request both gave", "took 82 ms", "took 7 ms", []Earlier{{Mock: givenPlace("test-1"), Given: true, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.retries", Type: Number, Value: "7"}, Live: "7"},
		}}}, eStartup(), "", ""},

		{"F's word, at a payload F served that was recorded after the call", "82", "1", nil, with(eStartup(), 1, mock1(eRecAt.Add(10*time.Millisecond)).ServedAs(2).ServedAsRecorded()), noRec, "mock-1"},
		// Asked before the call but answered at its own instant: the
		// recording cannot show the application had the answer before the
		// call, so it gave the application no word there before it.
		{"F's word, at a payload F served that was answered at the call's own instant", "82", "7", nil, append(eStartup(), ePgAnswered("mock-5", 10*time.Millisecond, eRecAt, "SELECT 7", []models.PostgresV3Cells{{{Value: "7"}}}, "SELECT 1").ServedAs(6).ServedAsRecorded()), noRec, "mock-5"},
		{"F's word, at a leaf whose recorded text lacks it (an echo of what F served)", "82", "16", []Earlier{eLeaf(before, 6, "5150", "16")}, eStartup(), noRec, "mock-8"},
		{"F's word, at a leaf recorded at the call's own instant", "took 82 ms", "took 7 ms", []Earlier{eLeaf(eRecAt, 6, "7", "7")}, eStartup(), noRec, "mock-8"},
		// Recorded after the call, the exchange is no earlier place of the
		// call's own test: only what F served there counts, so the word F
		// served at its leaf is refused there, naming it, before the rule.
		{"F's word, at a leaf recorded after the call", "took 82 ms", "took 7 ms", []Earlier{eLeaf(eRecAt.Add(time.Millisecond), 6, "7", "7")}, eStartup(), "appears in mock-8, a recording record's full round gave the application before the call that was recorded after the call", "mock-8"},
		{"F's word, at a payload F did not serve", "82", "1", nil, with(eStartup(), 1, mock1(eRecAt.Add(-850*time.Millisecond))), noF, "mock-1"},
		{"F's word, at a payload F served changed", "82", "1", nil, with(eStartup(), 1, mock1(eRecAt.Add(-850*time.Millisecond)).ServedAs(2)), noF, "mock-1"},
		{"the recorded word, at a leaf F served drifted", "took 82 ms", "took 5 ms", []Earlier{eLeaf(before, 6, "82", "83")}, eStartup(), noF, "mock-8"},
		{"a word both values hold", "82 1", "4 1 1", nil, eStartup(), both, "mock-1"},
		{"a payload whose recorded time keploy cannot read", "82", "1", nil, with(eStartup(), 1, mock1(time.Time{}).ServedAs(2).ServedAsRecorded()), untimed, "mock-1"},
		{"a leaf whose recorded time keploy cannot read", "took 82 ms", "took 7 ms", []Earlier{eLeaf(time.Time{}, 6, "7", "7")}, eStartup(), "keploy cannot read when the recording gave the application mock-8", "mock-8"},
	} {
		got := classifyF(t, tc.recorded, tc.live, tc.earlier, tc.raw, nil)
		switch {
		case tc.want == "" && got.Kind != KindNoise:
			t.Errorf("%s: %q recorded, %q in F: %s (%q), want noise", tc.name, tc.recorded, tc.live, got.Kind, got.Why)
		case tc.want != "" && (got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.want) || !strings.Contains(got.Why, "in "+tc.at)):
			t.Errorf("FALSE GREEN? %s: %q recorded, %q in F: %s (%q), want refused naming %s: %q", tc.name, tc.recorded, tc.live, got.Kind, got.Why, tc.at, tc.want)
		}
	}
	// A word the recorded value held, at a payload: the recorded value is a
	// copy of that payload's word as far as the recording alone shows. F
	// served the payload unchanged and the value moved, so it is no copy of
	// it (a copy moves with its source): noise, naming the place set aside.
	// Where F did not serve it, or served it changed, it stays a variable
	// there (appearedIn), before the word rule reads it.
	if got := classifyF(t, "1", "82", nil, eStartup(), nil); got.Kind != KindNoise || !strings.Contains(got.Why, "its recorded value stands whole in mock-1 at") || !strings.Contains(got.Why, "a copy moves with its source") {
		t.Errorf("1 recorded beside SELECT 1 F served unchanged, 82 in F: %s (%q), want noise naming mock-1", got.Kind, got.Why)
	}
	for name, raw := range map[string][]Raw{
		"F did not serve it":  with(eStartup(), 1, mock1(eRecAt.Add(-850*time.Millisecond))),
		"F served it changed": with(eStartup(), 1, mock1(eRecAt.Add(-850*time.Millisecond)).ServedAs(2)),
	} {
		if got := classifyF(t, "1", "82", nil, raw, nil); got.Kind != KindVariable || got.From.Mock != "mock-1" {
			t.Errorf("1 recorded beside SELECT 1, %s, 82 in F: %s (%q), want a variable of mock-1", name, got.Kind, got.Why)
		}
	}
}

// eHistory is Flyway's schema-history table, created 150ms before the
// INSERT (after eStartup's places) and served by F after them: a statement
// the application sent whose literals ("50" of VARCHAR(50), "200") stand
// whole in it, served as the n-th serve.
func eHistory(at time.Time, n int, asRecorded bool) Raw {
	x := ePgAnswered("mock-5", eRecAt.Sub(at), at.Add(time.Millisecond), `CREATE TABLE "public"."flyway_schema_history" ("installed_rank" INT NOT NULL, "version" VARCHAR(50), "description" VARCHAR(200) NOT NULL)`, nil, "CREATE TABLE")
	if n == 0 {
		return x
	}
	x = x.ServedAs(n)
	if asRecorded {
		x = x.ServedAsRecorded()
	}
	return x
}

// Flyway's execution_time recorded as 50, a literal of the history table's
// CREATE TABLE the application sent before (VARCHAR(50)): the recording
// alone reads it a copy of that literal (a variable of mock-5). F sent the
// statement again byte for byte, its "50" unchanged, and bound 7: the value
// did not move with what it would be a copy of, so it is no copy (a copy
// follows its source, coordinator 21:20), and it is learned as varying,
// naming the reading set aside. The reading stands (a variable of mock-5)
// where F did not observe the source (not served, served changed), where
// the source holds F's value too (200), where the recording did not give
// the application the source before the call; and the noise is refused
// where F served the places in another order than the recording gave them.
func TestAVariableReadingFDidNotFollowIsNoise(t *testing.T) {
	before := eRecAt.Add(-150 * time.Millisecond)
	startup := func(x Raw) []Raw { return append(eStartup(), x) }
	got := classifyF(t, "50", "7", nil, startup(eHistory(before, 6, true)), nil)
	if got.Kind != KindNoise || !strings.Contains(got.Why, "its recorded value stands whole in mock-5 at") || !strings.Contains(got.Why, "which record's full round sent and gave the application as recorded, byte for byte, while the value changed: a copy moves with its source, so the value is no copy of it, and keploy learns it as varying") {
		t.Errorf("50 recorded beside VARCHAR(50) F sent unchanged, 7 in F: %s (%q), want noise naming mock-5", got.Kind, got.Why)
	}
	if today := ClassifyGiven(NewDriftCache(), []string{"bind.$8"}, Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Type: String, Value: "7"}}}, Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Type: String, Value: "50"}}}, eRecAt, nil, startup(eHistory(before, 6, true)), nil, nil, nil); today[0].Kind != KindVariable || today[0].From.Mock != "mock-5" {
		t.Errorf("ClassifyGiven, 50 beside VARCHAR(50), 7 in F: %s (%q), want a variable of mock-5 (the rule is ClassifyFollowing's)", today[0].Kind, today[0].Why)
	}
	for _, tc := range []struct {
		name, live string
		raw        []Raw
	}{
		{"F did not serve the CREATE TABLE", "7", startup(eHistory(before, 0, false))},
		{"F served the CREATE TABLE changed", "7", startup(eHistory(before, 6, false))},
		{"the CREATE TABLE holds F's value too", "200", startup(eHistory(before, 6, true))},
		{"the CREATE TABLE was recorded after the call", "7", startup(eHistory(eRecAt.Add(10*time.Millisecond), 6, true))},
		{"the CREATE TABLE was answered at the call's own instant", "7", startup(eHistory(eRecAt.Add(-time.Millisecond), 6, true))},
	} {
		if got := classifyF(t, "50", tc.live, nil, tc.raw, nil); got.Kind != KindVariable || got.From.Mock != "mock-5" {
			t.Errorf("FALSE GREEN? 50 beside VARCHAR(50), %s, %s in F: %s (%q), want a variable of mock-5", tc.name, tc.live, got.Kind, got.Why)
		}
	}
	// The call's recorded answer echoes the value: an echo beside a source
	// set aside is refused, as one beside a word the rule let go is.
	if got := classifyF(t, "50", "7", nil, startup(eHistory(before, 6, true)), []Leaf{{Path: "rows[0][0]", Type: String, Value: "50"}}); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-5 at") {
		t.Errorf("FALSE GREEN? 50 beside VARCHAR(50) F sent unchanged, echoed in the answer, 7 in F: %s (%q), want refused naming mock-5", got.Kind, got.Why)
	}
	// F served the CREATE TABLE before Flyway's seed row (mock-4), which the
	// recording gave the application first.
	raw := startup(eHistory(before, 6, true))
	raw[4] = raw[4].ServedAs(7).ServedAsRecorded()
	got = classifyF(t, "50", "7", nil, raw, nil)
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-5 at") || !strings.Contains(got.Why, "in a different order") {
		t.Errorf("FALSE GREEN? 50 beside VARCHAR(50) F served out of the recording's order, 7 in F: %s (%q), want refused for the order, naming mock-5", got.Kind, got.Why)
	}
}

// An echo (the call's recorded answer holds the recorded value) beside a
// word the word rule let go is refused, naming the word's place, as
// ClassifyGiven refuses it.
func TestAnEchoBesideAWordFoundEarlierIsRefused(t *testing.T) {
	resp := []Leaf{{Path: "rows[0][0]", Type: String, Value: "82"}}
	got := classifyF(t, "82", "1", nil, eStartup(), resp)
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "appears in an earlier recording, in mock-1") {
		t.Errorf("82 echoed in the answer, 1 in F beside SELECT 1: %s (%q), want refused naming mock-1", got.Kind, got.Why)
	}
}

// eAnswer is a start-up recording made before eRecAt that answers one row,
// the word word: a place whose word the application may copy.
func eAnswer(name string, before time.Duration, word string) Raw {
	return ePg(name, before, "SELECT next_slot FROM "+name, []models.PostgresV3Cells{{{Value: word}}}, "SELECT 1")
}

// An application that copies the last answer it got takes another place's
// word where F gave it the same answers in another order than the recording
// did. mock-a answers 7 and mock-b 9; the recording gave a then b and
// recorded "slot 82", F saw "slot 7": the recording shows the value no copy
// of a's 7 only where F gave a at the place the recording did. Noise only
// where the recording and F gave the same places in the same order;
// otherwise refused, saying why. Each refused subcase is noise without the
// order.
func TestARoundThatGaveTheEarlierAnswersInAnotherOrderThanTheRecordingRefuses(t *testing.T) {
	a := eAnswer("mock-a", 300*time.Millisecond, "7")
	b := eAnswer("mock-b", 200*time.Millisecond, "9")
	c := eAnswer("mock-c", 250*time.Millisecond, "x")
	late := eAnswer("mock-d", -100*time.Millisecond, "y")
	together := eAnswer("mock-b", 300*time.Millisecond, "9")
	const tail = ", so keploy cannot tell a copy from a varying value"
	for _, tc := range []struct {
		name string
		raw  []Raw
		want string
	}{
		{"F served b then a", []Raw{a.ServedAs(2).ServedAsRecorded(), b.ServedAs(1).ServedAsRecorded()}, "the recording and record's full round gave the application the earlier answers in a different order (the 1st earlier answer was mock-a in the recording and mock-b in record's full round)" + tail},
		{"F served a, b and a again", []Raw{a.ServedIn(1, 3).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}, "(the recording gave 2 earlier answers, record's full round more)" + tail},
		{"F served a where keploy does not number it", []Raw{a.WithServed(true).ServedAsRecorded(), b.ServedAs(1).ServedAsRecorded()}, "keploy does not know where record's full round served mock-a among the other earlier answers it served" + tail},
		{"F served a twice with a serve keploy cannot place between", []Raw{a.ServedIn(1, 3).ServedAsRecorded(), b.ServedAs(4).ServedAsRecorded()}, "record's full round served mock-a more than once, with a serve between that is no earlier answer's, which keploy cannot place" + tail},
		{"the recording gave a and b at one instant", []Raw{a.ServedAs(1).ServedAsRecorded(), together.ServedAs(2).ServedAsRecorded()}, "the recording gave the application mock-a and mock-b at one instant, so keploy cannot tell the order it gave them in" + tail},
		{"the recording gave c between them, F did not serve it", []Raw{a.ServedAs(1).ServedAsRecorded(), c, b.ServedAs(2).ServedAsRecorded()}, "(the 2nd earlier answer was mock-c in the recording and mock-b in record's full round)" + tail},
		{"F served a recording made after the call", []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded(), late.ServedAs(3).ServedAsRecorded()}, "(the recording gave 2 earlier answers, record's full round more)" + tail},
	} {
		got := classifyF(t, "slot 82", "slot 7", nil, tc.raw, nil)
		if got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.want) || !strings.Contains(got.Why, "appears in an earlier recording, in mock-") {
			t.Errorf("FALSE GREEN? %s, slot 82 at record, slot 7 in F: %s (%q), want refused: %q", tc.name, got.Kind, got.Why, tc.want)
		}
	}
	// The control: F served the places in the order the recording gave
	// them (numbered apart: other serves of the set between them; a mock
	// served twice with the other's serve after both), and the value is a
	// varying one (a copy follows its source).
	for _, tc := range []struct {
		name string
		raw  []Raw
	}{
		{"F served a then b", []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}},
		{"F served a then b, numbered apart", []Raw{a.ServedAs(3).ServedAsRecorded(), b.ServedAs(5).ServedAsRecorded()}},
		{"F served a then b, and an exchange after both", nil},
	} {
		var earlier []Earlier
		raw := tc.raw
		if raw == nil {
			raw = []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}
			earlier = []Earlier{eLeaf(eRecAt.Add(-50*time.Millisecond), 3, "0", "0")}
		}
		if got := classifyF(t, "slot 82", "slot 7", earlier, raw, nil); got.Kind != KindNoise {
			t.Errorf("%s, slot 82 at record, slot 7 in F: %s (%q), want noise", tc.name, got.Kind, got.Why)
		}
	}
}

// eSpan is eAnswer requested before and answered answered before eRecAt.
func eSpan(name string, before, answered time.Duration, word string) Raw {
	return ePgAnswered(name, before, eRecAt.Add(-answered), "SELECT next_slot FROM "+name, []models.PostgresV3Cells{{{Value: word}}}, "SELECT 1")
}

// Two requests in flight together: the recording gave the application
// their answers in the order the dependency and the network gave them,
// which keploy cannot tell from when the requests were recorded. mock-a is
// requested first and answered last, so the application had b's answer
// first. Ordered by request, the recording's order would be F's (a then b),
// and the value learned as varying: refused, saying why, on any two
// connections. On one connection whose protocol answers in the order sent,
// the positions tell the order (a pipelined client); positions that disagree
// with the recorded requests refuse. A recording whose answer time keploy
// cannot read shows nothing, and one still in flight when the application
// made the call gave it nothing before the call (recordedGave).
// Sequential places stay in their time order (the Flyway start-up,
// TestEFlywayExecutionTimeBesideStartupWordsIsLearnedVarying).
func TestTwoRequestsInFlightTogetherAreInAnOrderTheRecordingShowsOnlyOnOneConnection(t *testing.T) {
	a := eSpan("mock-a", 300*time.Millisecond, 100*time.Millisecond, "7")
	b := eSpan("mock-b", 250*time.Millisecond, 200*time.Millisecond, "9")
	const tail = ", so keploy cannot tell a copy from a varying value"
	const inFlight = "the recording's requests for mock-a and mock-b were in flight together (mock-b's was recorded before mock-a was answered), so keploy cannot tell which answer it gave the application first" + tail
	for _, tc := range []struct {
		name string
		raw  []Raw
		want string
	}{
		{"answered in reverse order, F served a then b", []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}, inFlight},
		{"answered in reverse order, F served b then a", []Raw{a.ServedAs(2).ServedAsRecorded(), b.ServedAs(1).ServedAsRecorded()}, inFlight},
		{"a answered at the instant b's request was recorded", []Raw{eSpan("mock-a", 300*time.Millisecond, 250*time.Millisecond, "7").ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}, inFlight},
		{"on one connection at positions that disagree with the requests", []Raw{a.SentOn("0", 2).ServedAs(1).ServedAsRecorded(), b.SentOn("0", 1).ServedAs(2).ServedAsRecorded()},
			"the recording carried mock-a and mock-b on one connection at positions that disagree with when their requests were recorded, so keploy cannot tell the order it gave them in" + tail},
		{"on two connections at positions", []Raw{a.SentOn("0", 1).ServedAs(1).ServedAsRecorded(), b.SentOn("1", 2).ServedAs(2).ServedAsRecorded()}, inFlight},
		{"a's mock holds no answer time", []Raw{ePgAnswered("mock-a", 300*time.Millisecond, time.Time{}, "SELECT next_slot FROM mock-a", []models.PostgresV3Cells{{{Value: "7"}}}, "SELECT 1").ServedAs(1).ServedAsRecorded(), eAnswer("mock-b", 200*time.Millisecond, "9").ServedAs(2).ServedAsRecorded()},
			"keploy cannot read when the recording answered mock-a (its mock holds no answer time it can read), so the recording cannot show the value does not follow that word"},
		{"b's mock holds no answer time", []Raw{a.ServedAs(1).ServedAsRecorded(), ePgAnswered("mock-b", 200*time.Millisecond, time.Time{}, "SELECT next_slot FROM mock-b", []models.PostgresV3Cells{{{Value: "9"}}}, "SELECT 1").ServedAs(2).ServedAsRecorded()},
			"keploy cannot read when the recording answered mock-b (its mock holds no answer time it can read)" + tail},
		{"a in flight when the application made the call", []Raw{eSpan("mock-a", 300*time.Millisecond, -50*time.Millisecond, "7").ServedAs(1).ServedAsRecorded()},
			"the recording did not give the application that word there before the call, so it cannot show the value does not follow it"},
		{"a's answer recorded before its request", []Raw{eSpan("mock-a", 300*time.Millisecond, 400*time.Millisecond, "7").ServedAs(1).ServedAsRecorded(), eAnswer("mock-b", 200*time.Millisecond, "9").ServedAs(2).ServedAsRecorded()},
			"keploy cannot read when the recording answered mock-a"},
	} {
		got := classifyF(t, "slot 82", "slot 7", nil, tc.raw, nil)
		if got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.want) {
			t.Errorf("FALSE GREEN? %s, slot 82 at record, slot 7 in F: %s (%q), want refused: %q", tc.name, got.Kind, got.Why, tc.want)
		}
	}
	// One connection, positions in the order of the requests: the protocol
	// answered a first, as F did. And sequential places: a answered before
	// b's request, whatever the positions say.
	for _, tc := range []struct {
		name string
		raw  []Raw
	}{
		{"pipelined on one connection", []Raw{a.SentOn("0", 1).ServedAs(1).ServedAsRecorded(), b.SentOn("0", 2).ServedAs(2).ServedAsRecorded()}},
		{"sequential", []Raw{eSpan("mock-a", 300*time.Millisecond, 260*time.Millisecond, "7").ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded()}},
		{"sequential on one connection, positions the other way", []Raw{eSpan("mock-a", 300*time.Millisecond, 260*time.Millisecond, "7").SentOn("0", 2).ServedAs(1).ServedAsRecorded(), b.SentOn("0", 1).ServedAs(2).ServedAsRecorded()}},
	} {
		if got := classifyF(t, "slot 82", "slot 7", nil, tc.raw, nil); got.Kind != KindNoise {
			t.Errorf("%s, slot 82 at record, slot 7 in F: %s (%q), want noise", tc.name, got.Kind, got.Why)
		}
	}
}

// From the Flyway item (lane flyway-e1-on3): pgjdbc sends a block's BEGIN in
// one write with the statement after it (SET ROLE), and the Postgres
// recorder samples one request time per decoded client bundle, so both
// mocks carry one recorded instant; the recorder keeps their order on the
// connection (the invocation id's sequence), which its adapter hands as the
// places' positions (Raw.SentOn). Places of different mocks recorded at one
// instant are in the order their client sent them where both were carried
// on one connection at known, distinct positions; otherwise the order stays
// untold and the value is refused. mock-9 and mock-10's names sort the
// other way, so no order by name passes. The same holds for an exchange
// served leaf by leaf (Earlier.Payload's position).
func TestPlacesSentInOneWriteAreInTheOrderTheirConnectionCarriedThem(t *testing.T) {
	first := eAnswer("mock-9", 300*time.Millisecond, "7").SentOn("0", 9)
	second := eAnswer("mock-10", 300*time.Millisecond, "9").SentOn("0", 10)
	if got := classifyF(t, "slot 82", "slot 7", nil, []Raw{first.ServedAs(1).ServedAsRecorded(), second.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindNoise {
		t.Errorf("one write on one connection, F served it in the connection's order: %s (%q), want noise", got.Kind, got.Why)
	}
	const tail = ", so keploy cannot tell a copy from a varying value"
	const tie = "at one instant, so keploy cannot tell the order it gave them in" + tail
	for _, tc := range []struct {
		name string
		raw  []Raw
		want string
	}{
		{"F served them against the connection's order", []Raw{first.ServedAs(2).ServedAsRecorded(), second.ServedAs(1).ServedAsRecorded()},
			"the recording and record's full round gave the application the earlier answers in a different order (the 1st earlier answer was mock-9 in the recording and mock-10 in record's full round)" + tail},
		{"one instant on two connections", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("1", 10).ServedAs(2).ServedAsRecorded()}, tie},
		{"one instant, a position unknown", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("0", 0).ServedAs(2).ServedAsRecorded()}, tie},
		{"one instant, no connection", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("", 10).ServedAs(2).ServedAsRecorded()}, tie},
		{"one instant, one position twice", []Raw{first.ServedAs(1).ServedAsRecorded(), second.SentOn("0", 9).ServedAs(2).ServedAsRecorded()}, tie},
	} {
		got := classifyF(t, "slot 82", "slot 7", nil, tc.raw, nil)
		if got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.want) {
			t.Errorf("FALSE GREEN? %s: %s (%q), want refused: %q", tc.name, got.Kind, got.Why, tc.want)
		}
	}
	// Three in one write, handed out of order: told by position, all three.
	third := eAnswer("mock-11", 300*time.Millisecond, "x").SentOn("0", 11)
	if got := classifyF(t, "slot 82", "slot 7", nil, []Raw{third.ServedAs(3).ServedAsRecorded(), first.ServedAs(1).ServedAsRecorded(), second.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindNoise {
		t.Errorf("three in one write, F served them in the connection's order: %s (%q), want noise", got.Kind, got.Why)
	}
	// Distinct instants keep their time order whatever the positions say:
	// positions order only requests in flight together.
	early := eAnswer("mock-a", 400*time.Millisecond, "7").SentOn("0", 20)
	if got := classifyF(t, "slot 82", "slot 7", nil, []Raw{early.ServedAs(1).ServedAsRecorded(), second.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindNoise {
		t.Errorf("distinct instants: %s (%q), want noise by time order", got.Kind, got.Why)
	}
	// An exchange served leaf by leaf, in one write with a recording: its
	// payload's position tells it on the recording's connection; on another
	// it is refused. And one answered before a later request is ordered by
	// time: its payload's answer time is read.
	at := eRecAt.Add(-300 * time.Millisecond)
	leaf := func(conn string, pos uint64, answered time.Time) []Earlier {
		e := eLeaf(at, 1, "7", "7")
		e.Payload = RawOf(&models.Mock{Name: "mock-8", Spec: models.MockSpec{ReqTimestampMock: at, ResTimestampMock: answered}}, true).SentOn(conn, pos).ServedAs(1)
		return []Earlier{e}
	}
	if got := classifyF(t, "took 82 ms", "took 7 ms", leaf("0", 9, at.Add(time.Millisecond)), []Raw{second.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindNoise {
		t.Errorf("an exchange in one write with mock-10, on its connection: %s (%q), want noise", got.Kind, got.Why)
	}
	if got := classifyF(t, "took 82 ms", "took 7 ms", leaf("1", 9, at.Add(time.Millisecond)), []Raw{second.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "and mock-8 at one instant, so keploy cannot tell the order it gave them in") {
		t.Errorf("FALSE GREEN? an exchange in one write with mock-10, on another connection: %s (%q), want refused", got.Kind, got.Why)
	}
	later := eAnswer("mock-12", 200*time.Millisecond, "9")
	if got := classifyF(t, "took 82 ms", "took 7 ms", leaf("", 0, at.Add(time.Millisecond)), []Raw{later.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindNoise {
		t.Errorf("an exchange answered before a later request: %s (%q), want noise", got.Kind, got.Why)
	}
	if got := classifyF(t, "took 82 ms", "took 7 ms", leaf("", 0, time.Time{}), []Raw{later.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "keploy cannot read when the recording answered mock-8") {
		t.Errorf("FALSE GREEN? an exchange whose answer time keploy cannot read: %s (%q), want refused", got.Kind, got.Why)
	}
	if got := classifyF(t, "took 82 ms", "took 7 ms", leaf("", 0, eRecAt), []Raw{later.ServedAs(2).ServedAsRecorded()}, nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "the recording did not give the application that word there before the call") {
		t.Errorf("FALSE GREEN? an exchange answered at the call's own instant: %s (%q), want refused", got.Kind, got.Why)
	}
}
