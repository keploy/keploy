package report

import (
	"bytes"
	"strings"
	"testing"

	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
)

func grpcStatusFailure(expected, actual int) models.TestResult {
	return models.TestResult{
		Kind:   models.GRPC_EXPORT,
		Status: models.TestStatusFailed,
		Result: models.Result{StatusCode: models.IntResult{Normal: false, Expected: expected, Actual: actual}},
	}
}

// A gRPC test that failed on its grpc-status shows the change, by code and
// name, in the report's status table and in JUnit: it used to show nothing.
func TestAGrpcStatusChangeIsShown(t *testing.T) {
	tr := grpcStatusFailure(0, 13)
	table := GenerateStatusAndHeadersTableDiff(tr)
	if !strings.Contains(table, "trailer.grpc-status") || !strings.Contains(table, "0 (OK)") || !strings.Contains(table, "13 (INTERNAL)") {
		t.Errorf("table diff:\n%s", table)
	}
	f := failureOf(tr, false)
	if f == nil || !strings.Contains(f.Text, "grpc-status: expected 0 (OK), got 13 (INTERNAL)") {
		t.Errorf("junit failure: %+v", f)
	}
	// An HTTP status is shown as before.
	h := models.TestResult{Kind: models.HTTP, Status: models.TestStatusFailed,
		Result: models.Result{StatusCode: models.IntResult{Expected: 200, Actual: 500}}}
	if table := GenerateStatusAndHeadersTableDiff(h); !strings.Contains(table, "Path: status_code") || strings.Contains(table, "grpc-status") {
		t.Errorf("HTTP table diff:\n%s", table)
	}
}

func TestGrpcStatusText(t *testing.T) {
	for code, want := range map[int]string{-1: "none or not a number", 0: "0 (OK)", 9: "9 (FAILED_PRECONDITION)", 16: "16 (UNAUTHENTICATED)", 42: "42", -5: "-5"} {
		if got := GrpcStatusText(code); got != want {
			t.Errorf("GrpcStatusText(%d) = %q, want %q", code, got, want)
		}
	}
}

// A failed status assertion that names no single status (status_code_in,
// _class, an unreadable value) shows what it asked for as the expected
// status, in the table and JUnit, for either kind: never "500 -> 500", and
// never nothing.
func TestAStatusAssertionShowsWhatItAskedFor(t *testing.T) {
	for _, c := range []struct {
		kind     models.Kind
		sc       models.IntResult
		expected string
		actual   string
	}{
		{models.HTTP, models.IntResult{Expected: 500, Actual: 500, ExpectedText: "one of 200, 201"}, "one of 200, 201", "500"},
		{models.HTTP, models.IntResult{Expected: 200, Actual: 500, ExpectedText: "4xx"}, "4xx", "500"},
		{models.GRPC_EXPORT, models.IntResult{Expected: 0, Actual: 0, ExpectedText: "one of 5, 7"}, "one of 5, 7", "0 (OK)"},
		{models.GRPC_EXPORT, models.IntResult{Expected: 5, Actual: 0}, "5 (NOT_FOUND)", "0 (OK)"},
		{models.GRPC_EXPORT, models.IntResult{Expected: 0, Actual: 0, ExpectedText: "5"}, "5 (NOT_FOUND)", "0 (OK)"},
		{models.GRPC_EXPORT, models.IntResult{Expected: -1, Actual: -1}, "none or not a number", "none or not a number"},
	} {
		tr := models.TestResult{Kind: c.kind, Status: models.TestStatusFailed, Result: models.Result{StatusCode: c.sc}}
		table := GenerateStatusAndHeadersTableDiff(tr)
		if !strings.Contains(table, "Expected: "+c.expected) || !strings.Contains(table, "Actual: "+c.actual) {
			t.Errorf("%s %+v: table\n%s", c.kind, c.sc, table)
		}
		if f := failureOf(tr, false); f == nil || !strings.Contains(f.Text, "expected "+c.expected+", got "+c.actual) {
			t.Errorf("%s %+v: junit %+v", c.kind, c.sc, f)
		}
		if e, a := statusTexts(tr); e != c.expected || a != c.actual {
			t.Errorf("%s %+v: statusTexts = %q, %q", c.kind, c.sc, e, a)
		}
	}
}

// --full shows a status as the compact table does: what the assertion asked
// for, a gRPC status by name.
func TestTheFullReportShowsTheStatusAsTheTableDoes(t *testing.T) {
	var buf bytes.Buffer
	printer := matcherUtils.NewDiffsPrinterOut(&buf, "t")
	tr := models.TestResult{Kind: models.GRPC_EXPORT, Status: models.TestStatusFailed,
		Result: models.Result{StatusCode: models.IntResult{Expected: 0, Actual: 0, ExpectedText: "one of 5, 7"}}}
	if err := (&Report{}).addStatusCodeDiffs(tr, &printer); err != nil {
		t.Fatal(err)
	}
	if err := printer.Render(); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "one of 5, 7") || !strings.Contains(out, "0 (OK)") {
		t.Fatalf("--full status:\n%s", out)
	}
}
