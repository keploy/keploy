package replay

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// printedTo is what fn writes on a console of its own.
func printedTo(fn func(w io.Writer)) string {
	var b bytes.Buffer
	fn(&b)
	return b.String()
}

// syncBuffer is a bytes.Buffer safe for a run's goroutines and the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// consoleOf runs fn with r's console one of its own (Replayer.SetConsole), as
// keploy's CLI gives it stdout, and returns what the run wrote there.
func consoleOf(r *Replayer, fn func()) string {
	var b syncBuffer
	r.SetConsole(&b)
	defer r.SetConsole(nil)
	fn()
	return b.String()
}

// stdoutConsole is the console keploy's CLI gives a run (os.Stdout,
// cli/provider), resolved as each line is written, so captureStdout's
// redirect and a capture of file descriptor 1 both see it.
type stdoutConsole struct{}

func (stdoutConsole) Write(p []byte) (int, error) { return os.Stdout.Write(p) }

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
	out := printedTo(func(w io.Writer) { printMismatchReport(w, outOfScopeReport()) })

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

	out := printedTo(func(w io.Writer) { printMismatchReport(w, r) })

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

	out := printedTo(tfs.PrintFailuresTable)

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

	out := printedTo(tfs.PrintFailuresTable)
	if strings.Contains(out, "Likely causes:") {
		t.Errorf("shared causes must not appear without an out-of-scope miss:\n%s", out)
	}
}

// A variable that did not hold is printed as the diff naming the variable,
// its value in this test and what the call sent; never as a request diff
// against the recording, which holds neither.
func TestPrintMismatchReport_AVariableThatDidNotHold(t *testing.T) {
	r := &models.MockMismatchReport{
		Protocol: "HTTP", ActualSummary: "GET /orders/o9", ClosestMock: "mock-2", MatchPhase: models.MatchPhaseVariable,
		Diff:           `mock mock-2: variable orderId is "o1" in this test (taken at mock-1 body.id), and this call sent "o9" at url.path[1]`,
		ClosestMockReq: "GET /orders/r1 HTTP/1.1\r\n\r\n", ReceivedReq: "GET /orders/o9 HTTP/1.1\r\n\r\n",
		FieldDiffs: []models.MockFieldDiff{{Path: "url.path[1]", Kind: models.DiffKindVariable, Expected: "o1", Actual: "o9"}},
		NextSteps:  "fix the application",
	}
	out := printedTo(func(w io.Writer) { printMismatchReport(w, r) })
	if !strings.Contains(out, `variable orderId is "o1" in this test`) || strings.Contains(out, "/orders/r1") {
		t.Errorf("printed:\n%s", out)
	}
}

// The console's mismatch table names a missed call whole, beside the path tag
// the agent's miss line names it by, and its Hint quotes keploy's advice
// whole: the sessionCalls entry's pathPrefix, which the line names only by
// pointing here with that tag. The report's call carries the same tag beside
// its summary. A reader of the line finds the call by its tag.
func TestTheConsoleNamesTheMissBesideTheTagItsLogLineCarries(t *testing.T) {
	// The agent's report, tagged where its miss is logged (the proxy's
	// funnel), and the call the agent hands the CLI for it.
	r := &models.MockMismatchReport{
		Protocol: "HTTP", ActualSummary: "POST /oauth/token", Destination: "127.0.0.1:9096",
		NextSteps: `Each test is served the HTTP calls it recorded. Declare it in keploy.yaml (sessionCalls: [{host: "127.0.0.1:9096", method: "POST", pathPrefix: "/oauth/token"}]), or record again.`,
	}
	r.PathTag = models.CallPathTag(models.StartLogKeySession(context.Background()), r.Protocol, r.ActualSummary)
	tag := r.PathTag
	if tag == "" {
		t.Fatalf("precondition: the report's tag is %q", tag)
	}
	call := models.UnmatchedCall{Protocol: r.Protocol, ActualSummary: r.ActualSummary, Destination: r.Destination, NextSteps: r.NextSteps, PathTag: r.PathTag}
	tfs := NewTestFailureStore()
	tfs.AddUnmatchedCallForTest("test-set-0", "get-profile-1", call)
	failures := tfs.GetFailures()
	if len(failures) != 1 || failures[0].MismatchReport.PathTag != tag {
		t.Fatalf("the console's report of the call carries %+v, want tag %s", failures, tag)
	}
	out := printedTo(func(w io.Writer) { printMismatchReport(w, failures[0].MismatchReport) })
	for _, want := range []string{
		"  Mock mismatch: [HTTP] POST /oauth/token [path tag " + tag + "]  →  127.0.0.1:9096",
		`  Hint: Each test is served the HTTP calls it recorded. Declare it in keploy.yaml (sessionCalls: [{host: "127.0.0.1:9096", method: "POST", pathPrefix: "/oauth/token"}]), or record again.`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the console's table is\n%s\nwant it to hold %q", out, want)
		}
	}
	// The agent's line: the shape and the tag, its advice pointing here.
	line := models.MissLogShape(r) + " " + models.MissLogNextStep(r)
	if strings.Contains(line, "oauth") || !strings.Contains(line, "POST /{}/{}") ||
		!strings.Contains(line, `pathPrefix: <the path keploy's console Hint and report name for [path tag `+tag+`]>`) {
		t.Errorf("the miss line names the call as %q, want its shape and a pointer to the Hint by tag %s", line, tag)
	}
}

// The console's mismatch table names a call's destination whole beside the
// host tag the agent's miss line named it by ({}.{}.{}:443 #tag there), and a
// DNS question, which has no destination, beside its tag after its summary:
// a reader of the log finds the host a tag names here. So does the line for
// a call an earlier test set made.
func TestTheMismatchTableNamesADestinationBesideItsHostTag(t *testing.T) {
	out := printedTo(func(w io.Writer) {
		printMismatchReport(w, &models.MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /v1/items", Destination: "api.example.com:443", HostTag: "#3fa2c1d0e9b8"})
	})
	if !strings.Contains(out, "  Mock mismatch: [HTTP] GET /v1/items  →  api.example.com:443 [host tag #3fa2c1d0e9b8]") {
		t.Errorf("the table names the destination as\n%s", out)
	}
	out = printedTo(func(w io.Writer) {
		printMismatchReport(w, &models.MockMismatchReport{Protocol: "DNS", ActualSummary: "A tenant.example.com.", HostTag: "#0a1b2c3d4e5f"})
	})
	if !strings.Contains(out, "  Mock mismatch: [DNS] A tenant.example.com. [host tag #0a1b2c3d4e5f]") {
		t.Errorf("the table names the DNS question as\n%s", out)
	}
	out = printedTo(func(w io.Writer) {
		sayEarlierSetCall(w, earlierSetCall{set: "test-set-a", call: models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /orders", Destination: "orders.internal:80", HostTag: "#3fa2c1d0e9b8"}})
	})
	if !strings.Contains(out, "  →  orders.internal:80 [host tag #3fa2c1d0e9b8]") {
		t.Errorf("the earlier set's call names its destination as\n%s", out)
	}
}
