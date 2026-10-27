package learn

// Rows of the rule a copy follows its source (follows.go), the recording
// being the second observation: record's full round (F) decides alone. From
// the Flyway item's probe: Flyway's start-up INSERT into
// flyway_schema_history binds execution_time, the milliseconds the migration
// took (82 at record, a digit or two in F), whose word is a word of an
// earlier start-up recording's payload that does not drift ("1" of "SELECT
// 1", "16" of "PostgreSQL 16.4").

import (
	"strconv"
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
	got := ClassifyFollowing(clockedDrifts(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: live}}}, rec, eRecAt, earlier, raw, resp, nil, nil)
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
		today := ClassifyGiven(clockedDrifts(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.f}}}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}, eRecAt, nil, eStartup(), nil, nil, nil)
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
// source moved with it), where F did not (not served, or served changed with
// no view of it, or at a leaf served without it and nothing new),
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
		// F gave the application 83 at the leaf, and the value took none
		// of it: a copy moves with its source, so it is no copy of 82 there
		// (TestAWordAtAPlaceTheFullRoundGaveAnotherValueAtIsNoCopyWhereTheValueTookNoneOfIt).
		{"the recorded word, at a leaf F served drifted", "took 82 ms", "took 5 ms", []Earlier{eLeaf(before, 6, "82", "83")}, eStartup(), "", ""},
		{"the recorded word, at a leaf F served without it and nothing new", "took 82 ms", "took 5 ms", []Earlier{eLeaf(before, 6, "82 7", "7")}, eStartup(), "record's full round gave the application another value there, which holds no word the recorded one lacked", "mock-8"},
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

// The random ids of the chance-word case: an earlier answer's job id, as
// recorded and as record's full round gave it, and the call's own random
// value, which holds "48d1" of the job's recorded id by chance.
const (
	chanceSrcRec  = "bb7ed9ee-670e-48d1-982f-b54b2b73982f"
	chanceSrcLive = "0c1d2e3f-4a5b-6c7d-8e9f-a0b1c2d3e4f5"
	chanceValRec  = "1111aaaa-2222-3333-48d1-444455556666"
	chanceValLive = "9999cccc-7777-8888-dddd-000011112222"
	// chanceCopy took "6c7d", the word F gave in place of "48d1".
	chanceCopy = "9999cccc-7777-8888-6c7d-000011112222"
)

// eJobs is a job's row, id and n, answered to the application 100ms before
// eRecAt (after eStartup's places), as recorded, or as record's full round
// gave it (the view ServedLive takes).
func eJobs(id, n string) Raw {
	return ePg("mock-6", 100*time.Millisecond, "SELECT id, n FROM jobs", []models.PostgresV3Cells{{{Value: id}, {Value: n}}}, "SELECT 1")
}

// eRow is one row of three columns, answered as eJobs is.
func eRow(a, b, c string) Raw {
	return eRows([]models.PostgresV3Cells{{{Value: a}, {Value: b}, {Value: c}}})
}

// eRows is rows, answered as eJobs is.
func eRows(rows []models.PostgresV3Cells) Raw {
	return ePg("mock-6", 100*time.Millisecond, "SELECT a, b, c FROM t", rows, "SELECT 1")
}

// jobsServed is eJobs(id, n) served by F as the 6th serve, after eStartup's,
// changed, with F's view of it (live; the zero Raw: none).
func jobsServed(id, n string, live Raw) []Raw {
	return append(eStartup(), eJobs(id, n).ServedAs(6).ServedLive(live))
}

