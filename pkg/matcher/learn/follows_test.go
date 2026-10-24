package learn

// Rows of the rule a copy follows its source (follows.go), from the Flyway
// item's probe: Flyway's start-up INSERT into flyway_schema_history binds
// execution_time, the milliseconds the migration took (82 at record, a digit
// or two in each replay round), whose live word is a word of an earlier
// start-up recording's payload that does not drift ("1" of "SELECT 1", "16"
// of "PostgreSQL 16.4"). Record's full round answers it provisionally and
// the confirming round decides.

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// eRecAt is the recorded instant of the INSERT (the lane's recording).
var eRecAt = time.Date(2026, 10, 24, 9, 55, 53, 938177550, time.UTC)

// eStartup is what the application received before the INSERT, at record
// and (served from these recordings) in F and C: the JDBC session set-up,
// HikariCP's validation query, Flyway's server probe, the schema-history
// table, and the migration's own seed row. None of it drifts.
func eStartup() []Raw {
	pg := func(name string, before time.Duration, sql string, rows []models.PostgresV3Cells, tag string) Raw {
		return RawOf(&models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: eRecAt.Add(-before),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: sql,
				Response:      &models.PostgresV3Response{Rows: rows, CommandComplete: tag},
			}},
		}}, false)
	}
	return []Raw{
		pg("mock-0", 900*time.Millisecond, "SET application_name = 'PostgreSQL JDBC Driver'", nil, "SET"),
		pg("mock-1", 850*time.Millisecond, "SELECT 1", []models.PostgresV3Cells{{{Value: "1"}}}, "SELECT 1"),
		pg("mock-2", 800*time.Millisecond, "SELECT version()", []models.PostgresV3Cells{{{Value: "PostgreSQL 16.4 (Debian 16.4-1.pgdg120+2) on x86_64-pc-linux-gnu, compiled by gcc (Debian 12.2.0-14) 12.2.0, 64-bit"}}}, "SELECT 1"),
		pg("mock-3", 300*time.Millisecond, `CREATE TABLE "public"."flyway_schema_history" ("installed_rank" INT NOT NULL, "execution_time" INTEGER NOT NULL)`, nil, "CREATE TABLE"),
		pg("mock-4", 200*time.Millisecond, "INSERT INTO customer (name, tier) VALUES ('Acme', 'gold')", nil, "INSERT 0 1"),
	}
}

// eRound is one replay round's observation of the call: the value the
// application bound for the drifted field, and the earlier places that
// round read (Seen leaves carry what that round served, in Live).
type eRound struct {
	name    string
	live    string
	earlier []Earlier
	raw     []Raw
}

func eRounds(f, c string) []eRound {
	return []eRound{{name: "F", live: f, raw: eStartup()}, {name: "C", live: c, raw: eStartup()}}
}

// Flyway's execution_time changes between F and C while every word it
// matched stays as recorded: it was copied from none of them, so it is
// learned as varying (noise), whichever start-up word each round's value
// happens to equal.
func TestEFlywayExecutionTimeBesideStartupWordsIsLearnedVarying(t *testing.T) {
	for _, tc := range []struct{ f, c, matched string }{
		{"1", "3", `"1" of SELECT 1, INSERT 0 1 (F)`},
		{"3", "1", `"1" (C only)`},
		{"16", "4", `"16" of PostgreSQL 16.4 (F), "4" of it (C)`},
		{"0", "1", `"0" of INSERT 0 1 (F), "1" (C): both fixed words, the value moved`},
		{"1", "82", "C binds the recorded value again"},
	} {
		got := classifyRounds(t, "bind.$8", "82", eRecAt, eRounds(tc.f, tc.c))
		if got.Kind != KindNoise {
			t.Errorf("execution_time 82 at record, %s in F, %s in C (%s): %s (%s), want noise", tc.f, tc.c, tc.matched, got.Kind, got.Why)
		}
	}
}

// The control: a value equal to the same fixed word in every replay round
// may be a copy of it. It stays refused, saying why.
func TestEAValueEqualToTheWordInEveryRoundStaysRefused(t *testing.T) {
	for _, tc := range []struct{ f, c string }{{"1", "1"}, {"16", "16"}, {"0", "0"}} {
		got := classifyRounds(t, "bind.$8", "82", eRecAt, eRounds(tc.f, tc.c))
		if got.Kind != KindUnaccounted || got.Why == "" {
			t.Errorf("execution_time 82 at record, %s in F and in C, beside a fixed %q: %s (%q), want refused with a reason", tc.f, tc.f, got.Kind, got.Why)
		}
	}
}

