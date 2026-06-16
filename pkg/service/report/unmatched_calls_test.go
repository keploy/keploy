package report

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestRenderUnmatchedCalls_StructuredOutput(t *testing.T) {
	test := models.TestResult{
		FailureInfo: models.FailureInfo{
			UnmatchedCalls: []models.UnmatchedCall{
				{
					Protocol:       "HTTP",
					ActualSummary:  "POST /orders",
					ClosestMock:    "mock-7",
					MatchPhase:     models.MatchPhaseBody,
					CandidateCount: 3,
					NextSteps:      "add noise or re-record",
					FieldDiffs: []models.MockFieldDiff{
						{Path: "body.created_at", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"},
						{Path: "header.X-Old", Kind: models.DiffKindMissingInLive, Expected: "v"},
					},
				},
			},
		},
	}

	var sb strings.Builder
	renderUnmatchedCalls(&sb, test)
	out := sb.String()

	for _, want := range []string{
		"OUTGOING CALLS WITH NO MATCHING MOCK",
		"[HTTP] POST /orders",
		models.MatchPhaseBody,
		"3 candidate mock(s)",
		"closest mock: mock-7",
		"body.created_at",
		"header.X-Old",
		"next steps: add noise or re-record",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderUnmatchedCalls_EmptyIsSilent(t *testing.T) {
	var sb strings.Builder
	renderUnmatchedCalls(&sb, models.TestResult{})
	if sb.Len() != 0 {
		t.Errorf("expected no output for tests without unmatched calls, got %q", sb.String())
	}
}