// A word of the value at a place F gave the application another value at
// (a leaf F served drifted, or a field of a payload F served changed and
// gave its view of: Raw.ServedLive) is no word the value copied there where
// the value took none of the words the place gained, in any form: a copy
// moves with its source. The call's random value held "48d1" of an earlier
// answer's random id by chance; F gave the application another id there,
// and the value took none of it: noise, naming the place set aside. Where
// the value took a word of what F gave there (a copy), or the place
// gained nothing, or F's view of it is missing, lacks the field, or holds
// the value F saw, it stays where the value came from (a variable), or the
// word is refused: fail closed. ClassifyGiven, the rule's absence, never
// learns it.
func TestAWordAtAPlaceTheFullRoundGaveAnotherValueAtIsNoCopyWhereTheValueTookNoneOfIt(t *testing.T) {
	const (
		moved   = "where record's full round gave the application another value, which the value took none of the words of: a copy moves with its source"
		took    = "record's full round gave the application another value there, and the value took a word of it"
		nothing = "record's full round gave the application another value there, which holds no word the recorded one lacked"
		holds   = "record's full round gave the application another value there, and it holds a word the value took"
		other   = ", where the recording did not, so it cannot show the value does not follow it"
		fOnly   = "where the recording did not give the application that word"
	)
	before := eRecAt.Add(-50 * time.Millisecond)
	leaf := func(rec, live string) []Earlier {
		return []Earlier{{Mock: "mock-8", At: before, Payload: eLeaf(before, 6, "", "").Payload, Seen: []Seen{
			{Side: Request, Leaf: Leaf{Path: "body.cmd.documents.0._id", Type: String, Value: rec}, Live: live},
		}}}
	}
	notServed := append(eStartup(), eJobs(chanceSrcRec, "5").ServedLive(eJobs(chanceSrcLive, "6")))
	unserved := append(eStartup(), eJobs(chanceSrcRec, "5").ServedAs(6).ServedLive(eJobs(chanceSrcLive, "6")).WithServed(false))
	for _, tc := range []struct {
		name           string
		recorded, live string
		earlier        []Earlier
		raw            []Raw
		kind           Kind
		why            string // what the reason holds ("" for a variable: the place it names)
		from           string // a variable's place
	}{
		{"payload, F's view, the value took none of what F gave", chanceValRec, chanceValLive, nil, jobsServed(chanceSrcRec, "5", eJobs(chanceSrcLive, "6")), KindNoise, "(its characters 20 to 23) stands in mock-6 at spec.postgresV3.query.response.rows[0][0], " + moved, ""},
		{"payload, F's view, the value took the word F gave there (a copy)", chanceValRec, chanceCopy, nil, jobsServed(chanceSrcRec, "5", eJobs(chanceSrcLive, "6")), KindVariable, "", "mock-6"},
		{"payload, F's view, F gave that word as recorded there", chanceValRec, chanceValLive, nil, jobsServed(chanceSrcRec, "5", eJobs(chanceSrcRec, "6")), KindNoise, "where record's full round gave the application that word as recorded, while the value changed", ""},
		{"payload, F's view holds the value F saw", chanceValRec, chanceValLive, nil, jobsServed(chanceSrcRec, "5", eJobs(chanceSrcLive, chanceValLive)), KindVariable, "", "mock-6"},
		{"payload, F's view lacks the field", chanceValRec, chanceValLive, nil, jobsServed(chanceSrcRec, "5", ePg("mock-6", 100*time.Millisecond, "SELECT id, n FROM jobs", nil, "SELECT 0")), KindVariable, "", "mock-6"},
		{"payload, served changed with no view", chanceValRec, chanceValLive, nil, jobsServed(chanceSrcRec, "5", Raw{}), KindVariable, "", "mock-6"},
		{"payload, not served, a view handed anyway", chanceValRec, chanceValLive, nil, notServed, KindVariable, "", "mock-6"},
		{"payload, a view dropped as not served", chanceValRec, chanceValLive, nil, unserved, KindVariable, "", "mock-6"},
		// The value whole at a field F gave another value at: a whole value
		// is followed (sources), as at a leaf.
		{"payload, F's view, the recorded value whole there", chanceSrcRec, chanceValLive, nil, jobsServed(chanceSrcRec, "5", eJobs(chanceSrcLive, "6")), KindVariable, "", "mock-6"},
		// 48d1 at the id F moved, which the value took none of; 77 at the
		// count F gave 78 at, which the value took: the search goes past
		// the first to the second, a place.
		{"payload, F's view, one word moved away from, one followed", "48d1 77", "dddd 78", nil, jobsServed(chanceSrcRec, "77", eJobs(chanceSrcLive, "78")), KindVariable, "", "mock-6"},
		// A word the value holds now that only F's view holds there: F gave
		// it, the recording did not.
		{"payload, F's view alone holds the word the value holds now", "82", "6c7d", nil, jobsServed(chanceSrcRec, "5", eJobs(chanceSrcLive, "5")), KindUnaccounted, "appears in an earlier recording, in mock-6 at spec.postgresV3.query.response.rows[0][0], as record's full round gave it, " + fOnly, ""},
		// F gave the rows in another order: the id's field holds the other
		// job's there, and the value took the word F gave at the row the
		// recorded id moved to, which the recording did not give.
		{"payload, F's view moved the row, the value took its word", chanceValRec, chanceCopy, nil, append(eStartup(), ePg("mock-6", 100*time.Millisecond, "SELECT id FROM jobs", []models.PostgresV3Cells{{{Value: chanceSrcRec}}, {{Value: "c0ffee00-1234-4321-abcd-0000000000ff"}}}, "SELECT 2").ServedAs(6).ServedLive(ePg("mock-6", 100*time.Millisecond, "SELECT id FROM jobs", []models.PostgresV3Cells{{{Value: "c0ffee00-1234-4321-abcd-0000000000ff"}}, {{Value: chanceSrcLive}}}, "SELECT 2"))), KindUnaccounted, "appears in an earlier recording, in mock-6 at spec.postgresV3.query.response.rows[1][0], as record's full round gave it, " + fOnly, ""},
		// F gave the word the value took at another field than the one
		// whose recorded text holds it, or at a field the recording lacks:
		// what F gave there is read field by field, never as moved.
		{"payload, F's view moved the word the value took to another column", "k-48d1", "k-6c7d", nil, append(eStartup(), eRow("48d1", "6c7d", "aaaa").ServedAs(6).ServedLive(eRow("9998", "9999", "6c7d"))), KindUnaccounted, "record's full round gave the application that word at spec.postgresV3.query.response.rows[0][2]" + other, ""},
		{"payload, F's view gave the word the value took at two columns", "k-48d1", "k-6c7d", nil, append(eStartup(), eRow("48d1", "6c7d", "aaaa").ServedAs(6).ServedLive(eRow("9999", "6c7d", "6c7d"))), KindUnaccounted, "record's full round gave the application that word at spec.postgresV3.query.response.rows[0][2]" + other, ""},
		{"payload, F's view gave the word the value took at a column besides the one both gave it at", "k-zzzz", "k-6c7d", nil, append(eStartup(), eRow("qqqq", "6c7d", "aaaa").ServedAs(6).ServedLive(eRow("qqqq", "6c7d", "6c7d"))), KindUnaccounted, "record's full round gave the application that word at spec.postgresV3.query.response.rows[0][2]" + other, ""},
		{"payload, F's view grew a row holding the word the value took", "k-48d1", "k-6c7d", nil, append(eStartup(), eRows([]models.PostgresV3Cells{{{Value: "48d1"}, {Value: "6c7d"}}}).ServedAs(6).ServedLive(eRows([]models.PostgresV3Cells{{{Value: "9999"}, {Value: "6c7d"}}, {{Value: "6c7d"}, {Value: "1"}}}))), KindUnaccounted, "record's full round gave the application that word at spec.postgresV3.query.response.rows[1][0]" + other, ""},
		// A view of another mock is no view of this one.
		{"payload, a view of another mock", chanceValRec, chanceCopy, nil, jobsServed(chanceSrcRec, "5", ePg("mock-7", 100*time.Millisecond, "SELECT name FROM users", []models.PostgresV3Cells{{{Value: "zzzz"}, {Value: "5"}}}, "SELECT 1")), KindVariable, "", "mock-6"},
		// F gave 48d1's place a longer word that holds what the value took.
		{"payload, F's view, the value took a part of a longer word F gave", "k-48d1", "k-6c7d", nil, append(eStartup(), eRow("48d1 aaaa", "x", "y").ServedAs(6).ServedLive(eRow("6c7daaaa", "x", "y"))), KindVariable, "", "mock-6"},
		{"leaf, the value took a part of a longer word F gave", "k-48d1", "k-6c7d", leaf("48d1 aaaa", "6c7daaaa"), eStartup(), KindUnaccounted, holds, ""},
		{"leaf, the value took a word F gave inside a longer word", "k-48d1", "k-x6c7dx", leaf("48d1", "6c7d"), eStartup(), KindUnaccounted, took, ""},
		{"leaf, the value took none of what F gave", chanceValRec, chanceValLive, leaf(chanceSrcRec, chanceSrcLive), eStartup(), KindNoise, "a word of its recorded value (its characters 20 to 23) stands in mock-8 at body.cmd.documents.0._id, " + moved, ""},
		{"leaf, the value took the word F gave there (a copy)", chanceValRec, chanceCopy, leaf(chanceSrcRec, chanceSrcLive), eStartup(), KindVariable, "", "mock-8"},
		{"leaf, the value holds a word F gave there it held before", "82 b", "5 b", leaf("82", "b"), eStartup(), KindUnaccounted, took, ""},
		{"leaf, F gave nothing new there", "took 82 ms", "took 5 ms", leaf("82 7", "7"), eStartup(), KindUnaccounted, nothing, ""},
	} {
		got := classifyF(t, tc.recorded, tc.live, tc.earlier, tc.raw, nil)
		switch {
		case got.Kind != tc.kind:
			t.Errorf("FALSE GREEN? %s: %q recorded, %q in F: %s (%q), want %s", tc.name, tc.recorded, tc.live, got.Kind, got.Why, tc.kind)
		case tc.kind == KindVariable && got.From.Mock != tc.from:
			t.Errorf("%s: a variable of %q (%+v), want of %s", tc.name, got.From.Mock, got.From, tc.from)
		case tc.why != "" && !strings.Contains(got.Why, tc.why):
			t.Errorf("%s: %s, why %q, want it to hold %q", tc.name, got.Kind, got.Why, tc.why)
		}
		if tc.kind != KindNoise {
			continue
		}
		path := "bind.$8"
		given := ClassifyGiven(clockedDrifts(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.live}}}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.recorded}}}, eRecAt, tc.earlier, tc.raw, nil, nil, nil)
		if given[0].Kind == KindNoise {
			t.Errorf("FALSE GREEN? ClassifyGiven, %s: noise (%q), want it never learned (the rule is ClassifyFollowing's)", tc.name, given[0].Why)
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
// the source holds F's value too (200, or a%2Fb in its decoded form, a/b),
// where the recording did not give
// the application the source before the call; and the noise is refused
// where F served the places in another order than the recording gave them.
func TestAVariableReadingFDidNotFollowIsNoise(t *testing.T) {
	before := eRecAt.Add(-150 * time.Millisecond)
	startup := func(x Raw) []Raw { return append(eStartup(), x) }
	got := classifyF(t, "50", "7", nil, startup(eHistory(before, 6, true)), nil)
	if got.Kind != KindNoise || !strings.Contains(got.Why, "its recorded value stands whole in mock-5 at") || !strings.Contains(got.Why, "which record's full round sent and gave the application as recorded, byte for byte, while the value changed: a copy moves with its source, so the value is no copy of it, and keploy learns it as varying") {
		t.Errorf("50 recorded beside VARCHAR(50) F sent unchanged, 7 in F: %s (%q), want noise naming mock-5", got.Kind, got.Why)
	}
	if today := ClassifyGiven(clockedDrifts(), []string{"bind.$8"}, Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Type: String, Value: "7"}}}, Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Type: String, Value: "50"}}}, eRecAt, nil, startup(eHistory(before, 6, true)), nil, nil, nil); today[0].Kind != KindVariable || today[0].From.Mock != "mock-5" {
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
	// A source F served as recorded holding F's value in the form a bind
	// with an escape decodes to (F's a%2Fb, a/b among its words) holds F's
	// value too: the value may have come from it, and the reading stands.
	src := ePg("mock-s", 150*time.Millisecond, "SELECT a, b FROM t", []models.PostgresV3Cells{{{Value: "50"}, {Value: "x a/b y"}}}, "SELECT 1").ServedAs(6).ServedAsRecorded()
	for _, live := range []string{"a/b", "a%2Fb"} {
		if got := classifyF(t, "50", live, nil, startup(src), nil); got.Kind != KindVariable || got.From.Mock != "mock-s" {
			t.Errorf("FALSE GREEN? 50 in mock-s, which holds F's value %s (a/b, decoded), %s in F: %s (%q), want a variable of mock-s", live, live, got.Kind, got.Why)
		}
	}
	// The call's recorded answer echoes the value: an echo beside a source
	// set aside is refused, as one beside a word the rule let go is.
	if got := classifyF(t, "50", "7", nil, startup(eHistory(before, 6, true)), []Leaf{{Path: "rows[0][0]", Type: String, Value: "50"}}); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-5 at") {
		t.Errorf("FALSE GREEN? 50 beside VARCHAR(50) F sent unchanged, echoed in the answer, 7 in F: %s (%q), want refused naming mock-5", got.Kind, got.Why)
	}
	// F served the CREATE TABLE before Flyway's seed row (mock-4), which the
	// recording gave the application first. The seed row holds no word of
	// either value, so it is no source of it and its order is not read.
	raw := startup(eHistory(before, 6, true))
	raw[4] = raw[4].ServedAs(7).ServedAsRecorded()
	if got := classifyF(t, "50", "7", nil, raw, nil); got.Kind != KindNoise {
		t.Errorf("50 beside VARCHAR(50), F served a place holding no word of the value out of the recording's order, 7 in F: %s (%q), want noise", got.Kind, got.Why)
	}
	// A place answering 7, F's value, recorded after the CREATE TABLE: a
	// second candidate source, which F served before it. Refused for the
	// order; in the recording's order, noise.
	seven := eAnswer("mock-a", 100*time.Millisecond, "7")
	got = classifyF(t, "50", "7", nil, append(startup(eHistory(before, 6, true)), seven.ServedAs(5).ServedAsRecorded()), nil)
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in a different order (the 1st earlier answer was mock-5 in the recording and mock-a in record's full round)") {
		t.Errorf("FALSE GREEN? 50 beside VARCHAR(50) and a 7 F served out of the recording's order, 7 in F: %s (%q), want refused for the order", got.Kind, got.Why)
	}
	if got := classifyF(t, "50", "7", nil, append(startup(eHistory(before, 6, true)), seven.ServedAs(8).ServedAsRecorded()), nil); got.Kind != KindNoise {
		t.Errorf("50 beside VARCHAR(50) and a 7 F served in the recording's order, 7 in F: %s (%q), want noise", got.Kind, got.Why)
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

// An application that copies an answer by its position takes another
// place's word where F gave it the same answers in another order than the
// recording did. mock-a answers 7 and mock-b 82; the recording gave a then b
// and recorded "slot 82", F saw "slot 7": an application that copies the
// last answer's number took b's 82 at record and, had F given b then a, a's
// 7 in F. Both are candidate sources (each holds a word of a value). Noise
// only where the recording and F gave the candidate sources in the same
// order; otherwise refused, saying why. Each refused subcase is noise
// without the order. A place that holds no word of either value (c's x,
// d's y) is no source, and its order is not read: one F did not serve, or
// served though recorded after the call, leaves the value noise. Where it
// holds such a word, the word rule reads it as any place.
func TestARoundThatGaveTheEarlierAnswersInAnotherOrderThanTheRecordingRefuses(t *testing.T) {
	a := eAnswer("mock-a", 300*time.Millisecond, "7")
	b := eAnswer("mock-b", 200*time.Millisecond, "82")
	c := eAnswer("mock-c", 250*time.Millisecond, "x")
	late := eAnswer("mock-d", -100*time.Millisecond, "y")
	together := eAnswer("mock-b", 300*time.Millisecond, "82")
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
		{"the recording gave a 7 between them, F did not serve it", []Raw{a.ServedAs(1).ServedAsRecorded(), eAnswer("mock-c", 250*time.Millisecond, "7"), b.ServedAs(2).ServedAsRecorded()}, "record's full round did not give the application that word there before the call"},
		{"F served a 7 recorded after the call", []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded(), eAnswer("mock-d", -100*time.Millisecond, "7").ServedAs(3).ServedAsRecorded()}, "mock-d"},
		{"F served a twice, b between, b no source", []Raw{a.ServedIn(1, 3).ServedAsRecorded(), eAnswer("mock-b", 200*time.Millisecond, "9").ServedAs(2).ServedAsRecorded()}, "(the recording gave 1 earlier answers, record's full round more)" + tail},
	} {
		got := classifyF(t, "slot 82", "slot 7", nil, tc.raw, nil)
		if got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.want) || !strings.Contains(got.Why, " in mock-") {
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
		{"the recording gave c between them, F did not serve it", []Raw{a.ServedAs(1).ServedAsRecorded(), c, b.ServedAs(2).ServedAsRecorded()}},
		{"F served a recording made after the call", []Raw{a.ServedAs(1).ServedAsRecorded(), b.ServedAs(2).ServedAsRecorded(), late.ServedAs(3).ServedAsRecorded()}},
		{"F served c, no source, before both", []Raw{a.ServedAs(2).ServedAsRecorded(), c.ServedAs(1).ServedAsRecorded(), b.ServedAs(3).ServedAsRecorded()}},
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

// Two requests in flight together, both candidate sources (a answers F's
// 7, b the recorded 82): the recording gave the application their answers
// in the order the dependency and the network gave them, which keploy
// cannot tell from when the requests were recorded. mock-a is requested
// first and answered last, so the application had b's answer first. Ordered by request, the recording's order would be F's (a then b),
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
	b := eSpan("mock-b", 250*time.Millisecond, 200*time.Millisecond, "82")
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
		{"b's mock holds no answer time", []Raw{a.ServedAs(1).ServedAsRecorded(), ePgAnswered("mock-b", 200*time.Millisecond, time.Time{}, "SELECT next_slot FROM mock-b", []models.PostgresV3Cells{{{Value: "7"}}}, "SELECT 1").ServedAs(2).ServedAsRecorded()},
			"keploy cannot read when the recording answered mock-b (its mock holds no answer time it can read), so the recording cannot show the value does not follow that word"},
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
	// b's answer time unread where b holds the recorded 82: keploy cannot
	// tell F's round did not follow it, so the reading of it stands (a
	// variable). Where b holds no word of either value, it is no source, and
	// its times are not read.
	untimed := func(word string) Raw {
		return ePgAnswered("mock-b", 200*time.Millisecond, time.Time{}, "SELECT next_slot FROM mock-b", []models.PostgresV3Cells{{{Value: word}}}, "SELECT 1").ServedAs(2).ServedAsRecorded()
	}
	if got := classifyF(t, "slot 82", "slot 7", nil, []Raw{a.ServedAs(1).ServedAsRecorded(), untimed("82")}, nil); got.Kind != KindVariable || got.From.Mock != "mock-b" {
		t.Errorf("FALSE GREEN? b's mock holds no answer time and the recorded 82: %s (%q), want a variable of mock-b", got.Kind, got.Why)
	}
	if got := classifyF(t, "slot 82", "slot 7", nil, []Raw{a.ServedAs(1).ServedAsRecorded(), untimed("9")}, nil); got.Kind != KindNoise {
		t.Errorf("b's mock holds no answer time and no word of either value: %s (%q), want noise", got.Kind, got.Why)
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
// other way, so no order by name passes. Each holds a word of a value
// (mock-9 F's 7, mock-10 and mock-11 the recorded 82): candidate sources. The same holds for an exchange
// served leaf by leaf (Earlier.Payload's position).
func TestPlacesSentInOneWriteAreInTheOrderTheirConnectionCarriedThem(t *testing.T) {
	first := eAnswer("mock-9", 300*time.Millisecond, "7").SentOn("0", 9)
	second := eAnswer("mock-10", 300*time.Millisecond, "82").SentOn("0", 10)
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
	third := eAnswer("mock-11", 300*time.Millisecond, "82").SentOn("0", 11)
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

// From the Flyway item (lanes flyway-e2-on1, -on4): HikariCP opens a pool
// connection on its own thread while Flyway migrates on another, so the
// pool's start-up exchange (connection 2) is in flight together with a
// migration statement (connection 0). Neither holds a word of the value
// (Flyway's execution_time, 82 at record, 16 in F: a word of SELECT
// version()'s text, mock-2), so neither is a place the value could have
// copied, and the order exists only to tell which of the value's candidate
// sources the application had last: the overlap is not read, nor where F
// served the pool's exchange. Two candidate sources in flight together on
// two connections are still refused; on one connection at known positions
// they are ordered by the wire.
func TestOnlyTheValuesCandidateSourcesAreOrdered(t *testing.T) {
	span := func(name string, before, answered time.Duration, sql, word string) Raw {
		var rows []models.PostgresV3Cells
		if word != "" {
			rows = []models.PostgresV3Cells{{{Value: word}}}
		}
		return ePgAnswered(name, before, eRecAt.Add(-answered), sql, rows, "SHOW")
	}
	// The start-up served at even numbers, so the pool's exchange can be
	// served between any two of its places.
	startup := func(xs ...Raw) []Raw {
		raw := eStartup()
		for i := range raw {
			raw[i] = raw[i].ServedAs(2 * (i + 1)).ServedAsRecorded()
		}
		return append(raw, xs...)
	}
	// The migration's statement on connection 0, 300 ms to 240 ms before
	// the call; the pool's exchange on connection 2, inside it.
	ddl := span("mock-3", 300*time.Millisecond, 240*time.Millisecond, `ALTER TABLE "public"."flyway_schema_history" ADD CONSTRAINT "flyway_schema_history_pk" PRIMARY KEY ("installed_rank")`, "").SentOn("0", 4).ServedAs(8).ServedAsRecorded()
	pool := func(word string, n int) Raw {
		return span("mock-p", 290*time.Millisecond, 250*time.Millisecond, "SHOW server_version", word).SentOn("2", 1).ServedIn(n, n).ServedAsRecorded()
	}
	migrating := func(p Raw) []Raw {
		raw := startup(p)
		raw[3] = ddl
		return raw
	}
	for _, tc := range []struct {
		name string
		raw  []Raw
	}{
		{"the pool's exchange in flight with a migration statement", migrating(pool("PostgreSQL JDBC Driver", 9))},
		{"F served the pool's exchange before every start-up place", migrating(pool("PostgreSQL JDBC Driver", 1))},
		{"F served the pool's exchange where keploy does not number it", migrating(pool("PostgreSQL JDBC Driver", 0))},
		{"the pool's exchange holds the word, in flight with a migration statement alone", migrating(pool("16", 9))},
	} {
		if got := classifyF(t, "82", "16", nil, tc.raw, nil); got.Kind != KindNoise {
			t.Errorf("%s, 82 at record, 16 in F: %s (%q), want noise", tc.name, got.Kind, got.Why)
		}
	}
	// The pool's exchange in flight with the source itself (SELECT
	// version(), 800 ms to 700 ms before the call, on connection 0): with no
	// word of the value, read for nothing; holding the word, two candidate
	// sources whose order the recording cannot show on two connections
	// (refused), and shows on one at known positions (by the wire), unless
	// the positions disagree with when the requests were recorded.
	version := ePgAnswered("mock-2", 800*time.Millisecond, eRecAt.Add(-700*time.Millisecond), "SELECT version()", []models.PostgresV3Cells{{{Value: "PostgreSQL 16.4 (Debian 16.4-1.pgdg120+2) on x86_64-pc-linux-gnu, compiled by gcc (Debian 12.2.0-14) 12.2.0, 64-bit"}}}, "SELECT 1").SentOn("0", 3).ServedAs(6).ServedAsRecorded()
	inside := func(word, conn string, pos uint64) []Raw {
		raw := startup(span("mock-p", 790*time.Millisecond, 750*time.Millisecond, "SHOW server_version", word).SentOn(conn, pos).ServedAs(7).ServedAsRecorded())
		raw[2] = version
		return raw
	}
	if got := classifyF(t, "82", "16", nil, inside("PostgreSQL JDBC Driver", "2", 1), nil); got.Kind != KindNoise {
		t.Errorf("the pool's exchange, no word of the value, in flight with the source: %s (%q), want noise", got.Kind, got.Why)
	}
	if got := classifyF(t, "82", "16", nil, inside("16", "2", 1), nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "the recording's requests for mock-2 and mock-p were in flight together") {
		t.Errorf("FALSE GREEN? two candidate sources in flight together on two connections: %s (%q), want refused", got.Kind, got.Why)
	}
	if got := classifyF(t, "82", "16", nil, inside("16", "0", 4), nil); got.Kind != KindNoise {
		t.Errorf("two candidate sources in flight together on one connection, positions in their order: %s (%q), want noise", got.Kind, got.Why)
	}
	if got := classifyF(t, "82", "16", nil, inside("16", "0", 2), nil); got.Kind != KindUnaccounted || !strings.Contains(got.Why, "on one connection at positions that disagree") {
		t.Errorf("FALSE GREEN? two candidate sources on one connection, positions against their order: %s (%q), want refused", got.Kind, got.Why)
	}
}

// eSession is a connection's recorded Postgres start-up, requested the
// duration before before eRecAt and answered a millisecond later, its backend pid pid.
func eSession(name string, before time.Duration, pid int32) *models.Mock {
	return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: eRecAt.Add(-before),
		ResTimestampMock: eRecAt.Add(-before + time.Millisecond),
		Metadata:         map[string]string{"type": "config"},
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeSession, Session: &models.PostgresV3SessionSpec{
			ServerVersion: "15.2", BackendProcessID: pid, BackendSecretKey: 7000 + pid,
		}},
	}}
}

