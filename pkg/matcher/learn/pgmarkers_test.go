package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A Postgres statement's parameter markers are syntax, not data: the digits
// of each "$k" that stands as a token of the statement and names one of the
// values it carries (its binds and normalized constants) are no word of its
// payload. Everything else that holds "$" and digits stays as it is: a
// quoted string, a quoted identifier, a dollar-quoted body, a comment, an
// identifier holding "$", a marker run on into a word, and a marker whose
// number names no value of the statement.
func TestAStatementsParameterMarkersAreNoWordsOfItsPayload(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		n         int
		want      string
	}{
		{"an INSERT's four binds", `INSERT INTO "roles" ("id","name") VALUES ($1,$2,$3,$4) RETURNING "id"`, 4, `INSERT INTO "roles" ("id","name") VALUES ($ ,$ ,$ ,$ ) RETURNING "id"`},
		{"a SET's normalized constant", "SET client_min_messages TO $1", 1, "SET client_min_messages TO $ "},
		{"a cast after a marker, two digits", "SELECT $12::int, $3", 12, "SELECT $  ::int, $ "},
		{"no values carried", "SELECT $1", 0, "SELECT $1"},
		{"a marker naming no value", "SELECT $1, $2", 1, "SELECT $ , $2"},
		{"$0", "SELECT $0", 3, "SELECT $0"},
		{"a quoted string", "SELECT '$1', $1", 1, "SELECT '$1', $ "},
		{"a quoted string with a doubled quote", "SELECT 'it''s $1', $1", 1, "SELECT 'it''s $1', $ "},
		{"an E string with an escaped quote", `SELECT E'\' $1', $1`, 1, `SELECT E'\' $1', $ `},
		{"an e string with an escaped quote", `SELECT e'\' $1', $1`, 1, `SELECT e'\' $1', $ `},
		// A doubled quote stands for one inside the E string. Read as a close
		// and a new plain string, the escaped quote after it would end that
		// string, and the "$1" inside would read as a marker.
		{"an E string with a doubled quote, then an escaped quote", `SELECT E'it''s \' $1', $1`, 1, `SELECT E'it''s \' $1', $ `},
		{"a plain string ending in a backslash", `SELECT 'a\', $1`, 1, `SELECT 'a\', $ `},
		{"an identifier ending in E before a string", `SELECT x FROM t WHERE'\' = $1`, 1, `SELECT x FROM t WHERE'\' = $ `},
		{"a quoted identifier", `SELECT "$1", $1 FROM t`, 1, `SELECT "$1", $  FROM t`},
		{"a dollar-quoted body", "DO $$ SELECT $1 $$; SELECT $1", 1, "DO $$ SELECT $1 $$; SELECT $ "},
		{"a tagged dollar-quoted body", "SELECT $fn$ $1 $x$ $fn$, $1", 1, "SELECT $fn$ $1 $x$ $fn$, $ "},
		{"an unclosed dollar quote", "SELECT $q$ $1", 1, "SELECT $q$ $1"},
		{"a line comment", "SELECT $1 -- $1\n, $1", 1, "SELECT $  -- $1\n, $ "},
		{"a nested block comment", "SELECT /* /* $1 */ $1 */ $1", 1, "SELECT /* /* $1 */ $1 */ $ "},
		{"an unclosed block comment", "SELECT $1 /* $1", 1, "SELECT $  /* $1"},
		{"an identifier holding $", "SELECT a$1, $1", 1, "SELECT a$1, $ "},
		{"a marker run on into a word", "SELECT $1a, $1$", 1, "SELECT $1a, $1$"},
		{"an unclosed quote", "SELECT '$1", 1, "SELECT '$1"},
		{"a lone dollar", "SELECT $ + $1", 1, "SELECT $ + $ "},
		{"an identifier holding a non-ASCII letter and $", "SELECT é$1, $1", 1, "SELECT é$1, $ "},
		{"a tag holding _, a digit and a non-ASCII letter", "SELECT $a_1é$ $1 $a_1é$, $1", 1, "SELECT $a_1é$ $1 $a_1é$, $ "},
	} {
		if got := pgMarkersBlanked(tc.sql, tc.n); got != tc.want {
			t.Errorf("%s: %q with %d values reads %q, want %q", tc.name, tc.sql, tc.n, got, tc.want)
		}
	}
	// A statement's values are its binds and the constants normalized out
	// of its text after them: each names a marker.
	for _, tc := range []struct {
		name          string
		binds, consts int
		want          string
	}{
		{"two binds", 2, 0, "SELECT $ , $ , $3"},
		{"a bind and a constant", 1, 1, "SELECT $ , $ , $3"},
		{"two constants", 0, 2, "SELECT $ , $ , $3"},
	} {
		q := &models.PostgresV3QuerySpec{SQLNormalized: "SELECT $1, $2, $3", BindValues: make(models.PostgresV3Cells, tc.binds), Literals: make(models.PostgresV3Cells, tc.consts)}
		spec := payloadSpec(models.MockSpec{PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: q}})
		if got := spec.PostgresV3.Query.SQLNormalized; got != tc.want || q.SQLNormalized != "SELECT $1, $2, $3" {
			t.Errorf("%s: the payload reads %q (the mock %q), want %q (the mock unchanged)", tc.name, got, q.SQLNormalized, tc.want)
		}
	}
}

