package mock

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// chargedSessionCallMiss is a miss of a lane's msc-prefix or msc-none section
// as keploy mock replay reads it at the run's end under the proven wire: a
// call a test made that a recording made outside every test answered earlier,
// which the agent charged to the test (models.UnmatchedCall.Charged), its
// next step ending with the repeat note's hint, which quotes the sessionCalls
// entry that would serve it. whole is that miss as the parser reported it.
func chargedSessionCallMiss(hint string) (whole, charged models.UnmatchedCall) {
	whole = models.UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "GET /echo/rates", Destination: "127.0.0.1:18094", PathTag: "#1f2e3d4c5b6a",
		ClosestMock: "mock-0", MatchPhase: "no_mocks", Kind: models.MissInTest,
		ReceivedReq: "GET /echo/rates HTTP/1.1\r\nHost: 127.0.0.1:18094\r\n\r\n",
		NextSteps:   "This request is identical to mock-0, a recording made outside every test (in TestMain or at start-up), which already answered a call at this replay. " + hint,
		Repeat:      &models.RepeatNote{Mock: "mock-0", OutsideTests: true, ReplayGapSeconds: 0.14, Hint: hint},
	}
	charged = whole.Charged()
	charged.ChargedTo = "rates::TestReload"
	return whole, charged
}

const (
	mscPrefixHint = `A keploy.yaml sessionCalls entry covers this call only by a path prefix or with no method. Under the proven switch a recording made outside every test (in TestMain or at start-up) is served to every test only for a call an entry names exactly, by its method and whole path, so that no recording is served to a call no one declared: to serve this one so, declare sessionCalls: [{host: "127.0.0.1:18094", method: "GET", pathPrefix: "/echo/rates"}]. A recording another test made of it is served to that test only, declared or not. Or record again with 'keploy mock record' so this test records it.`
	mscNoneHint   = `Each test is served the HTTP calls it recorded. If the application made this call outside every test (in TestMain or at start-up) and every test may make it (a token, a shared configuration read), declare it in keploy.yaml to serve that recording to every test (sessionCalls: [{host: "127.0.0.1:18094", method: "GET", pathPrefix: "/echo/rates"}]); a recording another test made of it is served to that test only, declared or not. Or record again with 'keploy mock record' so this test records it.`
)

// A miss the agent charged to a test is named whole where keploy names a
// call whole, the console's Hint and the misses file, whose next step tells
// the user which path to declare; its log line names the path by its tag, and
// the receipts keep it in its shape, as written.
func TestAChargedMissIsWholeOnTheConsoleAndShapedInTheLogAndReceipts(t *testing.T) {
	const wholePath = `pathPrefix: "/echo/rates"`
	for _, tc := range []struct{ name, hint string }{
		{"msc-prefix", mscPrefixHint},
		{"msc-none", mscNoneHint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole, charged := chargedSessionCallMiss(tc.hint)
			misses := []models.UnmatchedCall{charged}
			keployDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(keployDir, "go-msc"), 0o755); err != nil {
				t.Fatal(err)
			}

			// The console and the misses file: whole, beside the tag.
			file := writeMissesFile(zap.NewNop(), keployDir, "go-msc", misses)
			if file == "" {
				t.Fatal("no misses file was written")
			}
			var console bytes.Buffer
			sayMisses(&console, misses, file)
			want := "  Mock mismatch: [HTTP] GET /echo/rates [path tag #1f2e3d4c5b6a]  →  127.0.0.1:18094\n  Hint: " + whole.NextSteps + "\n"
			if console.String() != want {
				t.Errorf("the console said\n%q, want\n%q", console.String(), want)
			}
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var got missesReport
			if err := yaml.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Misses) != 1 || got.Misses[0].Call != "GET /echo/rates" || got.Misses[0].PathTag != "#1f2e3d4c5b6a" ||
				got.Misses[0].NextStep != whole.NextSteps || !strings.Contains(got.Misses[0].NextStep, wholePath) {
				t.Errorf("the misses file holds %+v, want the call and its next step whole, beside its tag", got.Misses)
			}

			// The log: the call's shape, its tag, and the path by the tag.
			core, logs := observer.New(zapcore.DebugLevel)
			logMiss(zap.New(core), charged)
			lines := logs.All()
			if len(lines) != 1 {
				t.Fatalf("logged %d lines, want 1", len(lines))
			}
			f := lines[0].ContextMap()
			next, _ := f["next_step"].(string)
			if f["call"] != "GET /{}/{}" || f["path_tag"] != "#1f2e3d4c5b6a" ||
				!strings.Contains(next, `pathPrefix: <the path keploy's console Hint and report name for [path tag #1f2e3d4c5b6a]>`) {
				t.Errorf("the line is %v, want the call's shape, its tag and the path by the tag", f)
			}
			for k, v := range f {
				if s, ok := v.(string); ok && strings.Contains(s, "echo") {
					t.Errorf("the line's %s names the call's path: %q", k, s)
				}
			}

			// The receipts, as written: the misses no test was charged with,
			// and a test's evidence (its window's misses, as evidence keeps
			// them).
			uncharged := charged
			uncharged.ChargedTo = ""
			r, err := yaml.Marshal(unchargedMisses([]models.UnmatchedCall{uncharged}))
			if err != nil {
				t.Fatal(err)
			}
			writeTestsReceipt(zap.NewNop(), keployDir, TestsReceipt{Set: "go-msc", Tests: []TestReceipt{{
				Name: "rates::TestReload", Outcome: models.ScopeOutcomeFailed,
				Evidence: &models.ScopeEvidence{Misses: []models.UnmatchedCall{whole.Sanitized()}},
			}}})
			tests, err := os.ReadFile(filepath.Join(keployDir, "go-msc", TestsReceiptFile))
			if err != nil {
				t.Fatal(err)
			}
			for name, written := range map[string]string{"the run's receipt": string(r), "the tests' receipt": string(tests)} {
				if strings.Contains(written, wholePath) || strings.Contains(written, "1f2e3d4c5b6a") || !strings.Contains(written, `pathPrefix: "/{}/{}"`) {
					t.Errorf("%s holds the path whole or a tag, or not in its shape:\n%s", name, written)
				}
			}

			// A test's misses, which the receipt's lines and the replay's
			// hooks are handed, keep no whole call.
			at := charged
			at.At = replayWindows[0].Start.Add(10 * time.Millisecond)
			flows := attributeMocks(replayWindows, nil, nil, []models.UnmatchedCall{at})
			if len(flows) != 1 || len(flows[0].Missed) != 1 || flows[0].Missed[0].Whole != nil {
				t.Errorf("a test's misses are %+v, want the charged miss with no whole call", flows)
			}
		})
	}
}
