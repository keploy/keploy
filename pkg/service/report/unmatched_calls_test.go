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

// When nothing in the compared set targets the upstream, the "closest mock" is
// on a DIFFERENT host and its field differences describe a comparison that
// never meant anything. This block advertises its paths as copy-pasteable noise
// configuration, so rendering them here would invite a user to noise away a
// path difference between two unrelated calls. The values stay on the
// UnmatchedCall for machine consumers; only this rendering drops them.
func TestRenderUnmatchedCalls_SuppressesClosestMockWhenOutOfScope(t *testing.T) {
	test := models.TestResult{
		FailureInfo: models.FailureInfo{
			UnmatchedCalls: []models.UnmatchedCall{
				{
					Protocol:         "HTTP",
					ActualSummary:    "GET /v1/metrics",
					Destination:      "192.0.2.20",
					ClosestMock:      "mock-1",
					MatchPhase:       models.MatchPhaseSchema,
					DestinationScope: models.DestinationScopeNotInComparedSet,
					CandidateCount:   19,
					NextSteps:        "No recorded HTTP mock in the compared set targets 192.0.2.20. ...",
					FieldDiffs: []models.MockFieldDiff{
						{Path: "path", Kind: models.DiffKindValueChanged, Expected: "/api/orders", Actual: "/v1/metrics"},
					},
				},
			},
		},
	}

	var sb strings.Builder
	renderUnmatchedCalls(&sb, test)
	out := sb.String()

	for _, absent := range []string{"closest mock: mock-1", "/api/orders", "noise-config compatible"} {
		if strings.Contains(out, absent) {
			t.Errorf("out-of-scope miss must not render %q:\n%s", absent, out)
		}
	}
	// The cascade stop and the guidance are still this miss's own facts, and
	// the suppression line NAMES the destination it is about. It used to say
	// "this destination" and resolve only because the default NextSteps
	// happens to spell the host out on the next line — an accident a parser
	// supplying its own hint (WithNextSteps) removes.
	for _, want := range []string{
		"no recorded mock in the compared set targets 192.0.2.20; closest-mock diff omitted",
		models.MatchPhaseSchema,
		"next steps: No recorded HTTP mock in the compared set targets 192.0.2.20",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output missing %q:\n%s", want, out)
		}
	}
}

// The suppression line must name the upstream even when the parser supplied
// its own hint, which is exactly when nothing else on the line does.
func TestRenderUnmatchedCalls_SuppressionLineNamesTheDestination(t *testing.T) {
	test := models.TestResult{
		FailureInfo: models.FailureInfo{
			UnmatchedCalls: []models.UnmatchedCall{{
				Protocol:         "HTTP",
				ActualSummary:    "GET /v1/metrics",
				Destination:      "192.0.2.20",
				ClosestMock:      "mock-1",
				MatchPhase:       models.MatchPhaseSchema,
				DestinationScope: models.DestinationScopeNotInComparedSet,
				NextSteps:        "parser-supplied hint",
			}},
		},
	}

	var sb strings.Builder
	renderUnmatchedCalls(&sb, test)
	out := sb.String()

	if !strings.Contains(out, "targets 192.0.2.20;") {
		t.Errorf("suppression line must name the destination:\n%s", out)
	}
	if strings.Contains(out, "this destination") {
		t.Errorf("suppression line must not defer to a host it never prints:\n%s", out)
	}
}

// The likely-causes paragraph is identical for every out-of-scope miss and one
// out-of-scope container produces one miss per outgoing call it makes (23 of 28
// unmatched calls in the recording this was built from). It is written ONCE per
// test, not once per call.
func TestRenderUnmatchedCalls_CausesRenderedOncePerTest(t *testing.T) {
	var calls []models.UnmatchedCall
	for _, host := range []string{"192.0.2.20", "192.0.2.21", "192.0.2.22"} {
		calls = append(calls, models.UnmatchedCall{
			Protocol:         "HTTP",
			ActualSummary:    "GET /v1/metrics",
			Destination:      host,
			ClosestMock:      "mock-1",
			MatchPhase:       models.MatchPhaseSchema,
			DestinationScope: models.DestinationScopeNotInComparedSet,
			NextSteps:        "No recorded HTTP mock in the compared set targets " + host + ".",
		})
	}
	var sb strings.Builder
	renderUnmatchedCalls(&sb, models.TestResult{FailureInfo: models.FailureInfo{UnmatchedCalls: calls}})
	out := sb.String()

	if got := strings.Count(out, "Likely causes:"); got != 1 {
		t.Errorf("shared causes written %d times for 3 calls in one test, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, "ONE container per session"); got != 1 {
		t.Errorf("cause (1) written %d times, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "per-test mock window") {
		t.Errorf("the window-misrouting cause must reach the file report:\n%s", out)
	}
	// Each call still carries its own upstream.
	for _, host := range []string{"192.0.2.20", "192.0.2.21", "192.0.2.22"} {
		if !strings.Contains(out, "targets "+host+";") {
			t.Errorf("per-call destination %s missing:\n%s", host, out)
		}
	}
}

// ...and not written at all when no call was out of scope.
func TestRenderUnmatchedCalls_NoCausesWithoutAnOutOfScopeMiss(t *testing.T) {
	var sb strings.Builder
	renderUnmatchedCalls(&sb, models.TestResult{FailureInfo: models.FailureInfo{
		UnmatchedCalls: []models.UnmatchedCall{{
			Protocol:         "HTTP",
			ActualSummary:    "GET /api/orders",
			Destination:      "192.0.2.10",
			ClosestMock:      "mock-1",
			MatchPhase:       models.MatchPhaseSchema,
			DestinationScope: models.DestinationScopeInComparedSet,
			NextSteps:        "Request structure changed since recording.",
		}},
	}})
	if strings.Contains(sb.String(), "Likely causes:") {
		t.Errorf("shared causes must not appear without an out-of-scope miss:\n%s", sb.String())
	}
}
