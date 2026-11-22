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
				// A link to /dev/zero is a link out of the recording, refused
				// before anything is opened; a FIFO in it is no regular file.
				if name == "devzero" && (err == nil || !strings.Contains(err.Error(), "only inside")) {
					t.Fatalf("GetReport on a %s report = %v, want it refused as outside the recording", name, err)
				}
				if name != "devzero" && !errors.Is(err, safeyaml.ErrNotRegular) {
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

// A report whose file, or whose test run's folder, leads out of the recording
// is refused, with why, and what it leads to is left as it was.
func TestAReportIsWrittenOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	link := func(t *testing.T, target, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(filepath.Dir(path), target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, path); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
			t.Fatalf("InsertReport: %v, want it refused as outside the recording", err)
		}
	}
	t.Run("the report's file links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy", "reports")
		out := filepath.Join(base, "outside.yaml")
		if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(root, "test-run-0", "set-report.yaml")
		link(t, out, file)
		refused(t, New(zap.NewNop(), root).InsertReport(ctx, "test-run-0", "set", &models.TestReport{}))
		if got, err := os.ReadFile(out); err != nil || string(got) != "KEEP ME\n" {
			t.Fatalf("the file outside holds %q (%v)", got, err)
		}
		if info, err := os.Lstat(file); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link is gone: %v, %v", info, err)
		}
	})
	t.Run("the run's folder links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy", "reports")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		link(t, out, filepath.Join(root, "test-run-0"))
		refused(t, New(zap.NewNop(), root).InsertReport(ctx, "test-run-0", "set", &models.TestReport{}))
		refused(t, New(zap.NewNop(), root).UpdateReport(ctx, "test-run-0", map[string]int{"covered": 1}))
		if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
			t.Fatalf("the report left %v (%v) outside the folder", entries, err)
		}
	})
}

// The reports folder is a folder of the recording, not a recording of its
// own: a keploy/reports that a cloned repository carries as a link out of
// keploy, to elsewhere in the project or out of it, is refused, with why, by
// every write and read of a report, and by the listing of test runs. Nothing
// is written where it leads, and no report there is read. A keploy folder
// that links out of the project takes its reports folder with it, and is
// refused too.
func TestAReportsFolderThatLinksOutIsRefused(t *testing.T) {
	ctx := context.Background()
	// plant makes base/project/keploy and links reports, or keploy itself,
	// to a folder holding a report of test-run-0 that names OUTSIDE-SECRET.
	plant := func(t *testing.T, linkKeploy bool, target func(base string) string) (keploy, out string) {
		t.Helper()
		base := t.TempDir()
		out = target(base)
		if err := os.MkdirAll(filepath.Join(out, "test-run-0"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "test-run-0", "set-report.yaml"), []byte("name: OUTSIDE-SECRET\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		keploy = filepath.Join(base, "project", "keploy")
		at, to := filepath.Join(keploy, "reports"), out
		if linkKeploy {
			// keploy -> the folder; its reports folder is the one out there.
			at, to = keploy, filepath.Dir(out)
			if err := os.Rename(out, filepath.Join(to, "reports")); err != nil {
				t.Fatal(err)
			}
			out = filepath.Join(to, "reports")
		}
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(filepath.Dir(at), to)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, at); err != nil {
			t.Skipf("cannot make a symbolic link here: %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(keploy, "reports", "test-run-0", "set-report.yaml")); err != nil || !strings.Contains(string(b), "OUTSIDE-SECRET") {
			t.Fatalf("the link does not reach the report: %q, %v", b, err)
		}
		return keploy, out
	}
	for name, tc := range map[string]struct {
		linkKeploy bool
		target     func(base string) string
	}{
		"reports out of the project": {target: func(base string) string { return filepath.Join(base, "outside") }},
		"reports out of keploy":      {target: func(base string) string { return filepath.Join(base, "project", "elsewhere") }},
		"keploy out of the project":  {linkKeploy: true, target: func(base string) string { return filepath.Join(base, "outside", "x") }},
	} {
		t.Run(name, func(t *testing.T) {
			keploy, out := plant(t, tc.linkKeploy, tc.target)
			db := New(zap.NewNop(), filepath.Join(keploy, "reports"))
			refused := func(what string, err error) {
				t.Helper()
				if err == nil || !strings.Contains(err.Error(), "only inside") {
					t.Fatalf("%s: %v, want it refused as outside the recording", what, err)
				}
			}
			refused("InsertReport", db.InsertReport(ctx, "test-run-1", "set", &models.TestReport{}))
			refused("UpdateReport", db.UpdateReport(ctx, "test-run-1", map[string]int{"covered": 1}))
			if _, err := os.Lstat(filepath.Join(out, "test-run-1")); !os.IsNotExist(err) {
				t.Fatalf("a report was written where the link leads: %v", err)
			}
			got, err := db.GetReport(ctx, "test-run-0", "set")
			if err == nil || (got != nil && got.Name == "OUTSIDE-SECRET") {
				t.Fatalf("GetReport read the report the link leads to: %+v, %v", got, err)
			}
			refused("GetReport", err)
			ids, err := db.GetAllTestRunIDs(ctx)
			refused(fmt.Sprintf("GetAllTestRunIDs (listed %v)", ids), err)
		})
	}
}

// A reports folder that is not there yet lists no test runs, and no error:
// first with no recording at all, then in a recording with no reports yet.
func TestAMissingReportsFolderListsNoTestRuns(t *testing.T) {
	keploy := filepath.Join(t.TempDir(), "keploy")
	db := New(zap.NewNop(), filepath.Join(keploy, "reports"))
	for _, recorded := range []bool{false, true} {
		if recorded {
			if err := os.MkdirAll(keploy, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		ids, err := db.GetAllTestRunIDs(context.Background())
		if err != nil || len(ids) != 0 {
			t.Fatalf("GetAllTestRunIDs (keploy folder there: %v) = %v, %v; want none, nil", recorded, ids, err)
		}
	}
}