// Two connections opened at once may get each other's start-ups in record's
// full round (F): the application copied its connection's backend pid (81 at
// record) and F's 82 is the other connection's. Both start-ups' requests were
// aloft together, so F's order of them cannot be told and the pid is refused,
// whichever F claimed first, never learned as varying. Opened one after the
// other, each start-up keeps its place in F's order (ServedHandshakeIn).
func TestAPidCopiedFromTwoStartUpsOpenedAtOnceIsRefused(t *testing.T) {
	a, b := eSession("mock-hs-81", 900*time.Millisecond, 81), eSession("mock-hs-82", 850*time.Millisecond, 82)
	open := func(c string, m *models.Mock) []flightStep {
		return []flightStep{{c, "lift", nil}, {c, "handshake", m}, {c, "land", nil}}
	}
	ran := map[bool]int{}
	for i, steps := range interleavings(open("A", a), open("B", b)) {
		tr := NewTracker()
		runFlights(tr, steps)
		o := tr.Placed().Order
		raw := make([]Raw, 0, 2)
		for _, m := range []*models.Mock{a, b} {
			first, last := o.Of(m)
			raw = append(raw, RawOf(m, false).ServedIn(first, last).ServedAsRecorded())
		}
		at := overlapped(steps, "A", "B")
		ran[at]++
		if !at {
			fa, _ := o.Of(a)
			fb, _ := o.Of(b)
			if fa == 0 || fb == 0 {
				t.Fatalf("interleaving %d %v: opened one after the other, the start-ups at %d and %d; want each placed in F's order", i, steps, fa, fb)
			}
			continue
		}
		if got := classifyF(t, "81", "82", nil, raw, nil); got.Kind == KindNoise {
			t.Fatalf("interleaving %d %v: the pid copied from one of two start-ups opened at once was learned as varying (%q); want refused", i, steps, got.Why)
		}
	}
	if ran[true] == 0 || ran[false] == 0 {
		t.Fatalf("interleavings: %d opened at once, %d one after the other; want both", ran[true], ran[false])
	}
}

