package report

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const legacyReportPath = "testdata/legacy-report.yaml"

// cleanDepsReportPath is the same report as written by a POST-slice-4 keploy
// in instrument+mapping mode where every test lost nothing. It is checked in
// for one reason: the nothing-missing case is the exact case the backward-
// compatibility constraint singled out, and it is the one case whose bytes DO
// change — by exactly the two scalars `deps_checked` / `deps_consumed`, with
// `dep_result: []` unchanged. Pinning it makes that deliberate rather than
// incidental, and shows exactly what the change is.
const cleanDepsReportPath = "testdata/clean-deps-report.yaml"

// updateGolden regenerates testdata/legacy-report.yaml FROM THE REAL STRUCTS:
//
//	go test ./pkg/service/report/... -run TestLegacyReport -update-golden
//
// The fixture is checked in and compared byte-for-byte by the tests below, so
// it still pins the on-disk shape; generating it removes the class of bug a
// hand-authored fixture invites (the previous version declared total: 2 while
// carrying three tests, and could not have been produced by any keploy run).
var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/legacy-report.yaml from legacyReportFixture()")

// legacyReportFixture is a report as a PRE-SLICE-4 keploy would have written
// it: three tests (passed, failed, obsolete) and `dep_result: []` on every one,
// because models.Result.DepResult had a declaration and no writer.
//
// Counters are derived from the tests rather than typed in, so the fixture
// cannot be internally inconsistent.
func legacyReportFixture() *models.TestReport {
	req := func(method models.Method, url string, body string, header map[string]string) models.HTTPReq {
		return models.HTTPReq{
			Method: method, ProtoMajor: 1, ProtoMinor: 1, URL: url,
			Header: header, Body: body, Timestamp: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		}
	}
	resp := func(code int, body string, header map[string]string) models.HTTPResp {
		return models.HTTPResp{
			StatusCode: code, Header: header, Body: body,
			Timestamp: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
		}
	}

	tests := []models.TestResult{
		{
			Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusPassed,
			Started: 1755861852, Completed: 1755861853,
			TestCasePath: "/keploy/test-set-0", MockPath: "/keploy/test-set-0/mocks.yaml",
			TestCaseID: "test-1",
			Req:        req("GET", "http://localhost:8080/orders/1", "", map[string]string{"Accept": "application/json"}),
			Res:        resp(200, `{"id":1,"status":"CONFIRMED"}`, map[string]string{"Content-Type": "application/json"}),
			Result: models.Result{
				StatusCode: models.IntResult{Normal: true, Expected: 200, Actual: 200},
				BodyResult: []models.BodyResult{{
					Normal: true, Type: models.JSON,
					Expected: `{"id":1,"status":"CONFIRMED"}`, Actual: `{"id":1,"status":"CONFIRMED"}`,
				}},
			},
			TimeTaken: "12.3ms",
		},
		{
			Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusFailed,
			Started: 1755861854, Completed: 1755861855,
			TestCasePath: "/keploy/test-set-0", MockPath: "/keploy/test-set-0/mocks.yaml",
			TestCaseID: "test-2",
			Req:        req("POST", "http://localhost:8080/orders", `{"sku":"SKU-9","qty":3}`, map[string]string{"Content-Type": "application/json"}),
			Res:        resp(500, `{"error":"boom"}`, map[string]string{"Content-Type": "application/json"}),
			Result: models.Result{
				StatusCode: models.IntResult{Normal: false, Expected: 201, Actual: 500},
				BodyResult: []models.BodyResult{{
					Normal: false, Type: models.JSON,
					Expected: `{"id":2,"status":"CONFIRMED"}`, Actual: `{"error":"boom"}`,
				}},
			},
			TimeTaken: "8.1ms",
		},
		{
			Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusObsolete,
			Started: 1755861856, Completed: 1755861857,
			TestCasePath: "/keploy/test-set-0", MockPath: "/keploy/test-set-0/mocks.yaml",
			TestCaseID: "test-3",
			Req:        req("GET", "http://localhost:8080/orders/9", "", map[string]string{}),
			Res:        resp(404, `{"error":"not found"}`, map[string]string{}),
			Result: models.Result{
				StatusCode: models.IntResult{Normal: false, Expected: 200, Actual: 404},
			},
			TimeTaken: "5.0ms",
		},
	}

	rep := &models.TestReport{
		Version:   "api.keploy.io/v1beta1",
		Name:      "test-set-0-report",
		Status:    string(models.TestSetStatusFailed),
		Total:     len(tests),
		TestSet:   "test-set-0",
		CreatedAt: 1755861860,
		TimeTaken: "25.4ms",
		Tests:     tests,
	}
	for _, t := range tests {
		switch t.Status {
		case models.TestStatusPassed:
			rep.Success++
		case models.TestStatusFailed:
			rep.Failure++
		case models.TestStatusIgnored:
			rep.Ignored++
		case models.TestStatusObsolete:
			rep.Obsolete++
		}
	}
	return rep
}

