package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	ossMockdb "go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// --- Unit: the fix's keep-set helper ---
//
// Auto-replay's prune (UpdateMocks) keeps only passing-test consumed mocks
// (+ config/startup). A test that EXECUTED and FAILED had its mapped mocks
// deleted from mocks.json while its mapping was still written
// (UpdateTestMapping=true) — leaving the mapping pointing at deleted mocks. On
// every later replay that test can't find its mocks (missing-mock -> EOF),
// breaking both the legacy and smart-set cloud-replay paths.
//
// The fix keys mock-preservation on "did the test PASS" instead of "was it
// EXECUTED". This shows both behaviours through the same helper.
func TestRetainNonPassingTestMocks_FailedTestMocksSurvivePrune(t *testing.T) {
	expected := map[string][]models.MockEntry{
		"test-failed": {{Name: "mock-104", Kind: "Mongo"}, {Name: "mock-105", Kind: "Mongo"}},
		"test-passed": {{Name: "mock-200", Kind: "Mongo"}},
		"test-notrun": {{Name: "mock-300", Kind: "Http"}},
	}

	// OLD behaviour (the bug): preserve only NOT-EXECUTED tests, i.e. skip every
	// executed test — passed AND failed. The failed test's mocks are not kept.
	oldSkip := map[string]bool{"test-passed": true, "test-failed": true}
	keepOld := map[string]models.MockState{"mock-200": {Name: "mock-200"}}
	retainNonPassingTestMocks(expected, oldSkip, keepOld)
	if _, ok := keepOld["mock-104"]; ok {
		t.Fatal("sanity: under the OLD executed-based behaviour, a failed test's mock should NOT be preserved")
	}

	// NEW behaviour (the fix): preserve every NON-PASSING test, i.e. skip only
	// PASSED tests. The failed and not-run tests' mapped mocks survive.
	newSkip := map[string]bool{"test-passed": true}
	keepNew := map[string]models.MockState{"mock-200": {Name: "mock-200"}}
	added := retainNonPassingTestMocks(expected, newSkip, keepNew)
	for _, n := range []string{"mock-104", "mock-105", "mock-300"} {
		if _, ok := keepNew[n]; !ok {
			t.Errorf("FIX: non-passing test's mapped mock %q must be preserved from pruning, but it was dropped", n)
		}
	}
	if _, ok := keepNew["mock-200"]; !ok {
		t.Error("a passed test's already-kept mock must remain in the keep-set")
	}
	if added != 3 {
		t.Errorf("expected 3 newly-preserved mocks, got %d", added)
	}
}

// pruneFixtureMock builds a minimal, validly-encodable HTTP mock with a given
// request timestamp. The prune is protocol-agnostic (it keeps/deletes by name,
// metadata.type, and timestamp), so HTTP stands in for the Mongo mocks the real
// bug dropped. metadata.type is deliberately NOT "config" so the mock is
// prune-eligible.
func pruneFixtureMock(reqTS time.Time) *models.Mock {
	return &models.Mock{
		Version: "api.keploy.io/v1beta1",
		Kind:    models.HTTP,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"operation": "GET"},
			HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://svc/x", ProtoMajor: 1, ProtoMinor: 1, Header: map[string]string{}},
			HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: "ok"},
			ReqTimestampMock: reqTS,
			ResTimestampMock: reqTS.Add(time.Millisecond),
		},
	}
}

func mockNamesOnDisk(t *testing.T, dir, testSetID string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, testSetID, "mocks.yaml"))
	if err != nil {
		t.Fatalf("read mocks.yaml: %v", err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name: ") {
			names[strings.TrimSpace(strings.TrimPrefix(line, "name:"))] = true
		}
	}
	return names
}