// Two connections opened one after the other whose roles in the application
// swap between the recording and F (a pool's threads that open in either
// order): at record the application's connection got mock-hs-81 and copied
// its backend pid, 81; in F the same connection opened second and got
// mock-hs-82, so it copied 82. F gave both start-ups as recorded, in the
// recorded order, before the call, so the rule a copy follows its source
// would read 81's start-up as a source the value moved away from and learn
// the pid as varying. A word a start-up carries never varies: refused,
// naming the start-up, whatever the order, under that rule and without it.
// A value the recording did not copy from a start-up is unchanged: F's word
// of one the recorded value lacks reads as any word both runs gave.
func TestAValueCopiedFromAStartUpIsRefusedWhenItsConnectionsSwapRoles(t *testing.T) {
	a, b := eSession("mock-hs-81", 900*time.Millisecond, 81), eSession("mock-hs-82", 850*time.Millisecond, 82)
	open := func(c string, m *models.Mock) []flightStep {
		return []flightStep{{c, "lift", nil}, {c, "handshake", m}, {c, "land", nil}}
	}
	const path = "bind.$8"
	given := func(recorded, live string, raw []Raw) Learned {
		t.Helper()
		got := ClassifyGiven(clockedDrifts(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: live}}}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: recorded}}}, eRecAt, nil, raw, nil, nil, nil)
		if len(got) != 1 {
			t.Fatalf("%q recorded, %q in F: %+v", recorded, live, got)
		}
		return got[0]
	}
	sequential := 0
	for i, steps := range interleavings(open("A", a), open("B", b)) {
		if overlapped(steps, "A", "B") {
			continue
		}
		sequential++
		tr := NewTracker()
		runFlights(tr, steps)
		o := tr.Placed().Order
		raw := make([]Raw, 0, 2)
		for _, m := range []*models.Mock{a, b} {
			first, last := o.Of(m)
			if first == 0 {
				t.Fatalf("interleaving %d %v: %s unplaced; want each start-up placed in F's order", i, steps, m.Name)
			}
			raw = append(raw, RawOf(m, false).ServedIn(first, last).ServedAsRecorded())
		}
		for _, tc := range []struct{ name, recorded, live, found string }{
			{"the backend pid", "81", "82", "its recorded value stands whole in mock-hs-81 at spec.postgresV3.session.backendProcessID"},
			{"the secret key", "7081", "7082", "its recorded value stands whole in mock-hs-81 at spec.postgresV3.session.backendSecretKey"},
			{"the pid inside a longer value", "conn-81", "conn-82", "a part of its recorded value (its characters 6 to 7) stands in mock-hs-81 at spec.postgresV3.session.backendProcessID"},
		} {
			for rule, got := range map[string]Learned{"a copy follows its source": classifyF(t, tc.recorded, tc.live, nil, raw, nil), "without it": given(tc.recorded, tc.live, raw)} {
				if got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.found+", a connection's recorded start-up") || !strings.Contains(got.Why, "never varies") {
					t.Errorf("FALSE GREEN? interleaving %d %v, %s copied from a start-up, %s: %s (%q); want refused, naming the start-up", i, steps, tc.name, rule, got.Kind, got.Why)
				}
			}
		}
		// Ten-digit secret keys, which half of random keys are, read as
		// instants too (the echo): refused as a start-up's word all the same.
		keyed := func(m *models.Mock, key int32) *models.Mock {
			c := *m
			c.Spec.PostgresV3 = &models.PostgresV3Spec{Type: models.PostgresV3TypeSession, Session: &models.PostgresV3SessionSpec{}}
			*c.Spec.PostgresV3.Session = *m.Spec.PostgresV3.Session
			c.Spec.PostgresV3.Session.BackendSecretKey = key
			return &c
		}
		for _, k := range [][2]int32{{1234567890, 1876543210}, {-1234567890, -1234567891}} {
			fa, la := o.Of(a)
			fb, lb := o.Of(b)
			rk := []Raw{RawOf(keyed(a, k[0]), false).ServedIn(fa, la).ServedAsRecorded(), RawOf(keyed(b, k[1]), false).ServedIn(fb, lb).ServedAsRecorded()}
			rec, live := strconv.Itoa(int(k[0])), strconv.Itoa(int(k[1]))
			for rule, got := range map[string]Learned{"a copy follows its source": classifyF(t, rec, live, nil, rk, nil), "without it": given(rec, live, rk)} {
				if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "the moment it was recorded with stands in mock-hs-81 at spec.postgresV3.session.backendSecretKey") || !strings.Contains(got.Why, ", a connection's recorded start-up") || !strings.Contains(got.Why, "never varies") {
					t.Errorf("FALSE GREEN? interleaving %d %v, the secret key %s copied from a start-up, %s in F, %s: %s (%q); want refused, naming the start-up", i, steps, rec, live, rule, got.Kind, got.Why)
				}
			}
		}
		if got := classifyF(t, "33", "82", nil, raw, nil); got.Kind != KindNoise {
			t.Errorf("interleaving %d %v: 33 at record, a start-up's pid in F: %s (%q); want noise", i, steps, got.Kind, got.Why)
		}
	}
	if sequential == 0 {
		t.Fatal("no interleaving opened the connections one after the other")
	}
}