// cleanDepsReportFixture is legacyReportFixture() as the current replayer
// writes it when the dependency assertion RAN and every test lost nothing: two
// scalars per test, NO rows at all, no categories, no status change.
func cleanDepsReportFixture() *models.TestReport {
	rep := legacyReportFixture()
	for i := range rep.Tests {
		rep.Tests[i].Result.DepsChecked = true
		rep.Tests[i].Result.DepsConsumed = 3 + i
	}
	return rep
}

func loadLegacyReport(t *testing.T) ([]byte, *models.TestReport) {
	t.Helper()
	raw, err := os.ReadFile(legacyReportPath)
	if err != nil {
		t.Fatalf("read %s: %v", legacyReportPath, err)
	}
	var rep models.TestReport
	if err := yaml.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("unmarshal %s: %v", legacyReportPath, err)
	}
	return raw, &rep
}

// A report written before models.Result.DepResult had any writer must
// round-trip byte-for-byte. This is the on-disk backward-compatibility pin for
// keploy-consumer-design-v2.md §7 slice 4: DepResult carries NO omitempty, so
// every existing report already serializes `dep_result: []`, and the schema
// does not change at all. Adding omitempty — or renaming/reordering any field
// on TestReport / TestResult / Result — breaks this test, which is the point.
func TestLegacyReportRoundTripsByteIdentically(t *testing.T) {
	if *updateGolden {
		out, err := yaml.Marshal(legacyReportFixture())
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		if err := os.WriteFile(legacyReportPath, out, 0o644); err != nil {
			t.Fatalf("write %s: %v", legacyReportPath, err)
		}
		t.Logf("regenerated %s (%d bytes)", legacyReportPath, len(out))
	}

	raw, rep := loadLegacyReport(t)

	out, err := yaml.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("legacy report did not round-trip byte-identically.\n--- on disk ---\n%s\n--- re-marshalled ---\n%s", raw, out)
	}
}

// The checked-in bytes must be exactly what the struct fixture produces, so
// the file cannot drift into a state no keploy run could emit (the previous
// hand-authored version declared total: 2 while carrying three tests).
func TestLegacyReportFixtureMatchesTheStructs(t *testing.T) {
	raw, _ := loadLegacyReport(t)
	want, err := yaml.Marshal(legacyReportFixture())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if string(raw) != string(want) {
		t.Fatalf("testdata/legacy-report.yaml is stale. Regenerate it:\n"+
			"  go test ./pkg/service/report/... -run TestLegacyReport -update-golden\n"+
			"--- on disk ---\n%s\n--- from structs ---\n%s", raw, want)
	}
}

// The fixture's own counters must agree with its tests.
func TestLegacyReportFixtureIsInternallyConsistent(t *testing.T) {
	_, rep := loadLegacyReport(t)
	if rep.Total != len(rep.Tests) {
		t.Errorf("total = %d but the report carries %d tests", rep.Total, len(rep.Tests))
	}
	var pass, fail, obsolete int
	for _, test := range rep.Tests {
		switch test.Status {
		case models.TestStatusPassed:
			pass++
		case models.TestStatusFailed:
			fail++
		case models.TestStatusObsolete:
			obsolete++
		}
	}
	if rep.Success != pass || rep.Failure != fail || rep.Obsolete != obsolete {
		t.Errorf("counters (success=%d failure=%d obsolete=%d) disagree with the tests (%d/%d/%d)",
			rep.Success, rep.Failure, rep.Obsolete, pass, fail, obsolete)
	}
}

