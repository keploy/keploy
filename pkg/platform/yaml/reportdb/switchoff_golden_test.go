package reportdb

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Without the proven switch, a test report, and the calls no mock answered as
// the agent hands them to keploy test, are written byte for byte as the
// goldens hold them (S12 v16 §4.5): a report with no answered calls writes
// what keploy wrote before reports could hold them.
//
// To regenerate: KEPLOY_GOLDEN_OUT=<dir> go test -run
// TestReportsAreUnchangedWithoutTheSwitch ./pkg/platform/yaml/reportdb/, then
// copy <dir> to testdata/switchoff_golden. To check a change that must leave
// these files alone, generate them on a checkout from before the change, with
// this file added to it, and run this test after the change.
const goldenSwitchOffDir = "testdata/switchoff_golden"

func checkSwitchOffGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if out := os.Getenv("KEPLOY_GOLDEN_OUT"); out != "" {
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(filepath.Join(goldenSwitchOffDir, name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if bytes.Equal(got, want) {
		return
	}
	gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("%s differs from the golden without the proven switch, first at line %d:\n got: %q\nwant: %q", name, i+1, g, w)
		}
	}
}

// goldenMisses are calls no mock answered, as the agent reports them.
func goldenMisses() []models.UnmatchedCall {
	at := time.Date(2026, 10, 22, 9, 0, 2, 0, time.UTC)
	return []models.UnmatchedCall{
		{Protocol: "MongoDB", ActualSummary: "insert keploy.events", Destination: "mongo:27017", ClosestMock: "mock-6",
			MatchPhase: "body", CandidateCount: 2, NotOwned: true, RecordedIn: "post-url-1", At: at, Kind: models.MissInTest,
			Differs: []string{"body.documents[0]._id"}, Reason: models.MissRequestChanged},
		{Protocol: "HTTP", ActualSummary: "GET /health", Destination: "localhost:9000", At: at.Add(time.Second),
			Kind: models.MissOutsideTest, BeforeSet: true},
	}
}

// goldenReport is a set's report: a passed test, a failed one with misses,
// and the set's own unclaimed call.
func goldenReport() models.TestReport {
	misses := goldenMisses()
	return models.TestReport{
		Version: models.GetVersion(), Name: "test-set-0-report", Status: string(models.TestSetStatusFailed),
		Success: 1, Failure: 1, Total: 2, TestSet: "test-set-0", CreatedAt: 1792659600, TimeTaken: "1.5s", Proven: true,
		Tests: []models.TestResult{
			{Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusPassed, Started: 1792659601, Completed: 1792659602,
				TestCaseID: "post-url-0", TimeTaken: "20ms"},
			{Kind: models.HTTP, Name: "test-set-0", Status: models.TestStatusFailed, Started: 1792659602, Completed: 1792659603,
				TestCaseID: "post-url-1", TimeTaken: "21ms",
				FailureInfo: models.FailureInfo{Risk: models.High, UnmatchedCalls: misses[:1]}},
		},
		UnclaimedCalls: misses[1:],
	}
}

func TestReportsAreUnchangedWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		ext := format.FileExtension()
		t.Run(ext, func(t *testing.T) {
			report := sanitizeReportForYAML(goldenReport())
			got, err := yaml.MarshalGeneric(format, &report)
			if err != nil {
				t.Fatal(err)
			}
			checkSwitchOffGolden(t, "report."+ext, got)
		})
	}
	// What the agent's /mockerrors answers, and keploy test reads: the calls
	// as JSON.
	got, err := json.Marshal(goldenMisses())
	if err != nil {
		t.Fatal(err)
	}
	checkSwitchOffGolden(t, "unmatched_calls.json", got)
}