// eLaneSession is a connection's recorded start-up as the Flyway lane's
// Postgres 16 server answers it (protocol 3.0, scram-sha-256, the server's
// parameters), its backend pid pid.
func eLaneSession(name string, before time.Duration, pid int32) *models.Mock {
	m := eSession(name, before, pid)
	s := m.Spec.PostgresV3.Session
	s.ProtocolVersion, s.ServerVersion, s.ObservedAuthMode = "3.0", "16.4 (Debian 16.4-1.pgdg120+2)", "scram-sha-256"
	s.ParameterStatus = map[string]string{
		"application_name": "PostgreSQL JDBC Driver", "DateStyle": "ISO, MDY", "integer_datetimes": "on",
		"scram_iterations": "4096", "server_encoding": "UTF8", "server_version": "16.4 (Debian 16.4-1.pgdg120+2)", "TimeZone": "UTC",
	}
	return m
}

// A changed value recorded equal to a word every start-up carries alike (on
// the lane's Postgres 16 server: 0 and 3 of the protocol version, 16 and 4
// of the server's, 4096, on, UTC) is read as any recording's word: no swap
// of start-ups could have changed it, since whichever start-up F gave the
// connection carried the same word there. So a migration's execution_time
// of 0, 3, 4 or 16 ms at record is learned again (F served both start-ups
// as recorded and the value moved while they did not: noise under the rule
// a copy follows its source; a variable at the start-up without it, as any
// recording's word, which the adapters refuse in a raw payload).
func TestAValueRecordedAsAWordEveryStartUpCarriesAlikeIsReadAsAnyRecordings(t *testing.T) {
	raw := []Raw{
		RawOf(eLaneSession("mock-hs-81", 900*time.Millisecond, 81), false).ServedAs(1).ServedAsRecorded(),
		RawOf(eLaneSession("mock-hs-82", 850*time.Millisecond, 82), false).ServedAs(2).ServedAsRecorded(),
	}
	const path = "bind.$8"
	for _, tc := range []struct{ recorded, live, field string }{
		{"0", "12", "protocolVersion"}, {"3", "12", "protocolVersion"}, {"4", "12", "serverVersion"}, {"16", "12", "serverVersion"},
		{"4096", "12", "parameterStatus.scram_iterations"}, {"on", "off", "parameterStatus.integer_datetimes"}, {"UTC", "IST", "parameterStatus.TimeZone"},
	} {
		if got := classifyF(t, tc.recorded, tc.live, nil, raw, nil); got.Kind != KindNoise {
			t.Errorf("%s at record, a word every start-up carries alike (%s), %s in F, the rule a copy follows its source: %s (%q); want noise", tc.recorded, tc.field, tc.live, got.Kind, got.Why)
		}
		got := ClassifyGiven(clockedDrifts(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.live}}}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: tc.recorded}}}, eRecAt, nil, raw, nil, nil, nil)
		if len(got) != 1 || got[0].Kind != KindVariable || got[0].From.Mock != "mock-hs-81" || got[0].From.startUp {
			t.Errorf("%s at record, a word every start-up carries alike (%s), %s in F, without the rule: %+v; want a variable at mock-hs-81 like any recording's word, not a start-up refusal", tc.recorded, tc.field, tc.live, got)
		}
	}
}

