package replay

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	out := <-done
	_ = r.Close()
	return out
}

// outOfScopeReport is the reported shape: the matcher's least-distant mock
// is on a DIFFERENT upstream, so every field of it differs for reasons that
// have nothing to do with the miss.
func outOfScopeReport() *models.MockMismatchReport {
	return &models.MockMismatchReport{
		Protocol:         "HTTP",
		ActualSummary:    "GET /v1/metrics",
		Destination:      "192.0.2.20",
		ClosestMock:      "mock-1",
		MatchPhase:       models.MatchPhaseSchema,
		DestinationScope: models.DestinationScopeNotInComparedSet,
		CandidateCount:   19,
		FieldDiffs: []models.MockFieldDiff{
			{Path: "path", Kind: models.DiffKindValueChanged, Expected: "/api/orders", Actual: "/v1/metrics"},
		},
		ClosestMockReq: "GET /api/orders\nHost: 192.0.2.10\n\n",
		ReceivedReq:    "GET /v1/metrics\nHost: 192.0.2.20\n\n",
		NextSteps:      "No recorded HTTP mock in the compared set targets 192.0.2.20. ...",
	}
}

// A diff against a mock on another upstream is not evidence, it is an
// invitation to reconcile two unrelated requests. The CLI must not lead with
// it — and must say why it is missing, so its absence reads as an answer.
func TestPrintMismatchReport_SuppressesClosestMockWhenOutOfScope(t *testing.T) {
	out := captureStdout(t, func() { printMismatchReport(outOfScopeReport()) })

	if strings.Contains(out, "mock-1") {
		t.Errorf("closest mock on a different upstream must not be rendered:\n%s", out)
	}
	if strings.Contains(out, "/api/orders") {
		t.Errorf("diff against a different upstream must not be rendered:\n%s", out)
	}
	if !strings.Contains(out, "closest-mock diff omitted") {
		t.Errorf("suppression must be explained, not silent:\n%s", out)
	}
	// ...in six words. The hint printed immediately below already opens with
	// "No recorded HTTP mock in the compared set targets 192.0.2.20."; a full
	// second copy of that sentence, once per missed call, was duplication.
	if strings.Count(out, "in the compared set targets") != 1 {
		t.Errorf("the suppression line must not restate the hint's first sentence:\n%s", out)
	}
	// The facts that ARE about this miss stay: which upstream, how far the
	// cascade got, and the guidance.
	for _, want := range []string{"192.0.2.20", "match stopped at: no_schema_candidates", "in the compared set targets"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
}

// The suppression is keyed to the verdict alone: an ordinary miss keeps the
// full side-by-side diff it has always had.
func TestPrintMismatchReport_KeepsDiffForInScopeMiss(t *testing.T) {
	r := outOfScopeReport()
	r.DestinationScope = models.DestinationScopeInComparedSet
	r.Destination = "192.0.2.10"
	r.NextSteps = "Request structure changed since recording."

	out := captureStdout(t, func() { printMismatchReport(r) })

	if !strings.Contains(out, "mock-1") {
		t.Errorf("in-scope miss must still name its closest mock:\n%s", out)
	}
	if strings.Contains(out, "closest-mock diff omitted") {
		t.Errorf("in-scope miss must not suppress its diff:\n%s", out)
	}
}

// The likely-causes paragraph is identical for every out-of-scope miss and an
// out-of-scope container produces one miss per outgoing call it makes. It is
// printed ONCE for the test, after that test's calls — not once per call, which
// is what put the same ~1 KB of prose on screen dozens of times.
func TestPrintFailuresTable_CausesRenderedOncePerTest(t *testing.T) {
	tfs := NewTestFailureStore()
	for _, host := range []string{"192.0.2.20", "192.0.2.21", "192.0.2.22"} {
		r := outOfScopeReport()
		r.Destination = host
		r.NextSteps = "No recorded HTTP mock in the compared set targets " + host + "."
		tfs.failures = append(tfs.failures, TestFailure{
			TestSetID:      "test-set-0",
			TestID:         "test-1",
			FailureReason:  models.ErrMockNotFound,
			MismatchReport: r,
		})
	}

	out := captureStdout(t, tfs.PrintFailuresTable)

	// Every call keeps its own one-line, call-specific hint.
	for _, host := range []string{"192.0.2.20", "192.0.2.21", "192.0.2.22"} {
		if !strings.Contains(out, "compared set targets "+host) {
			t.Errorf("per-call hint for %s missing:\n%s", host, out)
		}
	}
	// The shared block appears exactly once for the three of them.
	if got := strings.Count(out, "Likely causes:"); got != 1 {
		t.Errorf("shared causes rendered %d times for 3 calls in one test, want 1:\n%s", got, out)
	}
	if got := strings.Count(out, "ONE container per session"); got != 1 {
		t.Errorf("cause (1) rendered %d times, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "per-test mock window") {
		t.Errorf("the window-misrouting cause must reach the CLI:\n%s", out)
	}
}

// ...and it is not printed at all for a test with no out-of-scope miss.
func TestPrintFailuresTable_NoCausesWithoutAnOutOfScopeMiss(t *testing.T) {
	r := outOfScopeReport()
	r.DestinationScope = models.DestinationScopeInComparedSet
	r.NextSteps = "Request structure changed since recording."
	tfs := NewTestFailureStore()
	tfs.failures = append(tfs.failures, TestFailure{
		TestSetID:      "test-set-0",
		TestID:         "test-1",
		FailureReason:  models.ErrMockNotFound,
		MismatchReport: r,
	})

	out := captureStdout(t, tfs.PrintFailuresTable)
	if strings.Contains(out, "Likely causes:") {
		t.Errorf("shared causes must not appear without an out-of-scope miss:\n%s", out)
	}
}
