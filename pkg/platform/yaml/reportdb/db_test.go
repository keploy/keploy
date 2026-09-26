package reportdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/safeyaml"
	"go.uber.org/zap"
)

// writeReport drops <run>/<set>-report.yaml under a fresh reports dir and
// returns the reports dir.
func writeReport(t *testing.T, run, set, body string) string {
	t.Helper()
	dir := t.TempDir()
	runDir := filepath.Join(dir, run)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, set+"-report.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestGetReportReadsAValidReport is the unchanged-behaviour anchor: a normal
// report still decodes.
func TestGetReportReadsAValidReport(t *testing.T) {
	body := "version: api.keploy.io/v1beta1\nname: test-set-0-report\nstatus: PASSED\nsuccess: 2\ntotal: 2\n"
	dir := writeReport(t, "test-run-0", "test-set-0", body)
	rdb := New(zap.NewNop(), dir)
	rep, err := rdb.GetReport(context.Background(), "test-run-0", "test-set-0")
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if rep.Status != "PASSED" || rep.Success != 2 {
		t.Fatalf("GetReport = %+v", rep)
	}
}

// TestGetReportRefusesNonRegular: a report symlinked to /dev/zero (which a
// cloned repo can commit) or that is a FIFO once ran `keploy report`/`status`
// out of memory or blocked it; it must now return promptly with an error.
func TestGetReportRefusesNonRegular(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs or /dev/zero on windows")
	}
	cases := map[string]func(t *testing.T, path string){
		"fifo": func(t *testing.T, path string) { mkfifo(t, path) },
		"devzero": func(t *testing.T, path string) {
			if _, err := os.Stat("/dev/zero"); err != nil {
				t.Skip("no /dev/zero")
			}
			if err := os.Symlink("/dev/zero", path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			runDir := filepath.Join(dir, "test-run-0")
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			plant(t, filepath.Join(runDir, "test-set-0-report.yaml"))
			rdb := New(zap.NewNop(), dir)
			done := make(chan error, 1)
			go func() {
				_, err := rdb.GetReport(context.Background(), "test-run-0", "test-set-0")
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, safeyaml.ErrNotRegular) {
					t.Fatalf("GetReport on a %s report = %v, want ErrNotRegular", name, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("GetReport blocked on a %s report", name)
			}
		})
	}
}

// A report larger than its bound is refused by its size, before a byte of it
// is read. The bound here is a MiB, not reportBytes: were the size not
// checked, the test would read a MiB, not a GiB. (Sparse: the file costs the
// test nothing.) Mutation: drop the size check.
func TestGetReportRefusesOversized(t *testing.T) {
	const limit = 1 << 20
	dir := writeReport(t, "test-run-0", "test-set-0", "")
	if err := os.Truncate(filepath.Join(dir, "test-run-0", "test-set-0-report.yaml"), limit+1); err != nil {
		t.Fatal(err)
	}
	rdb := New(zap.NewNop(), dir)
	if _, err := rdb.readReport(context.Background(), "test-run-0", "test-set-0", limit); !safeyaml.IsRefused(err) {
		t.Fatalf("readReport of a report past %d bytes = %v, want refused", limit, err)
	}
}

// keploy reads back a large report of the shape it writes: one of 1,200 tests
// with 20 KB JSON bodies, which the matcher records as expected and actual
// too, comes to 75 MB, within reportBytes (512 MiB); a 64 MiB limit once
// refused it. Mutation: bound reports at 64 MiB again.
func TestGetReportReadsALargeReportKeployWrites(t *testing.T) {
	if raceEnabled {
		t.Skip("too slow under the race runtime, and it races nothing")
	}
	var body strings.Builder
	body.WriteString(`{"items":[`)
	for i := 0; body.Len() < 20<<10; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, `{"id":%d,"name":"item-%d","price":%d.99,"tags":["a","b"]}`, i, i, i)
	}
	body.WriteString("]}")
	js := body.String()
	const tests = 1200
	rep := &models.TestReport{Version: "api.keploy.io/v1beta1", Status: "PASSED", Total: tests, Success: tests, TestSet: "test-set-0"}
	for i := 0; i < tests; i++ {
		rep.Tests = append(rep.Tests, models.TestResult{
			Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusPassed, TestCaseID: fmt.Sprintf("test-%d", i),
			Req:    models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://localhost:8080/items?page=%d", i), Header: map[string]string{"Accept": "application/json"}},
			Res:    models.HTTPResp{StatusCode: 200, Body: js, Header: map[string]string{"Content-Type": "application/json"}},
			Result: models.Result{BodyResult: []models.BodyResult{{Normal: true, Type: models.JSON, Expected: js, Actual: js}}},
		})
	}
	dir := t.TempDir()
	db := New(zap.NewNop(), dir)
	if err := db.InsertReport(context.Background(), "test-run-0", "test-set-0", rep); err != nil {
		t.Fatal(err)
	}
	rep = nil
	st, err := os.Stat(filepath.Join(dir, "test-run-0", "test-set-0-report.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() <= 64<<20 {
		t.Fatalf("the report is %d bytes; this test needs one past 64 MiB", st.Size())
	}
	got, err := db.GetReport(context.Background(), "test-run-0", "test-set-0")
	if err != nil {
		t.Fatalf("GetReport of a %d-byte report keploy wrote: %v", st.Size(), err)
	}
	if len(got.Tests) != tests || got.Tests[tests-1].Res.Body != js || got.Tests[0].Result.BodyResult[0].Actual != js {
		t.Fatalf("GetReport read %d tests, not the %d written", len(got.Tests), tests)
	}
}

// keploy reads back a report past 128 MiB: this one, 46 tests each with a 1 MB
// response body that the body result holds as expected and actual too, is
// 138 MiB. Mutation: bound reports at 128 MiB again.
func TestGetReportReadsAReportPast128MiB(t *testing.T) {
	if raceEnabled {
		t.Skip("too slow under the race runtime, and it races nothing")
	}
	body := strings.Repeat("a", 1<<20)
	const tests = 46
	rep := &models.TestReport{Version: "api.keploy.io/v1beta1", Status: "PASSED", Total: tests, Success: tests, TestSet: "test-set-0"}
	for i := 0; i < tests; i++ {
		rep.Tests = append(rep.Tests, models.TestResult{
			Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusPassed, TestCaseID: fmt.Sprintf("test-%d", i),
			Req:    models.HTTPReq{Method: "GET", URL: fmt.Sprintf("http://localhost:8080/blob?page=%d", i)},
			Res:    models.HTTPResp{StatusCode: 200, Body: body},
			Result: models.Result{BodyResult: []models.BodyResult{{Normal: true, Type: models.Plain, Expected: body, Actual: body}}},
		})
	}
	dir := t.TempDir()
	db := New(zap.NewNop(), dir)
	if err := db.InsertReport(context.Background(), "test-run-0", "test-set-0", rep); err != nil {
		t.Fatal(err)
	}
	rep = nil
	st, err := os.Stat(filepath.Join(dir, "test-run-0", "test-set-0-report.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() <= 128<<20 {
		t.Fatalf("the report is %d bytes; this test needs one past 128 MiB", st.Size())
	}
	got, err := db.GetReport(context.Background(), "test-run-0", "test-set-0")
	if err != nil {
		t.Fatalf("GetReport of a %d-byte report keploy wrote: %v", st.Size(), err)
	}
	if len(got.Tests) != tests || got.Tests[tests-1].Res.Body != body || got.Tests[0].Result.BodyResult[0].Actual != body {
		t.Fatalf("GetReport read %d tests, not the %d written", len(got.Tests), tests)
	}
}