// A server parameter the start-ups recorded otherwise (an application_name
// set per pool) or one of them lacks can be changed by a swap of start-ups
// as a backend pid can: a value copied from it is refused, naming the
// start-up, under the rule a copy follows its source and without it. A
// backend pid recorded equal to a word every start-up carries alike (4096)
// is refused all the same: the pid's own field holds it, and so is the pid
// of the one start-up a set recorded, which no other start-up differs from:
// a backend pid and a secret key are each connection's own.
func TestAStartUpWordTheConnectionsDifferInIsRefusedWhateverItsField(t *testing.T) {
	const path = "bind.$8"
	given := func(recorded, live string, raw []Raw) Learned {
		t.Helper()
		got := ClassifyGiven(clockedDrifts(), []string{path}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: live}}}, Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: recorded}}}, eRecAt, nil, raw, nil, nil, nil)
		if len(got) != 1 {
			t.Fatalf("%q recorded, %q in F: %+v", recorded, live, got)
		}
		return got[0]
	}
	named := func(name string, before time.Duration, pid int32, app string) *models.Mock {
		m := eLaneSession(name, before, pid)
		if app == "" {
			delete(m.Spec.PostgresV3.Session.ParameterStatus, "application_name")
		} else {
			m.Spec.PostgresV3.Session.ParameterStatus["application_name"] = app
		}
		return m
	}
	for _, tc := range []struct {
		name, recorded, live, found string
		a, b                        *models.Mock
	}{
		{"application_name set per pool", "flyway", "hikari", "mock-hs-81 at spec.postgresV3.session.parameterStatus.application_name",
			named("mock-hs-81", 900*time.Millisecond, 81, "flyway"), named("mock-hs-82", 850*time.Millisecond, 82, "hikari")},
		{"application_name one start-up lacks", "flyway", "12", "mock-hs-81 at spec.postgresV3.session.parameterStatus.application_name",
			named("mock-hs-81", 900*time.Millisecond, 81, "flyway"), named("mock-hs-82", 850*time.Millisecond, 82, "")},
		{"a pid equal to a shared word", "4096", "82", "mock-hs-81 at spec.postgresV3.session.backendProcessID",
			eLaneSession("mock-hs-81", 900*time.Millisecond, 4096), eLaneSession("mock-hs-82", 850*time.Millisecond, 82)},
		{"the pid of the one start-up recorded", "81", "82", "mock-hs-81 at spec.postgresV3.session.backendProcessID",
			eLaneSession("mock-hs-81", 900*time.Millisecond, 81), nil},
	} {
		raw := []Raw{RawOf(tc.a, false).ServedAs(1).ServedAsRecorded()}
		if tc.b != nil {
			raw = append(raw, RawOf(tc.b, false).ServedAs(2).ServedAsRecorded())
		}
		for rule, got := range map[string]Learned{"a copy follows its source": classifyF(t, tc.recorded, tc.live, nil, raw, nil), "without it": given(tc.recorded, tc.live, raw)} {
			if got.Kind != KindUnaccounted || !strings.Contains(got.Why, tc.found+", a connection's recorded start-up") || !strings.Contains(got.Why, "never varies") {
				t.Errorf("FALSE GREEN? %s, %s at record, %s in F, %s: %s (%q); want refused, naming %s", tc.name, tc.recorded, tc.live, rule, got.Kind, got.Why, tc.found)
			}
		}
	}
}

