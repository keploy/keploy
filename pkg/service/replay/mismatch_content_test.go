package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
)

// contentFailures are two misses of one test whose reports carry the
// application's content: one an agent withheld nothing from (an older agent,
// or one opted in), one with both whole-request renders.
func contentFailures() *TestFailureStore {
	tfs := NewTestFailureStore()
	tfs.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "POST /api/users?token=kp-print-query", ClosestMock: "mock-1", MatchPhase: models.MatchPhaseBody,
		Diff: `body.email: recorded "kp-print-diff"`,
		FieldDiffs: []models.MockFieldDiff{
			{Path: "body.email", Kind: models.DiffKindValueChanged, Expected: "kp-print-rec", Actual: "kp-print-live"},
			{Path: "body.n", Kind: models.DiffKindTypeChanged, Expected: "string: kp-print-type", Actual: "number: 7"},
		},
	})
	tfs.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "POST /api/cards", ClosestMock: "mock-2", MatchPhase: models.MatchPhaseBody,
		FieldDiffs:     []models.MockFieldDiff{{Path: "body.card", Kind: models.DiffKindValueChanged, Expected: "kp-print-card-rec", Actual: "kp-print-card-live"}},
		ClosestMockReq: "POST /api/cards\n\n{\"card\":\"kp-print-card-rec\"}", ReceivedReq: "POST /api/cards\n\n{\"card\":\"kp-print-card-live\"}",
	})
	return tfs
}

var printCanaries = []string{"kp-print-query", "kp-print-diff", "kp-print-rec", "kp-print-live", "kp-print-type", "kp-print-card-rec", "kp-print-card-live"}

// By default the mismatch table prints each call, the fields it differs in
// and how, and none of the values: a side the field has reads "(withheld)",
// and the table says once how to see the values.
func TestTheMismatchTablePrintsNoContentByDefault(t *testing.T) {
	out := captureStdout(t, contentFailures().PrintFailuresTable)
	for _, c := range printCanaries {
		if strings.Contains(out, c) {
			t.Errorf("the mismatch table printed %q:\n%s", c, out)
		}
	}
	for _, want := range []string{"POST /api/users", "body.email", "body.n", "type " + models.WithheldValue, "body.card", models.WithheldValue} {
		if !strings.Contains(out, want) {
			t.Errorf("the mismatch table does not print %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, models.ContentWithheldNote); got != 1 {
		t.Errorf("the note is printed %d times, want once:\n%s", got, out)
	}
}

// With the switch on the table prints the values, as it did, and no note.
func TestTheMismatchTablePrintsTheContentWithTheSwitchOn(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	out := captureStdout(t, contentFailures().PrintFailuresTable)
	for _, c := range []string{"kp-print-query", "kp-print-rec", "kp-print-live", "kp-print-type", "kp-print-card-rec", "kp-print-card-live"} {
		if !strings.Contains(out, c) {
			t.Errorf("with the switch on the table lost %q:\n%s", c, out)
		}
	}
	if strings.Contains(out, models.WithheldValue) || strings.Contains(out, models.ContentWithheldNote) {
		t.Errorf("with the switch on the table withholds:\n%s", out)
	}
}

// A variable's miss prints its diff, which names the variable and its place,
// with the values withheld, and the table says once how to see them.
func TestTheMismatchTablePrintsAVariablesMissWithoutItsValues(t *testing.T) {
	tfs := NewTestFailureStore()
	tfs.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "GET /orders/o9", ClosestMock: "mock-2", MatchPhase: models.MatchPhaseVariable, ContentWithheld: true,
		Diff:       "mock mock-2: variable orderId is (withheld) in this test (taken at mock-1 body.id), and this call sent (withheld) at url.path[1]",
		FieldDiffs: []models.MockFieldDiff{{Path: "url.path[1]", Kind: models.DiffKindVariable}},
	})
	out := captureStdout(t, tfs.PrintFailuresTable)
	if !strings.Contains(out, "variable orderId is (withheld)") || strings.Count(out, models.ContentWithheldNote) != 1 {
		t.Errorf("printed:\n%s", out)
	}
}

// A call the agent withheld stays withheld when this CLI is opted in (the
// agent was not, or was older): the table prints "(withheld)" where the
// values were, and the note, never an empty value that reads as one the
// application sent.
func TestTheMismatchTableSaysACallTheAgentWithheldIsWithheld(t *testing.T) {
	t.Cleanup(logcontent.Set(true))
	tfs := NewTestFailureStore()
	tfs.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "POST /api/users", ClosestMock: "mock-1", MatchPhase: models.MatchPhaseBody, ContentWithheld: true,
		Diff:       "body.email: value changed",
		FieldDiffs: []models.MockFieldDiff{{Path: "body.email", Kind: models.DiffKindValueChanged}},
	})
	out := captureStdout(t, tfs.PrintFailuresTable)
	if strings.Count(out, models.WithheldValue) != 2 || strings.Count(out, models.ContentWithheldNote) != 1 {
		t.Errorf("a withheld call printed with the switch on:\n%s", out)
	}
}

// A miss shown by its Diff alone, a parser's own words with their values
// withheld (MySQL's statement with its literals written "?"), says how to see
// them, as one shown by its fields does; one that withheld nothing does not.
func TestTheMismatchTableSaysADiffOnlyMissIsWithheld(t *testing.T) {
	tfs := NewTestFailureStore()
	tfs.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{
		Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT * FROM users WHERE id = ?", ClosestMock: "mock-1", MatchPhase: models.MatchPhaseBody, ContentWithheld: true,
		Diff: "actual: SELECT * FROM users WHERE id = ?; closest: SELECT * FROM users WHERE id = ?",
	})
	out := captureStdout(t, tfs.PrintFailuresTable)
	if !strings.Contains(out, "Diff: actual: SELECT * FROM users WHERE id = ?") || strings.Count(out, models.ContentWithheldNote) != 1 {
		t.Errorf("a withheld Diff-only miss printed:\n%s", out)
	}
	t.Cleanup(logcontent.Set(true))
	tfs = NewTestFailureStore()
	tfs.AddUnmatchedCallForTest("test-set-0", "test-1", models.UnmatchedCall{
		Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT * FROM users WHERE id = 7", ClosestMock: "mock-1", MatchPhase: models.MatchPhaseBody,
		Diff: "actual: SELECT * FROM users WHERE id = 7; closest: SELECT * FROM users WHERE id = 8",
	})
	if out := captureStdout(t, tfs.PrintFailuresTable); strings.Contains(out, models.ContentWithheldNote) {
		t.Errorf("a Diff-only miss that withheld nothing says it did:\n%s", out)
	}
}
