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
// tag: a start-up place whose words the application may copy.
func ePg(name string, before time.Duration, sql string, rows []models.PostgresV3Cells, tag string) Raw {
	return RawOf(&models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: eRecAt.Add(-before),
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
		raw[i] = raw[i].ServedAs(i + 1)
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
// at at, served as the n-th serve of the set, whose leaf body.retries held
// recorded at record and was served as live.
func eLeaf(at time.Time, n int, recorded, live string) Earlier {
	return Earlier{Mock: "mock-8", At: at, Payload: payloadOf("mock-8", at).ServedAs(n), Seen: []Seen{
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
			ReqTimestampMock: at,
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

		{"F's word, at a payload F served that was recorded after the call", "82", "1", nil, with(eStartup(), 1, mock1(eRecAt.Add(10*time.Millisecond)).ServedAs(2)), noRec, "mock-1"},
		{"F's word, at a leaf whose recorded text lacks it (an echo of what F served)", "82", "16", []Earlier{eLeaf(before, 6, "5150", "16")}, eStartup(), noRec, "mock-8"},
		{"F's word, at a leaf recorded at the call's own instant", "took 82 ms", "took 7 ms", []Earlier{eLeaf(eRecAt, 6, "7", "7")}, eStartup(), noRec, "mock-8"},
		{"F's word, at a leaf recorded after the call", "took 82 ms", "took 7 ms", []Earlier{eLeaf(eRecAt.Add(time.Millisecond), 6, "7", "7")}, eStartup(), noRec, "mock-8"},
		{"F's word, at a payload F did not serve", "82", "1", nil, with(eStartup(), 1, mock1(eRecAt.Add(-850*time.Millisecond))), noF, "mock-1"},
		{"the recorded word, at a leaf F served drifted", "took 82 ms", "took 5 ms", []Earlier{eLeaf(before, 6, "82", "83")}, eStartup(), noF, "mock-8"},
		{"a word both values hold", "82 1", "4 1 1", nil, eStartup(), both, "mock-1"},
		{"a payload whose recorded time keploy cannot read", "82", "1", nil, with(eStartup(), 1, mock1(time.Time{}).ServedAs(2)), untimed, "mock-1"},
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
	// copy of that payload's word as far as the recording shows, and is
	// bound as a variable there (appearedIn) before the word rule reads it,
	// whether F served the payload or not. The rule never makes it noise.
	for _, raw := range [][]Raw{eStartup(), with(eStartup(), 1, mock1(eRecAt.Add(-850*time.Millisecond)))} {
		if got := classifyF(t, "1", "82", nil, raw, nil); got.Kind != KindVariable {
			t.Errorf("1 recorded beside SELECT 1, 82 in F: %s (%q), want a variable of mock-1", got.Kind, got.Why)
		}
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
		{"F served b then a", []Raw{a.ServedAs(2), b.ServedAs(1)}, "the recording and record's full round gave the application the earlier answers in a different order (the 1st earlier answer was mock-a in the recording and mock-b in record's full round)" + tail},
		{"F served a, b and a again", []Raw{a.ServedIn(1, 3), b.ServedAs(2)}, "(the recording gave 2 earlier answers, record's full round more)" + tail},
		{"F served a where keploy does not number it", []Raw{a.WithServed(true), b.ServedAs(1)}, "keploy does not know where record's full round served mock-a among the other earlier answers it served" + tail},
		{"F served a twice with a serve keploy cannot place between", []Raw{a.ServedIn(1, 3), b.ServedAs(4)}, "record's full round served mock-a more than once, with a serve between that is no earlier answer's, which keploy cannot place" + tail},
		{"the recording gave a and b at one instant", []Raw{a.ServedAs(1), together.ServedAs(2)}, "the recording gave the application mock-a and mock-b at one instant, so keploy cannot tell the order it gave them in" + tail},
		{"the recording gave c between them, F did not serve it", []Raw{a.ServedAs(1), c, b.ServedAs(2)}, "(the 2nd earlier answer was mock-c in the recording and mock-b in record's full round)" + tail},
		{"F served a recording made after the call", []Raw{a.ServedAs(1), b.ServedAs(2), late.ServedAs(3)}, "(the recording gave 2 earlier answers, record's full round more)" + tail},
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
		{"F served a then b", []Raw{a.ServedAs(1), b.ServedAs(2)}},
		{"F served a then b, numbered apart", []Raw{a.ServedAs(3), b.ServedAs(5)}},
		{"F served a then b, and an exchange after both", nil},
	} {
		var earlier []Earlier
		raw := tc.raw
		if raw == nil {
			raw = []Raw{a.ServedAs(1), b.ServedAs(2)}
			earlier = []Earlier{eLeaf(eRecAt.Add(-50*time.Millisecond), 3, "0", "0")}
		}
		if got := classifyF(t, "slot 82", "slot 7", earlier, raw, nil); got.Kind != KindNoise {
			t.Errorf("%s, slot 82 at record, slot 7 in F: %s (%q), want noise", tc.name, got.Kind, got.Why)
		}
	}
}