// A start-up's match that stands in no one of its fields (an echo read
// across them) cannot be told apart from a word a swap of start-ups can
// change, so it is read as one and refused (swapHolds). A match in a field
// every start-up carries alike is no swap, and no other recording's match
// is a start-up's.
func TestAStartUpMatchInNoOneFieldIsReadAsASwapsWord(t *testing.T) {
	a, b := RawOf(eLaneSession("mock-hs-81", 900*time.Millisecond, 81), false), RawOf(eLaneSession("mock-hs-82", 850*time.Millisecond, 82), false)
	raw := []Raw{a, b}
	if !a.swapHolds(a.payload().text, raw, func(string) bool { return false }) {
		t.Error("a start-up's match in no one field is not read as a swap's word; want refused")
	}
	if a.swapHolds(a.payload().text, raw, func(own string) bool { return own == "UTF8" }) {
		t.Error("a start-up's match in a field every start-up carries alike is read as a swap's word")
	}
	if !a.swapHolds(a.payload().text, raw, func(own string) bool { return own == "81" }) {
		t.Error("a start-up's backend pid is not read as a swap's word")
	}
	other := RawOf(&models.Mock{Name: "mock-q", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://deps.example/a"}}}, false)
	if other.swapHolds(other.payload().text, raw, func(string) bool { return false }) {
		t.Error("a recording that is no start-up is read as one")
	}
}
