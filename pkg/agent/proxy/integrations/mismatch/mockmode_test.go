package mismatch

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Every report shape whose hint tells the user to record again.
func rerecordReports() map[string]*models.MockMismatchReport {
	return map[string]*models.MockMismatchReport{
		"no mocks": NewReport(ProtocolGeneric, "opaque exchange").WithPhase(models.MatchPhaseNoMocks, 0).Build(),
		"body values drifted": NewReport(ProtocolHTTP, "POST /reserve").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "body.qty", Kind: models.DiffKindValueChanged, Expected: "2", Actual: "1"}}).Build(),
		"other values drifted": NewReport(ProtocolHTTP, "POST /v2/reserve").
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "path", Kind: models.DiffKindValueChanged, Expected: "/reserve", Actual: "/v2/reserve"}}).Build(),
		"structure changed": NewReport(ProtocolHTTP, "GET /a").WithPhase(models.MatchPhaseSchema, 5).
			WithClosest("mock-1", []models.MockFieldDiff{{Path: "body.new_field", Kind: models.DiffKindMissingInMock, Actual: "1"}}).Build(),
	}
}

// A `keploy mock` run is recorded again with `keploy mock record`; pointing it
// at `keploy record` sends the user to a command that records something else.
func TestHintsNameKeployMockRecordInMockMode(t *testing.T) {
	SetMockMode(true)
	t.Cleanup(func() { SetMockMode(false) })
	for name, r := range rerecordReports() {
		if !strings.Contains(r.NextSteps, "'keploy mock record'") {
			t.Errorf("%s: a mock run's hint should name 'keploy mock record', got %q", name, r.NextSteps)
		}
		if strings.Contains(r.NextSteps, "'keploy record'") {
			t.Errorf("%s: a mock run's hint must not name 'keploy record', got %q", name, r.NextSteps)
		}
		if strings.Contains(r.NextSteps, "--update-test-mapping") {
			t.Errorf("%s: --update-test-mapping is a keploy test flag, not a keploy mock one: %q", name, r.NextSteps)
		}
		// keploy mock replay sends the agent no noise config, so noise advice cannot fix the miss.
		if strings.Contains(r.NextSteps, "globalNoise") {
			t.Errorf("%s: a mock run has no test.globalNoise to add to: %q", name, r.NextSteps)
		}
	}
}

// The exact mock-mode text, so a change to it is a deliberate one.
func TestHintTextInMockMode(t *testing.T) {
	SetMockMode(true)
	t.Cleanup(func() { SetMockMode(false) })
	want := map[string]string{
		"no mocks":             "No recorded mocks were available to match against for this protocol in the selected mock set. Re-record the mock set with 'keploy mock record'.",
		"body values drifted":  "Only values drifted (body.qty). If the change is expected, re-record the mock set with 'keploy mock record', or capture the new calls with 'keploy mock replay --on-miss record'.",
		"other values drifted": "Only values drifted (path). If the change is expected, re-record the mock set with 'keploy mock record', or capture the new calls with 'keploy mock replay --on-miss record'.",
		"structure changed":    "Request structure changed since recording. Re-record the mock set with 'keploy mock record'.",
	}
	for name, r := range rerecordReports() {
		if r.NextSteps != want[name] {
			t.Errorf("%s: mock-mode hint:\n got  %q\n want %q", name, r.NextSteps, want[name])
		}
	}
	if got := RecordCommand(); got != "keploy mock record" {
		t.Errorf("RecordCommand in mock mode = %q", got)
	}
}

// Everything that is not a mock run keeps the exact hint it had, including
// after mock mode was on, so a flag left over from one setup cannot leak into
// a later one.
func TestHintsAreUnchangedOutsideMockMode(t *testing.T) {
	SetMockMode(true)
	SetMockMode(false)
	if got := RecordCommand(); got != "keploy record" {
		t.Errorf("RecordCommand outside mock mode = %q", got)
	}
	want := map[string]string{
		"no mocks":             "No recorded mocks were available to match against for this protocol in the selected test set. Re-record the test set with 'keploy record'.",
		"body values drifted":  "Only values drifted (body.qty). If these are dynamic (timestamps, ids, tokens), add the request-body fields under test.globalNoise.requestbody with root-relative keys (e.g. requestbody: {qty: []}); otherwise re-record with 'keploy record'.",
		"other values drifted": "Only values drifted (path). If these are dynamic (timestamps, ids, tokens), add them to the matching noise (test.globalNoise); otherwise re-record with 'keploy record'.",
		"structure changed":    "Request structure changed since recording. Re-record the test set with 'keploy record', or refresh mappings with --update-test-mapping if mocks were edited.",
	}
	for name, r := range rerecordReports() {
		if r.NextSteps != want[name] {
			t.Errorf("%s: hint changed outside mock mode:\n got  %q\n want %q", name, r.NextSteps, want[name])
		}
	}
}