// Integration: drive the REAL prune (MockYaml.UpdateMocks) and show that a
// mapped mock which was NOT re-consumed during auto-replay (failed/diverged
// test) is deleted (the bug), and that augmenting the keep-set via the fix
// helper preserves it (the fix). This replicates the on-disk dangling-mapping
// state observed on the real staging recording (vhncm: mapped mongo mocks
// absent from mocks.json).
func TestAutoReplayPrune_MappedMockOfNonPassingTestSurvives_Integration(t *testing.T) {
	ctx := context.Background()
	const set = "test-set-0"

	base := time.Now().Add(-time.Hour)
	pruneBefore := time.Now()                // mocks recorded before now -> prune-eligible
	startupCutoff := base.Add(-time.Minute)  // before all mocks -> none are startup-exempt

	insertThree := func(dir string) (string, string, string) {
		ys := ossMockdb.New(zap.NewNop(), dir, "mocks")
		m1 := pruneFixtureMock(base)
		m2 := pruneFixtureMock(base.Add(time.Second))
		m3 := pruneFixtureMock(base.Add(2 * time.Second))
		for _, m := range []*models.Mock{m1, m2, m3} {
			if err := ys.InsertMock(ctx, m, set); err != nil {
				t.Fatalf("InsertMock: %v", err)
			}
		}
		ys.Close()
		return m1.Name, m2.Name, m3.Name // InsertMock renames -> mock-1, mock-2, mock-3
	}

	// ---- Phase A: BUG ----
	// test-1 (owns mockA) FAILED/diverged in auto-replay, so mockA is NOT in the
	// passing-consumed keep-set. mockB/mockC were consumed by passing tests.
	dirBug := t.TempDir()
	mockA, mockB, mockC := insertThree(dirBug)
	passing := map[string]models.MockState{mockB: {Name: mockB}, mockC: {Name: mockC}}
	ysBug := ossMockdb.New(zap.NewNop(), dirBug, "mocks")
	if err := ysBug.UpdateMocks(ctx, set, passing, pruneBefore, startupCutoff); err != nil {
		t.Fatalf("UpdateMocks (bug phase): %v", err)
	}
	got := mockNamesOnDisk(t, dirBug, set)
	if got[mockA] {
		t.Fatalf("sanity: expected the prune to DELETE the non-re-consumed mapped mock %q (the bug), but it survived", mockA)
	}
	if !got[mockB] || !got[mockC] {
		t.Fatalf("passing-consumed mocks %q/%q must survive the prune", mockB, mockC)
	}
	t.Logf("BUG reproduced: prune deleted mapped mock %q; mapping referencing it now dangles", mockA)

	// ---- Phase B: FIX ----
	// Same setup, but the fix augments the keep-set with the EXPECTED mocks of
	// the non-passing test (test-1 -> mockA) before pruning.
	dirFix := t.TempDir()
	mockA2, mockB2, mockC2 := insertThree(dirFix)
	passingFix := map[string]models.MockState{mockB2: {Name: mockB2}, mockC2: {Name: mockC2}}
	expectedMappings := map[string][]models.MockEntry{
		"test-1": {{Name: mockA2, Kind: "Http"}}, // test-1 did not pass; its mapping references mockA2
	}
	retainNonPassingTestMocks(expectedMappings, map[string]bool{ /* test-1 not passed */ }, passingFix)
	ysFix := ossMockdb.New(zap.NewNop(), dirFix, "mocks")
	if err := ysFix.UpdateMocks(ctx, set, passingFix, pruneBefore, startupCutoff); err != nil {
		t.Fatalf("UpdateMocks (fix phase): %v", err)
	}
	gotFix := mockNamesOnDisk(t, dirFix, set)
	if !gotFix[mockA2] {
		t.Errorf("FIX: a non-passing test's mapped mock %q must survive the prune so its mapping doesn't dangle, but it was deleted", mockA2)
	}
	if !gotFix[mockB2] || !gotFix[mockC2] {
		t.Errorf("passing-consumed mocks must still survive under the fix")
	}
}
