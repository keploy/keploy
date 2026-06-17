package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestHasPayloadAwareMismatch pins the rule that decides whether a mock mismatch
// fails the test: only a MatchPhaseBody mismatch (schema matched, request body
// diverged — a real payload regression) does. Benign misses (no candidate /
// schema phase, DNS, unrecorded ops) stay warn-only.
func TestHasPayloadAwareMismatch(t *testing.T) {
	r := &Replayer{mockMismatchFailures: NewTestFailureStore()}

	if r.hasPayloadAwareMismatch("ts-0", "tc-1") {
		t.Fatal("no failures must not be a payload-aware mismatch")
	}

	// A body-phase mismatch is a real payload regression -> must fail the test.
	r.mockMismatchFailures.AddUnmatchedCallForTest("ts-0", "tc-1", models.UnmatchedCall{
		Protocol:   "Pulsar",
		MatchPhase: models.MatchPhaseBody,
	})
	if !r.hasPayloadAwareMismatch("ts-0", "tc-1") {
		t.Error("a MatchPhaseBody mismatch must be payload-aware (fail the test)")
	}

	// A schema-phase miss (no candidate / benign) must NOT fail the test.
	r.mockMismatchFailures.AddUnmatchedCallForTest("ts-0", "tc-2", models.UnmatchedCall{
		Protocol:   "Pulsar",
		MatchPhase: models.MatchPhaseSchema,
	})
	if r.hasPayloadAwareMismatch("ts-0", "tc-2") {
		t.Error("a non-body (schema) miss must NOT fail the test (warn-only)")
	}

	// Scoped per test case: tc-1's body mismatch must not leak to another case.
	if r.hasPayloadAwareMismatch("ts-0", "tc-99") {
		t.Error("an unrelated test case must not report a payload-aware mismatch")
	}
}