// A real echo moves with its source: the earlier exchange F and C served
// carries this run's value (a served leaf whose Live differs per round), and
// the bound value equals it in each round. The value changes between rounds,
// but so does its source: it is still refused. This row kills an (e) that
// reads "the value changed between F and C" alone as no echo.
func TestEARealEchoMovesWithItsSourceAndIsRefused(t *testing.T) {
	served := func(live string) []Earlier {
		return []Earlier{{Mock: "mock-5", At: eRecAt.Add(-100 * time.Millisecond), Payload: payloadOf("mock-5", eRecAt.Add(-100*time.Millisecond)), Seen: []Seen{
			{Side: Response, Leaf: Leaf{Path: "rows[0][1]", Type: String, Value: "5150"}, Live: live},
		}}}
	}
	rounds := []eRound{
		{name: "F", live: "16", earlier: served("16"), raw: eStartup()},
		{name: "C", live: "7", earlier: served("7"), raw: eStartup()},
	}
	got := classifyRounds(t, "bind.$8", "82", eRecAt, rounds)
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-5 at rows[0][1]") {
		t.Errorf("a value that equals mock-5's served rows[0][1] in F (16) and in C (7): %s (%q), want refused naming mock-5's rows[0][1] (it follows its source)", got.Kind, got.Why)
	}
}

// One replay observation is not enough: with only F seen, the drift is not
// learned as noise (refused, or pending C; see FLOW above).
func TestEOneReplayRoundIsNotEnough(t *testing.T) {
	got := classifyRounds(t, "bind.$8", "82", eRecAt, []eRound{{name: "F", live: "1", raw: eStartup()}})
	if got.Kind == KindNoise || got.Kind == KindVariable {
		t.Errorf("execution_time 82 at record, 1 in F, C not run: %s, want refused or pending", got.Kind)
	}
}

// classifyRounds reads the call in record's full round (the first round)
// under the rule a copy follows its source, and has the confirming round
// (the second, when there is one) decide a provisional reading.
func classifyRounds(t *testing.T, path, recorded string, at time.Time, rounds []eRound) Learned {
	t.Helper()
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: recorded}}}
	f := rounds[0]
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: f.live}}}
	out := ClassifyFollowing(NewDriftCache(), []string{path}, live, rec, at, f.earlier, f.raw, nil, nil, nil)
	if len(out) != 1 {
		t.Fatalf("round %s: %+v", f.name, out)
	}
	if out[0].Kind != KindPending || len(rounds) == 1 {
		return out[0]
	}
	if out[0].Pending == nil {
		t.Fatalf("round %s: pending with nothing kept: %+v", f.name, out[0])
	}
	c := rounds[1]
	return out[0].Pending.Decide(NewDriftCache(), Leaf{Path: path, Type: String, Value: c.live}, rec.Leaves[path], at, c.earlier, c.raw, nil, nil, nil)
}

// Record's full round alone answers the call provisionally: pending, never
// noise, with a reason that says it is undecided and names the place the
// word rule found the word at. ClassifyGiven, which no confirming round
// decides after, still refuses it.
func TestAProvisionalReadingIsPendingAndSaysWhy(t *testing.T) {
	path := "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "1"}}}
	got := ClassifyFollowing(NewDriftCache(), []string{path}, live, rec, eRecAt, nil, eStartup(), nil, nil, nil)
	if len(got) != 1 || got[0].Kind != KindPending || got[0].Pending == nil {
		t.Fatalf("execution_time 82 at record, 1 in record's full round: %+v, want pending", got)
	}
	why := got[0].Why
	for _, want := range []string{"pending, varying or a copy undecided: ", "appears in an earlier recording, in mock-1", "the confirming round decides", "counts as refused"} {
		if !strings.Contains(why, want) {
			t.Errorf("pending reason %q lacks %q", why, want)
		}
	}
	if strings.Contains(why, `"1"`) {
		t.Errorf("pending reason %q holds the value", why)
	}
	if p := got[0].Pending; p.Live != "1" || p.Recorded != "82" || len(p.Words) == 0 || len(p.Raw) != len(eStartup()) {
		t.Errorf("pending kept %+v, want F's value, the recorded one, the words found and the 5 recordings read", p)
	}
	today := ClassifyGiven(NewDriftCache(), []string{path}, live, rec, eRecAt, nil, eStartup(), nil, nil, nil)
	if today[0].Kind != KindUnaccounted {
		t.Errorf("ClassifyGiven of the same call: %s, want refused (no confirming round decides it)", today[0].Kind)
	}
}

// The confirming round decides only from places it gave the application
// too: where it gave none of the startup recordings before the call, it
// cannot show the value does not follow their words, and refuses, saying so.
func TestAConfirmingRoundThatDidNotGiveThePlaceRefuses(t *testing.T) {
	rounds := []eRound{{name: "F", live: "1", raw: eStartup()}, {name: "C", live: "3"}}
	got := classifyRounds(t, "bind.$8", "82", eRecAt, rounds)
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "the confirming round did not give the application that word there before the call") {
		t.Errorf("1 in F beside SELECT 1, 3 in C with no start-up recording given: %s (%q), want refused naming the missing place", got.Kind, got.Why)
	}
}

