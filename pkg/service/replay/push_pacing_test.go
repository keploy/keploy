package replay

// RunTestSet-level tests for server-push pacing: the staging call's recorded
// windows (O1) and the prune/mapping attribution of mocks consumed outside
// their own window (§8).

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestRunTestSetStagesEveryRecordedWindow: the staging call carries the window of
// EVERY recorded test — the deselected and ignored ones too, since the traffic a
// deselected test's handler produced is still released at that test's place in
// the recording — and the per-test calls carry none.
func TestRunTestSetStagesEveryRecordedWindow(t *testing.T) {
	h := newPartialRunHarness(t, 4, 0)
	h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-2", "test-3"}}
	h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-3"}}

	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %q, want PASSED", status)
	}

	h.instr.mu.Lock()
	params := append([]models.MockFilterParams(nil), h.instr.allParams...)
	h.instr.mu.Unlock()

	var staging []models.MockFilterParams
	perTest := 0
	for _, p := range params {
		if p.AfterTime.Equal(models.BaseTime) {
			staging = append(staging, p)
			continue
		}
		perTest++
		if len(p.RecordedWindows) != 0 {
			t.Fatalf("a per-test call carried %d recorded windows; they belong to the staging call only",
				len(p.RecordedWindows))
		}
	}
	if len(staging) != 1 {
		t.Fatalf("%d staging calls, want 1", len(staging))
	}
	if perTest != 1 {
		t.Fatalf("%d per-test calls, want 1 (test-2 only)", perTest)
	}
	got := staging[0].RecordedWindows
	if len(got) != len(h.cases) {
		t.Fatalf("staging carried %d recorded windows, want %d (every recorded test, selected or not)",
			len(got), len(h.cases))
	}
	for i, tc := range h.cases {
		w := got[i]
		if w.TestCase != tc.Name || !w.Start.Equal(tc.HTTPReq.Timestamp) || !w.End.Equal(tc.HTTPResp.Timestamp) {
			t.Fatalf("window %d = %+v, want %s [%v, %v]", i, w, tc.Name, tc.HTTPReq.Timestamp, tc.HTTPResp.Timestamp)
		}
	}
	if !staging[0].FirstRecordedTestStart.Equal(h.cases[0].HTTPReq.Timestamp) {
		t.Fatal("the startup cutoff seed moved")
	}
}

func TestRecordedTestWindowsSkipsCasesWithoutAWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	cases := []*models.TestCase{
		{Name: "http", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: t0}, HTTPResp: models.HTTPResp{Timestamp: t0.Add(time.Second)}},
		{Name: "grpc", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Timestamp: t0.Add(2 * time.Second)}, GrpcResp: models.GrpcResp{Timestamp: t0.Add(3 * time.Second)}},
		{Name: "no-ts", Kind: models.HTTP},
		nil,
	}
	got := recordedTestWindows(cases)
	if len(got) != 2 || got[0].TestCase != "http" || got[1].TestCase != "grpc" || !got[1].End.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("recordedTestWindows = %+v", got)
	}
}