// dep_result must stay present-and-empty rather than disappearing: k8s-proxy's
// report-download handler reads the key, and an absent key is a schema change
// for every consumer of every report ever written.
func TestLegacyReportKeepsEmptyDepResultKey(t *testing.T) {
	raw, rep := loadLegacyReport(t)

	if !strings.Contains(string(raw), "dep_result: []") {
		t.Fatal("fixture is not a pre-slice-4 report: it has no empty dep_result key")
	}
	for _, test := range rep.Tests {
		if len(test.Result.DepResult) != 0 {
			t.Fatalf("fixture test %q unexpectedly carries dependency rows", test.TestCaseID)
		}
		data, err := json.Marshal(test.Result)
		if err != nil {
			t.Fatalf("marshal result for %q: %v", test.TestCaseID, err)
		}
		if !strings.Contains(string(data), `"dep_result"`) {
			t.Errorf("dep_result key vanished from the JSON projection of %q: %s", test.TestCaseID, data)
		}
	}
}

// Every renderer must produce exactly what it produced before this slice for a
// report with no dependency rows.
func TestLegacyReportRenderersAreUnchanged(t *testing.T) {
	_, rep := loadLegacyReport(t)
	r := New(zap.NewNop(), &config.Config{DisableANSI: true}, nil, nil)

	t.Run("cli renderer emits no dependency block", func(t *testing.T) {
		for _, test := range rep.Tests {
			var sb strings.Builder
			if err := r.renderSingleFailedTest(context.Background(), &sb, test); err != nil {
				t.Fatalf("renderSingleFailedTest(%s): %v", test.TestCaseID, err)
			}
			out := sb.String()
			if strings.Contains(out, "DEPENDENCY ASSERTIONS") || strings.Contains(out, models.DepNoticePrefix) {
				t.Errorf("test %q grew a dependency block:\n%s", test.TestCaseID, out)
			}
			// The historical header is FAILED-only wording; obsolete tests were
			// never rendered before, so only FAILED wording is pinned here.
			if test.Status == models.TestStatusFailed &&
				!strings.Contains(out, "Testrun failed for "+test.Name+"/"+test.TestCaseID) {
				t.Errorf("failed test %q lost its historical header:\n%s", test.TestCaseID, out)
			}
		}
	})

	t.Run("failed-test collector still admits only FAILED", func(t *testing.T) {
		got := r.extractFailedTestsFromResults(rep.Tests)
		if len(got) != 1 || got[0].TestCaseID != "test-2" {
			ids := make([]string, 0, len(got))
			for _, g := range got {
				ids = append(ids, g.TestCaseID)
			}
			t.Fatalf("collector returned %v, want [test-2] (the only FAILED test)", ids)
		}
	})

	t.Run("junit output carries no dependency text and keeps the skip literal", func(t *testing.T) {
		data, err := xml.MarshalIndent(buildJUnitSuites(map[string]*models.TestReport{"test-set-0": rep}), "", "  ")
		if err != nil {
			t.Fatalf("marshal junit: %v", err)
		}
		out := string(data)
		if strings.Contains(out, "dependency ") {
			t.Errorf("junit output grew dependency lines:\n%s", out)
		}
		if strings.Contains(out, "<system-out>") {
			t.Errorf("junit output grew a system-out element for a legacy report:\n%s", out)
		}
		if !strings.Contains(out, `<skipped message="obsolete test case">`) {
			t.Errorf("obsolete skip message changed:\n%s", out)
		}
	})

	t.Run("ndjson projection reports empty effects", func(t *testing.T) {
		verdicts := buildTestVerdicts("test-run-0", map[string]*models.TestReport{"test-set-0": rep})
		if len(verdicts) != len(rep.Tests) {
			t.Fatalf("got %d verdicts, want %d", len(verdicts), len(rep.Tests))
		}
		for _, v := range verdicts {
			if len(v.Effects) != 0 {
				t.Errorf("test %q has effects for a legacy report: %+v", v.TestCaseID, v.Effects)
			}
			if v.FailureCategories != nil {
				t.Errorf("test %q grew failure categories: %v", v.TestCaseID, v.FailureCategories)
			}
		}
	})
}