// A confirming round whose value still holds the word the place gave shows
// nothing: refused, saying both rounds held it.
func TestAConfirmingValueStillHoldingTheWordRefuses(t *testing.T) {
	got := classifyRounds(t, "bind.$8", "82", eRecAt, eRounds("1", "1 7"))
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "both saw a value holding that word") && !strings.Contains(got.Why, "held that word too") {
		t.Errorf("1 in F, \"1 7\" in C beside SELECT 1: %s (%q), want refused saying both rounds held the word", got.Kind, got.Why)
	}
}

// A word only the recorded value held, found at an earlier exchange that
// did not drift, is shown no copy by record's full round itself (it gave the
// word, the value lost it); the value is still noise only where the two
// rounds saw different values, and refused where they saw the same.
func TestARecordedWordIsNoiseOnlyWhereTheRoundsSawDifferentValues(t *testing.T) {
	served := []Earlier{{Mock: "mock-8", At: eRecAt.Add(-50 * time.Millisecond), Payload: payloadOf("mock-8", eRecAt.Add(-50*time.Millisecond)), Seen: []Seen{
		{Side: Response, Leaf: Leaf{Path: "body.retries", Type: Number, Value: "82"}, Live: "82"},
	}}}
	rounds := func(f, c string) []eRound {
		return []eRound{{name: "F", live: f, earlier: served, raw: eStartup()}, {name: "C", live: c, earlier: served, raw: eStartup()}}
	}
	if got := classifyRounds(t, "bind.$8", "took 82 ms", eRecAt, rounds("took 5 ms", "took 6 ms")); got.Kind != KindNoise {
		t.Errorf("took 82 ms at record beside mock-8's 82, 5 ms in F, 6 ms in C: %s (%q), want noise", got.Kind, got.Why)
	}
	got := classifyRounds(t, "bind.$8", "took 82 ms", eRecAt, rounds("took 5 ms", "took 5 ms"))
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "the confirming round saw the same value as record's full round") {
		t.Errorf("took 82 ms at record beside mock-8's 82, 5 ms in F and in C: %s (%q), want refused: two rounds that saw one value", got.Kind, got.Why)
	}
}

// The confirming round decides a pending reading of the recording it was
// made of alone.
func TestAPendingReadingOfAnotherRecordedValueIsRefused(t *testing.T) {
	path := "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "1"}}}
	got := ClassifyFollowing(NewDriftCache(), []string{path}, live, rec, eRecAt, nil, eStartup(), nil, nil, nil)
	if got[0].Kind != KindPending {
		t.Fatalf("F: %s, want pending", got[0].Kind)
	}
	d := got[0].Pending.Decide(NewDriftCache(), Leaf{Path: path, Type: String, Value: "3"}, Leaf{Path: path, Type: String, Value: "83"}, eRecAt, nil, eStartup(), nil, nil, nil)
	if d.Kind != KindUnaccounted || !strings.Contains(d.Why, "another recorded value") {
		t.Errorf("decided against recorded 83: %s (%q), want refused", d.Kind, d.Why)
	}
}

// An echo (the call's recorded answer holds the recorded value) beside a
// word the word rule found earlier is refused in record's full round, as
// ClassifyGiven refuses it: only a reading that would be noise is answered
// provisionally.
func TestAnEchoBesideAWordFoundEarlierIsRefusedNotPending(t *testing.T) {
	path := "bind.$8"
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "1"}}}
	resp := []Leaf{{Path: "rows[0][0]", Type: String, Value: "82"}}
	got := ClassifyFollowing(NewDriftCache(), []string{path}, live, rec, eRecAt, nil, eStartup(), resp, nil, nil)
	if got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, "appears in an earlier recording, in mock-1") {
		t.Errorf("82 echoed in the answer, 1 in F beside SELECT 1: %s (%q), want refused naming mock-1", got[0].Kind, got[0].Why)
	}
}

// A word of the confirming round's value found at a place record's full
// round never gave the application is one no round shows the value does not
// follow: refused, whatever F's words show.
func TestAConfirmingValueFoundAtAPlaceOnlyItGaveRefuses(t *testing.T) {
	extra := RawOf(&models.Mock{Name: "mock-7", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: eRecAt.Add(-40 * time.Millisecond),
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT count(*) FROM job", Response: &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: "7"}}}, CommandComplete: "SELECT 1"},
		}},
	}}, false)
	rounds := []eRound{{name: "F", live: "1", raw: eStartup()}, {name: "C", live: "7", raw: append(eStartup(), extra)}}
	got := classifyRounds(t, "bind.$8", "82", eRecAt, rounds)
	if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "in mock-7") {
		t.Errorf("1 in F, 7 in C beside mock-7's 7 that only C gave: %s (%q), want refused naming mock-7", got.Kind, got.Why)
	}
}
