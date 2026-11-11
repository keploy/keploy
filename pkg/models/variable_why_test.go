package models

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A variable miss's evidence keeps the variable pass's why (VariableWhy): the
// variable by its name, the place it was bound at in its shape, and the
// recording that bound it, never a value, so a reader of the run (a user's
// agent among them) is told which variable did not hold and where.
func TestSanitizedVariableMissKeepsThePassWhy(t *testing.T) {
	const why = "the call does not hold variable run-appId0-carried-2's value in this run (bound at body.docs.msgs.0.appId#0 of mock-326)"
	miss := func(why string) UnmatchedCall {
		return UnmatchedCall{Protocol: "MongoDB", MatchPhase: MatchPhaseVariable, ClosestMock: "mock-462", VariableWhy: why,
			NextSteps:  "mock-462 is a recording of this call whose values flow between calls (its variables), and it does not answer this call: " + why,
			FieldDiffs: []MockFieldDiff{{Path: "body.docs.msgs.0.q.app_id", Kind: DiffKindVariable, Expected: "17f9a561-3094", Actual: "4503da12-86ec"}}}.Sanitized()
	}
	got := miss(why)
	require.Equal(t, "keploy's variable pass refused the call as mock-462's call: the call does not hold variable run-appId0-carried-2's value in this run (bound at "+FieldShape("body.docs.msgs.0.appId#0")+" of mock-326): fix the application, or re-record the test with 'keploy mock record' if that is intended", got.NextSteps)
	require.Equal(t, "the call does not hold variable run-appId0-carried-2's value in this run (bound at "+FieldShape("body.docs.msgs.0.appId#0")+" of mock-326)", got.VariableWhy)
	for _, value := range []string{"17f9a561", "4503da12"} {
		require.NotContains(t, got.NextSteps, value)
	}
	// A body path whose key may be a value keeps its shape alone.
	keyed := miss("the call does not hold variable userId's value in this run (bound at body.users.alice@x.com.id of mock-9)")
	require.NotContains(t, keyed.NextSteps, "alice")
	require.Contains(t, keyed.NextSteps, "variable userId's value")
	// A name keploy does not learn (a variables block edited by hand) may be
	// a value: the why stays out, and the next step is keploy's own sentence.
	for _, name := range []string{"alice@x.com", "a.b", "x/y", "app#0"} {
		bad := miss("the call does not hold variable " + name + "'s value in this run (bound at body.id of mock-9)")
		require.Empty(t, bad.VariableWhy, name)
		require.NotContains(t, bad.NextSteps, name)
		require.True(t, strings.HasPrefix(bad.NextSteps, "a test variable did not hold in mock-462"), bad.NextSteps)
	}
	// A learned name is a key's length at most: 128 is kept, a longer one is
	// not a key keploy learns at, and the why stays out.
	long := miss("the call does not hold variable " + strings.Repeat("a", 128) + "'s value in this run (bound at body.id of mock-9)")
	require.Contains(t, long.VariableWhy, strings.Repeat("a", 128))
	tooLong := miss("the call does not hold variable " + strings.Repeat("a", 129) + "'s value in this run (bound at body.id of mock-9)")
	require.Empty(t, tooLong.VariableWhy)
	require.True(t, strings.HasPrefix(tooLong.NextSteps, "a test variable did not hold in mock-462"), tooLong.NextSteps)
	// No why: keploy's own sentence, as before.
	none := miss("")
	require.Empty(t, none.VariableWhy)
	require.True(t, strings.HasPrefix(none.NextSteps, "a test variable did not hold in mock-462"), none.NextSteps)
	// Only a variable miss keeps the why.
	other := UnmatchedCall{Protocol: "MongoDB", MatchPhase: "body_mismatch", VariableWhy: why}.Sanitized()
	require.Empty(t, other.VariableWhy)
	// A given's sentence still wins: it names the test's own name for a value.
	given := UnmatchedCall{Protocol: "HTTP", MatchPhase: MatchPhaseVariable, ClosestMock: "mock-1", Given: "given:app#2", VariableWhy: why}.Sanitized()
	require.Equal(t, "given:app#2", given.Given)
	require.True(t, strings.HasPrefix(given.NextSteps, "the test gave given:app#2"), given.NextSteps)
}