// A per-test-set report is written to disk, re-read by `keploy report` and
// uploaded to the fleet report store, so the per-dependency cost of the
// default mode is a shipping constraint, not a detail. One dependency row is
// ~190 bytes of YAML; the aggregate is one row no matter how many
// dependencies a test consumed.
func TestDependencyRowsDoNotBloatThePersistedReport(t *testing.T) {
	base := legacyReportFixture()
	baseline, err := yaml.Marshal(base)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	withDeps := legacyReportFixture()
	// 200 consumed dependencies + 1 missing, the shape a Postgres-chatty test
	// produces, in the DEFAULT (knob off) mode.
	withDeps.Tests[0].Result.DepsChecked = true
	withDeps.Tests[0].Result.DepsConsumed = 200
	withDeps.Tests[0].Result.DepResult = []models.DepResult{
		{
			Name: "deps[0] postgres PostgreSQL INSERT (presence)", Type: "postgres",
			Meta: []models.DepMetaResult{{
				Normal: false, Key: models.DepKeyPresence,
				Expected: models.DepPresenceConsumed, Actual: models.DepPresenceMissing,
			}},
		},
	}
	grown, err := yaml.Marshal(withDeps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	const maxGrowth = 320 // one missing row + two scalars, with slack for indentation
	if delta := len(grown) - len(baseline); delta > maxGrowth {
		t.Fatalf("a test consuming 200 dependencies grew the report by %d bytes (max %d). "+
			"Per-dependency matched rows must not be persisted, in any mode — see "+
			"models.Result.DepResult for the 3-11 MB/report measurement.",
			delta, maxGrowth)
	}
}

// The nothing-missing report: the one case where this slice changes the bytes
// of a report that would otherwise be identical.
//
// Regenerate with:
//
//	go test ./pkg/service/report/... -run TestCleanDeps -update-golden
func TestCleanDepsReportRoundTripsByteIdentically(t *testing.T) {
	if *updateGolden {
		out, err := yaml.Marshal(cleanDepsReportFixture())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(cleanDepsReportPath, out, 0o644); err != nil {
			t.Fatalf("write %s: %v", cleanDepsReportPath, err)
		}
		t.Logf("regenerated %s (%d bytes)", cleanDepsReportPath, len(out))
		return
	}

	raw, err := os.ReadFile(cleanDepsReportPath)
	if err != nil {
		t.Fatalf("read %s: %v", cleanDepsReportPath, err)
	}
	var rep models.TestReport
	if err := yaml.Unmarshal(raw, &rep); err != nil {
		t.Fatalf("unmarshal %s: %v", cleanDepsReportPath, err)
	}
	out, err := yaml.Marshal(&rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != string(raw) {
		t.Fatalf("the nothing-missing report no longer round-trips byte-identically.\n"+
			"Regenerate with -update-golden if the change is deliberate.\n--- got ---\n%s\n--- want ---\n%s",
			out, raw)
	}

	want, err := yaml.Marshal(cleanDepsReportFixture())
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if string(want) != string(raw) {
		t.Fatalf("%s has drifted from the structs; regenerate it with -update-golden", cleanDepsReportPath)
	}
}

// What "nothing is missing" must look like, and must keep looking like: ZERO
// dependency rows, the checked bit set, nothing failed, no category, and STATUS
// UNCHANGED from the legacy report.
func TestCleanDepsReportShape(t *testing.T) {
	rep := cleanDepsReportFixture()
	legacy := legacyReportFixture()

	for i, tr := range rep.Tests {
		if got := len(tr.Result.DepResult); got != 0 {
			t.Fatalf("test %q carries %d dependency rows; a nothing-missing test must carry NONE — "+
				"rows are for missing dependencies only:\n%+v",
				tr.TestCaseID, got, tr.Result.DepResult)
		}
		if tr.Result.HasMissingDeps() {
			t.Errorf("test %q reports a missing dependency in the nothing-missing fixture", tr.TestCaseID)
		}
		if !tr.Result.DependenciesChecked() {
			t.Errorf("test %q must read as CHECKED; that is what deps_checked is for", tr.TestCaseID)
		}
		if tr.Result.DepsConsumed == 0 {
			t.Errorf("test %q lost its consumed count", tr.TestCaseID)
		}
		if tr.Status != legacy.Tests[i].Status {
			t.Errorf("test %q status changed from %s to %s — nothing-missing must not touch the verdict",
				tr.TestCaseID, legacy.Tests[i].Status, tr.Status)
		}
		if len(tr.FailureInfo.Category) != len(legacy.Tests[i].FailureInfo.Category) {
			t.Errorf("test %q grew a failure category with nothing missing: %v",
				tr.TestCaseID, tr.FailureInfo.Category)
		}
	}
	if rep.Status != legacy.Status || rep.Success != legacy.Success ||
		rep.Failure != legacy.Failure || rep.Obsolete != legacy.Obsolete {
		t.Errorf("the test-set counters changed: %+v vs %+v", rep, legacy)
	}
}

// The measured price of making "checked and clean" distinguishable from "never
// checked" on disk.
//
// This is the ONE surface the hard backward-compatibility constraint touches:
// statuses, exit codes, stdout, JUnit XML and --json are byte-identical for a
// nothing-missing run, and the report file grows by exactly two short scalar
// keys. An earlier revision of this slice encoded the same one bit as an
// unconditionally-persisted aggregate DepResult row, measured at +224 bytes per
// test on EVERY report, forever, uploaded to the fleet report store. The bound
// below is what keeps it from creeping back.
func TestCleanDepsReportGrowthIsOneAggregateRowPerTest(t *testing.T) {
	baseline, err := yaml.Marshal(legacyReportFixture())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	grown, err := yaml.Marshal(cleanDepsReportFixture())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// `dep_result: []` must survive verbatim: k8s-proxy's report-download
	// handler reads the key, and a nothing-missing test must not start
	// carrying rows again.
	if strings.Contains(string(grown), "deps[") {
		t.Fatalf("a nothing-missing report persisted a dependency row:\n%s", grown)
	}
	if !strings.Contains(string(grown), "dep_result: []") {
		t.Fatalf("a nothing-missing report lost the pre-slice-4 `dep_result: []`:\n%s", grown)
	}

	n := len(legacyReportFixture().Tests)
	delta := len(grown) - len(baseline)
	perTest := delta / n
	// Two scalar keys at report indentation. The deleted aggregate row cost
	// 224; anything approaching that is the row coming back in disguise.
	const maxPerTest = 64
	if perTest > maxPerTest {
		t.Fatalf("a nothing-missing report grew by %d bytes per test (max %d): %d bytes over %d tests. "+
			"Encoding 'checked, N consumed' costs one bit and one small int; a nested row is 224.",
			perTest, maxPerTest, delta, n)
	}
	t.Logf("nothing-missing growth: %d bytes total, %d bytes/test over %d tests", delta, perTest, n)
}

// The renderers must stay silent for a nothing-missing report: no dependency
// block, no notice, no summary counter. Everything a user sees is unchanged.
func TestCleanDepsReportRendersNothingNew(t *testing.T) {
	rep := cleanDepsReportFixture()

	for _, tr := range rep.Tests {
		var sb strings.Builder
		renderDepResults(&sb, tr)
		if sb.Len() != 0 {
			t.Errorf("test %q rendered a dependency block with nothing missing:\n%s", tr.TestCaseID, sb.String())
		}
		if got := models.FormatDepNotice(tr.Result.DepResult); got != "" {
			t.Errorf("test %q produced a notice with nothing missing: %q", tr.TestCaseID, got)
		}
		if tr.Status != models.TestStatusFailed && shouldRenderDiff(tr) {
			t.Errorf("test %q was admitted to the per-test renderer with nothing missing", tr.TestCaseID)
		}
	}

	total, passed := depNoticeCounts(map[string]*models.TestReport{"test-set-0": rep})
	if total != 0 || passed != 0 {
		t.Errorf("the summary counted %d/%d dependency notices with nothing missing", passed, total)
	}

	// JUnit: no <system-out>, no dependency lines in the failure body.
	suites := buildJUnitSuites(map[string]*models.TestReport{"test-set-0": rep})
	out, err := xml.MarshalIndent(suites, "", "  ")
	if err != nil {
		t.Fatalf("marshal junit: %v", err)
	}
	if strings.Contains(string(out), "system-out") || strings.Contains(string(out), "dependency ") {
		t.Errorf("JUnit grew dependency output for a nothing-missing report:\n%s", out)
	}

	// NDJSON: checked, and no failed effect.
	for _, v := range buildTestVerdicts("run-1", map[string]*models.TestReport{"test-set-0": rep}) {
		if !v.DependenciesChecked {
			t.Errorf("%s: dependencies_checked must be true", v.TestCaseID)
		}
		for _, e := range v.Effects {
			if !e.Matched {
				t.Errorf("%s: unexpected failed effect %+v", v.TestCaseID, e)
			}
		}
	}
}