// node-jwt's start-up (from the lane's leg r30F-2): Sequelize's statements
// carry their values as binds, and every one with a first value says "$1".
// A value recorded with a "1" that changed in F is no copy of that "1": a
// statement F never served, whose text holds "$1" at a bind's place, is no
// place the value came from, and the value is learned as varying. A "$1"
// inside a quoted string of such a statement is data: the value stays
// refused there. The statement's own payload keeps its binds.
func TestAParameterMarkerIsNoPlaceAValueCameFrom(t *testing.T) {
	stmt := func(sql string, binds ...string) Raw {
		var cells models.PostgresV3Cells
		for _, v := range binds {
			cells = append(cells, models.PostgresV3Cell{Value: v})
		}
		return RawOf(&models.Mock{Name: "mock-16", Kind: models.PostgresV3, Spec: models.MockSpec{
			ReqTimestampMock: eRecAt.Add(-550 * time.Millisecond), ResTimestampMock: eRecAt.Add(-547 * time.Millisecond),
			PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
				SQLNormalized: sql, BindValues: cells,
				Response: &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: "2"}}}},
			}},
		}}, false)
	}
	insert := stmt(`INSERT INTO "roles" ("id","name") VALUES ($1,$2) RETURNING "id"`, "2", "user")
	if text := insert.Text(); ContainsToken(text, "1") || !ContainsToken(text, "user") || !strings.Contains(text, "VALUES ($ ,$ )") {
		t.Fatalf("the INSERT's payload reads %q: want its binds, and its markers without their digits", text)
	}
	if got := classifyF(t, "slot 1", "slot 9", nil, []Raw{stmt(`INSERT INTO "roles" ("id","name") VALUES ($1,$2) RETURNING "id"`, "2", "user")}, nil); got.Kind != KindNoise {
		t.Errorf("a \"1\" only in a marker of a statement F never served: %s (%q), want noise", got.Kind, got.Why)
	}
	if got := classifyF(t, "slot 1", "slot 9", nil, []Raw{stmt(`INSERT INTO "roles" ("id","name") VALUES ($1,'$1') RETURNING "id"`, "2")}, nil); got.Kind == KindNoise || !strings.Contains(got.Why, "mock-16") {
		t.Errorf("FALSE GREEN? a \"1\" in a quoted string of a statement F never served: %s (%q), want refused naming mock-16", got.Kind, got.Why)
	}
	if got := classifyF(t, "slot 1", "slot 9", nil, []Raw{stmt(`INSERT INTO "roles" ("id","name") VALUES ($1,$2) RETURNING "id"`)}, nil); got.Kind == KindNoise || !strings.Contains(got.Why, "mock-16") {
		t.Errorf("FALSE GREEN? a \"$1\" of a statement whose binds the recording does not hold: %s (%q), want refused naming mock-16", got.Kind, got.Why)
	}
}
