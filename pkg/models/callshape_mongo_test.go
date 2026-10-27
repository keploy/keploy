package models

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requestValues are values the requests behind the rows' summaries sent (an
// app's ID, a user's, an email, a tenant's): no shape may hold one.
var requestValues = []string{"5e246614", "u-1", "u-2", "a@b.c", "alice", "12345"}

// A MongoDB miss's summary (integrations' "<command> on <db>.<collection>")
// keeps its command and the names it ran on when they read as names, and
// writes {} for any that may be a value; a summary of another form has no
// shape.
func TestCallShape_MongoDB(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"a find", "find on keploy.apps", "find on keploy.apps"},
		{"an update", "update on app.docs", "update on app.docs"},
		{"an aggregate", "aggregate on app.events", "aggregate on app.events"},
		{"an insert", "insert on app.users", "insert on app.users"},
		{"a delete", "delete on app.sessions", "delete on app.sessions"},
		{"a findAndModify", "findAndModify on app.counters", "findAndModify on app.counters"},
		{"a count", "count on app.users", "count on app.users"},
		{"no database", "find on apps", "find on apps"},
		{"a dotted collection", "find on app.fs.chunks", "find on app.fs.chunks"},
		{"a command on no collection", "ping on ", "ping"},
		{"a command alone", "ping", "ping"},
		{"a bare value", "u-1", ""},
		{"a collection named for a tenant's ID", "find on app.events_5e246614", "find on app.{}"},
		{"a database named for a tenant's ID", "find on t5e246614.events", "find on {}.events"},
		{"a numeric collection", "getMore on app.12345", "getMore on app.{}"},
		{"a collection with no database, value-looking", "find on u-1", "find on {}"},
		{"a command that is no name", "a@b.c on app.docs", "{} on app.docs"},
		{"no command", " on app.docs", "{} on app.docs"},
		{"a raw request", `{ SectionSingle msg: {"find":"apps","filter":{"appId":"5e246614"}} }`, ""},
		{"empty", "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, proto := range []string{"MongoDB", "Mongo", "mongodb"} {
				got := CallShape(proto, c.in)
				require.Equal(t, c.want, got, proto)
				for _, v := range requestValues {
					require.NotContains(t, got, v)
				}
				require.Equal(t, got, CallShape(proto, got), "a shape is its own shape")
			}
		})
	}
	require.Empty(t, CallShape("Redis", "find on app.docs"), "another protocol has no shape")
}

// A field keeps an operator key ($set, $in, $oid) when the name after its
// '$' reads as a name; one with no name, or a value after it, is {}.
func TestFieldShape_KeepsOperatorKeys(t *testing.T) {
	for in, want := range map[string]string{
		"body.cmd.updates[].u.$set.createdBy":    "body.cmd.updates[].u.$set.createdBy",
		"body.cmd.filter.$and[0].status":         "body.cmd.filter.$and[0].status",
		"body.cmd.filter.appId.$in[2]":           "body.cmd.filter.appId.$in[2]",
		"body.cmd.documents[]._id.$oid":          "body.cmd.documents[]._id.$oid",
		"body.cmd.updates[].u.$setOnInsert.at":   "body.cmd.updates[].u.$setOnInsert.at",
		"body.cmd.updates[].u.$set.u-1":          "body.cmd.updates[].u.$set.{}",
		"body.cmd.filter.$":                      "body.cmd.filter.{}",
		"body.cmd.filter.$5e246614":              "body.cmd.filter.{}",
		"body.cmd.filter.$$ROOT":                 "body.cmd.filter.{}",
		"body.cmd.filter.$a1b2":                  "body.cmd.filter.{}",
		"body.cmd.pipeline[0].$match.tenant":     "body.cmd.pipeline[0].$match.tenant",
		"body.users.$NffrFeUfNVaHib.score":       "body.users.{}.score",
		"body.cmd.updates[].u.$set.createdBy[3]": "body.cmd.updates[].u.$set.createdBy[3]",
	} {
		got := FieldShape(in)
		require.Equal(t, want, got, in)
		require.Equal(t, got, FieldShape(got), "a shape is its own shape")
		for _, v := range requestValues {
			require.NotContains(t, got, v)
		}
	}
}

// A miss names its closest recording with why it was closest, in fixed
// words of how far its matcher got; a phase that says nothing of the mock's
// likeness names it alone.
func TestClosestText(t *testing.T) {
	for phase, want := range map[string]string{
		MatchPhaseBody:       "closest recording mock-12 (a call of the same shape, its request body differs)",
		MatchPhaseStrict:     "closest recording mock-12 (a call of the same shape, a field it checks strictly differs)",
		MatchPhaseExhausted:  "closest recording mock-12 (the nearest the matcher found)",
		MatchPhaseVariable:   "closest recording mock-12 (a test variable it declares did not hold)",
		MatchPhaseSchema:     "closest recording mock-12",
		MatchPhaseProtoError: "closest recording mock-12",
		"":                   "closest recording mock-12",
	} {
		require.Equal(t, want, ClosestText(UnmatchedCall{ClosestMock: "mock-12", MatchPhase: phase}), phase)
	}
	require.Empty(t, ClosestText(UnmatchedCall{MatchPhase: MatchPhaseBody}))
	require.False(t, strings.Contains(ClosestText(UnmatchedCall{ClosestMock: "mock-1", MatchPhase: MatchPhaseBody, ActualSummary: "find on app.u-1"}), "u-1"))
}

// DiffersPhrase is the one naming of a miss's fields every reason, next step
// and evidence line uses; NamedFields is it without the verb, naming fewer
// fields when asked and counting the rest, never naming none.
func TestDiffersPhraseNamesThreeFieldsAndCountsTheRest(t *testing.T) {
	for _, c := range []struct {
		fields []string
		want   string
	}{
		{nil, ""},
		{[]string{"a"}, "a differs"},
		{[]string{"a", "b"}, "a and b differ"},
		{[]string{"a", "b", "c"}, "a, b and c differ"},
		{[]string{"a", "b", "c", "d"}, "a, b, c and 1 more differ"},
		{[]string{"a", "b", "c", "d", "e"}, "a, b, c and 2 more differ"},
	} {
		if got := DiffersPhrase(c.fields); got != c.want {
			t.Errorf("DiffersPhrase(%q) = %q, want %q", c.fields, got, c.want)
		}
	}
	for _, c := range []struct {
		fields []string
		named  int
		want   string
	}{
		{nil, 3, ""},
		{[]string{"a"}, 0, "a"},
		{[]string{"a", "b", "c"}, 0, "a and 2 more"},
		{[]string{"a", "b", "c"}, 1, "a and 2 more"},
		{[]string{"a", "b", "c"}, 2, "a, b and 1 more"},
		{[]string{"a", "b", "c"}, 3, "a, b and c"},
		{[]string{"a", "b", "c"}, 9, "a, b and c"},
	} {
		if got := NamedFields(c.fields, c.named); got != c.want {
			t.Errorf("NamedFields(%q, %d) = %q, want %q", c.fields, c.named, got, c.want)
		}
	}
}
