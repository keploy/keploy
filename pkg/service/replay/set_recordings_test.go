package replay

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A call outside every test whose matcher found no recording to compare is
// told what its WHOLE set recorded of its shape, not what that matcher's pool
// held: the pool is one test's window and the session's, so an application's
// timer recorded only as it fired inside test-3 found none there when it
// fired between test-1 and test-2 on replay. It was told "no recording of
// this call in the set" with a step that records the set again whatever the
// application meant; the set has its recording, so the step holds only if the
// application changed the call on purpose. A call the set has truly no
// recording of is still told so, and one of a protocol keploy reads no shape
// of from a recording is told keploy cannot tell, never that there is none.
func TestRunTestSet_ACallOutsideEveryTestIsToldWhatItsWholeSetRecorded(t *testing.T) {
	tick := &models.Mock{Name: "mock-tick", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://clock:80/api/tick"}}}
	for _, c := range []struct {
		name     string
		call     models.UnmatchedCall
		recorded string
		facts    string
		ifMeant  bool
	}{
		{"recorded in another test's window", models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tick", Destination: "clock:80"},
			models.RecordedShape, "the set has recordings of this call's shape, and it matches none of them", true},
		{"recorded nowhere in the set", models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /api/tock", Destination: "clock:80"},
			models.RecordedNone, "no recording of this call in the set", false},
		{"a protocol keploy reads no shape of", models.UnmatchedCall{Protocol: "Redis", Command: "GET", Destination: "cache:6379"},
			"", "no recording came close to this call, and keploy cannot tell whether the set has one of its shape", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 3, 0)
			h.mocks.filtered = []*models.Mock{tick}
			logs := observeSetLogs(h)
			call := c.call
			call.Kind, call.Between, call.MatchPhase = models.MissOutsideTest, true, models.MatchPhaseNoMocks
			h.instr.mockErrorsFor = misses(nil, func() ([]models.UnmatchedCall, error) { return []models.UnmatchedCall{call}, nil })

			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Fatalf("status %s; a call outside every test that no mock answered fails the set", status)
			}
			lines := outsideLines(logs)
			if len(lines) != 1 {
				t.Fatalf("%d outside_every_test lines; want one: %+v", len(lines), logs.All())
			}
			f := lines[0].ContextMap()
			next := "record the test set again in its place: " + recordAgainOf(h, "test-set-0")
			if c.ifMeant {
				next = "if the application changed this call on purpose, " + next
			}
			for k, want := range map[string]string{"recorded": c.recorded, "facts": c.facts, "next_step": next, "closest": ""} {
				if got := f[k]; got != want {
					t.Errorf("line field %s = %v; want %q", k, got, want)
				}
			}
			if got := h.report.report.UnclaimedCalls; len(got) != 1 || got[0].SetRecorded != c.recorded {
				t.Errorf("the report's call %+v; want it to say the set read %q", got, c.recorded)
			}
		})
	}
}
