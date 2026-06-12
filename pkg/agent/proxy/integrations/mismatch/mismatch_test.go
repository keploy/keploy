package mismatch

import (
	"net/http"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestBuilder_RendersFieldDiffsAndDefaults(t *testing.T) {
	report := NewReport(ProtocolHTTP, "POST /orders").
		WithPhase(models.MatchPhaseExhausted, 3).
		WithClosest("mock-7", []models.MockFieldDiff{
			{Path: "body.created_at", Kind: models.DiffKindValueChanged, Expected: "2026-01-01", Actual: "2026-06-12"},
		}).Build()

	if report.Protocol != ProtocolHTTP || report.ActualSummary != "POST /orders" {
		t.Fatalf("identity fields wrong: %+v", report)
	}
	if report.MatchPhase != models.MatchPhaseExhausted || report.CandidateCount != 3 {
		t.Errorf("phase fields wrong: %+v", report)
	}
	if report.ClosestMock != "mock-7" {
		t.Errorf("closest mock wrong: %q", report.ClosestMock)
	}
	if !strings.Contains(report.Diff, "body.created_at") {
		t.Errorf("Diff should render field paths, got %q", report.Diff)
	}
	// Pure value drift → next steps must suggest noise, never a nonexistent command.
	if !strings.Contains(report.NextSteps, "noise") {
		t.Errorf("value-only drift should suggest noise, got %q", report.NextSteps)
	}
	if strings.Contains(report.NextSteps, "keploy rerecord") {
		t.Errorf("next steps must not reference the nonexistent 'keploy rerecord' command: %q", report.NextSteps)
	}
}

func TestBuilder_NoMocksPhase(t *testing.T) {
	report := NewReport(ProtocolGeneric, "opaque exchange").
		WithPhase(models.MatchPhaseNoMocks, 0).Build()
	if !strings.Contains(report.NextSteps, "keploy record") {
		t.Errorf("no-mocks phase should point at 'keploy record', got %q", report.NextSteps)
	}
}

func TestBuilder_StructuralChangeNextSteps(t *testing.T) {
	report := NewReport(ProtocolHTTP, "GET /a").
		WithPhase(models.MatchPhaseSchema, 5).
		WithClosest("mock-1", []models.MockFieldDiff{
			{Path: "body.new_field", Kind: models.DiffKindMissingInMock, Actual: "1"},
		}).Build()
	if !strings.Contains(report.NextSteps, "keploy record") {
		t.Errorf("structural change should suggest re-record, got %q", report.NextSteps)
	}
}

func TestBuilder_FieldDiffCap(t *testing.T) {
	diffs := make([]models.MockFieldDiff, maxFieldDiffs+10)
	for i := range diffs {
		diffs[i] = models.MockFieldDiff{Path: "body.x", Kind: models.DiffKindValueChanged}
	}
	report := NewReport(ProtocolHTTP, "GET /a").WithClosest("m", diffs).Build()
	if len(report.FieldDiffs) != maxFieldDiffs {
		t.Errorf("expected diff cap %d, got %d", maxFieldDiffs, len(report.FieldDiffs))
	}
}

func TestJSONBodyDiffs_RespectsIgnoreAndReportsValues(t *testing.T) {
	recorded := `{"id":"abc","ts":"111","nested":{"v":1}}`
	live := `{"id":"abc","ts":"222","nested":{"v":2}}`

	diffs := JSONBodyDiffs(recorded, live, map[string][]string{"ts": {}})
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff (ts ignored), got %d: %+v", len(diffs), diffs)
	}
	d := diffs[0]
	if d.Path != "body.nested.v" || d.Kind != models.DiffKindValueChanged || d.Expected != "1" || d.Actual != "2" {
		t.Errorf("unexpected diff: %+v", d)
	}
}

func TestHeaderKeyDiffs_SkipsNoiseAndKeployHeaders(t *testing.T) {
	recorded := map[string]string{
		"Authorization":  "sig",      // in noise → skipped
		"X-Custom":       "v",        // missing in live → reported
		"Keploy-Test-Id": "test-1",   // keploy header → skipped
		"Content-Type":   "app/json", // present both → not reported
	}
	live := http.Header{
		"Content-Type": {"app/json"},
		"X-New":        {"n"}, // missing in recording → reported
	}
	noise := map[string][]string{"authorization": {}}

	diffs := HeaderKeyDiffs(recorded, live, noise)
	got := map[string]models.MockFieldDiffKind{}
	for _, d := range diffs {
		got[d.Path] = d.Kind
	}
	if got["header.X-Custom"] != models.DiffKindMissingInLive {
		t.Errorf("expected header.X-Custom missing_in_live, got %+v", diffs)
	}
	if got["header.X-New"] != models.DiffKindMissingInMock {
		t.Errorf("expected header.X-New missing_in_mock, got %+v", diffs)
	}
	if _, ok := got["header.Authorization"]; ok {
		t.Errorf("noised header must not be reported: %+v", diffs)
	}
	if _, ok := got["header.Keploy-Test-Id"]; ok {
		t.Errorf("keploy header must not be reported: %+v", diffs)
	}
}

func TestQueryParamDiffs(t *testing.T) {
	recorded := map[string][]string{"page": {"1"}, "old": {"x"}}
	live := map[string][]string{"page": {"2"}, "new": {"y"}}

	diffs := QueryParamDiffs(recorded, live)
	got := map[string]models.MockFieldDiff{}
	for _, d := range diffs {
		got[d.Path] = d
	}
	if d := got["query.page"]; d.Kind != models.DiffKindValueChanged || d.Expected != "1" || d.Actual != "2" {
		t.Errorf("query.page diff wrong: %+v", d)
	}
	if d := got["query.old"]; d.Kind != models.DiffKindMissingInLive {
		t.Errorf("query.old diff wrong: %+v", d)
	}
	if d := got["query.new"]; d.Kind != models.DiffKindMissingInMock {
		t.Errorf("query.new diff wrong: %+v", d)
	}
}

func TestRenderFieldDiffs_AllKinds(t *testing.T) {
	out := RenderFieldDiffs([]models.MockFieldDiff{
		{Path: "body.a", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "2"},
		{Path: "header.B", Kind: models.DiffKindMissingInLive, Expected: "x"},
		{Path: "query.c", Kind: models.DiffKindMissingInMock, Actual: "y"},
		{Path: "body.d", Kind: models.DiffKindTypeChanged, Expected: "string: s", Actual: "number: 1"},
	})
	for _, want := range []string{"body.a", "header.B", "query.c", "body.d", "absent in live", "absent in recording", "type changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered diff missing %q: %s", want, out)
		}
	}
}
