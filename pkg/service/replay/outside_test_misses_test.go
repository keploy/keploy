package replay

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/localfile"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// The set's outside-test misses (TestReport.OutsideTestMisses): with the
// proven switch off each call made before the set's first test, and each its
// end read returned, last: none fails anything; with it on, of both, those
// that count against no test, since failing and the end read take the
// others. An answered call is never one.
func TestTheSetsOutsideTestMissesFailNothingInEitherMode(t *testing.T) {
	dns := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A db.example.", BeforeSet: true}
	call := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /config", BeforeSet: true}
	answered := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /ok", BeforeSet: true, Answer: "by_kind"}
	lateDNS := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A late.example."}
	lateCall := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /late"}
	lateBackground := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /tick", Background: true}
	lateAnswered := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /late-ok", Answer: "by_kind"}

	var s setCalls
	if rest := s.take([]models.UnmatchedCall{dns, call, answered}); len(rest) != 0 {
		t.Fatalf("the first test's read kept %+v as its own", rest)
	}
	s.keepAfter([]models.UnmatchedCall{lateDNS, lateCall, lateBackground, lateAnswered})

	names := func(calls []models.UnmatchedCall) string {
		out := ""
		for _, c := range calls {
			out += c.ActualSummary + ";"
		}
		return out
	}
	if got, want := names(s.outside(false)), "A db.example.;GET /config;A late.example.;GET /late;GET /tick;"; got != want {
		t.Errorf("switch off: outside = %s, want %s", got, want)
	}
	if got, want := names(s.outside(true)), "A db.example.;A late.example.;GET /tick;"; got != want {
		t.Errorf("switch on: outside = %s, want %s", got, want)
	}
	if got, want := names(s.failing()), "GET /config;"; got != want {
		t.Errorf("failing = %s, want %s", got, want)
	}
	s.reset()
	if len(s.outside(false)) != 0 || len(s.outside(true)) != 0 {
		t.Error("a reset set keeps outside-test misses")
	}
}

// Outside-test misses emit ONE Info summary line naming the call count, the
// misses file (when written) and each distinct next step the misses carry
// (next_steps), while per-call detail is logged at Debug.
func TestEachOutsideTestMissIsSaidWithItsNextStep(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	r := &Replayer{logger: zap.New(core)}
	misses := models.OutsideTestMissesOf([]models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A db.example.", HostTag: "#ab12", BeforeSet: true}})
	r.logOutsideTestMisses("test-set-0", misses, "")
	infos := logs.FilterLevelExact(zap.InfoLevel).All()
	if len(infos) != 1 {
		t.Fatalf("got %d Info lines, want 1: %+v", len(infos), logs.All())
	}
	if warns := logs.FilterLevelExact(zap.WarnLevel).All(); len(warns) != 0 {
		t.Fatalf("got %d Warn lines, want 0: %+v", len(warns), warns)
	}
	dbgs := logs.FilterLevelExact(zap.DebugLevel).All()
	if len(dbgs) != 1 {
		t.Fatalf("got %d Debug lines, want 1: %+v", len(dbgs), dbgs)
	}
	f := dbgs[0].ContextMap()
	if f["call"] != "A {}.{}." || f["destination"] != "{}.{}. #ab12" || f["when"] != models.OutsideBeforeFirstTest || f["next_step"] != misses[0].NextStep || f["test-set"] != "test-set-0" {
		t.Errorf("the debug line reads %+v", f)
	}
	if got := fmt.Sprint(infos[0].ContextMap()["next_steps"]); got != fmt.Sprint([]any{misses[0].NextStep}) {
		t.Errorf("the summary's next_steps = %s, want the lookup's next step %q", got, misses[0].NextStep)
	}
	if !strings.Contains(misses[0].NextStep, "record the test set again") {
		t.Errorf("a DNS lookup's next step does not say to record the test set again: %q", misses[0].NextStep)
	}

	logs.TakeAll()
	var many []models.UnmatchedCall
	for i := 0; i < 23; i++ {
		many = append(many, models.UnmatchedCall{Protocol: "HTTP", ActualSummary: fmt.Sprintf("GET /c%d", i), Destination: fmt.Sprintf("10.0.0.%d:80", i), BeforeSet: true})
	}
	const file = "keploy/test-set-0/last-test-misses.yaml"
	r.logOutsideTestMisses("test-set-0", models.OutsideTestMissesOf(many), file)
	infos = logs.FilterLevelExact(zap.InfoLevel).All()
	if len(infos) != 1 {
		t.Fatalf("got %d Info lines for %d misses, want 1 summary line: %+v", len(infos), len(many), infos)
	}
	if infos[0].ContextMap()["misses_file"] != file || infos[0].ContextMap()["calls"] != int64(23) {
		t.Fatalf("summary line missing misses_file or calls count: %+v", infos[0].ContextMap())
	}
	if dbgs := logs.FilterLevelExact(zap.DebugLevel).All(); len(dbgs) != 23 {
		t.Fatalf("got %d Debug detail lines, want 23", len(dbgs))
	}
	one := models.OutsideTestMissesOf(many[:1])[0].NextStep
	if got := fmt.Sprint(infos[0].ContextMap()["next_steps"]); got != fmt.Sprint([]any{one}) {
		t.Errorf("23 calls of one kind: next_steps = %s, want their one next step %q", got, one)
	}

	// A lookup and another call: each kind's next step, once, in the order
	// first carried.
	logs.TakeAll()
	mixed := models.OutsideTestMissesOf([]models.UnmatchedCall{
		{Protocol: "HTTP", ActualSummary: "GET /a", Destination: "10.0.0.1:80", BeforeSet: true},
		{Protocol: "DNS", ActualSummary: "A db.example.", HostTag: "#ab12", BeforeSet: true},
		{Protocol: "HTTP", ActualSummary: "GET /b", Destination: "10.0.0.2:80", BeforeSet: true},
	})
	r.logOutsideTestMisses("test-set-0", mixed, file)
	infos = logs.FilterLevelExact(zap.InfoLevel).All()
	if len(infos) != 1 {
		t.Fatalf("got %d Info lines for a mixed set, want 1: %+v", len(infos), infos)
	}
	if got, want := fmt.Sprint(infos[0].ContextMap()["next_steps"]), fmt.Sprint([]any{mixed[0].NextStep, mixed[1].NextStep}); got != want || mixed[0].NextStep == mixed[1].NextStep {
		t.Errorf("mixed: next_steps = %s, want %s", got, want)
	}
}

// An application that resolves a name keploy test's recording holds no answer
// for, at start-up only in the replay (its configuration names another host
// than the recording's), and calls an IP no mock answers: the set's report
// names both by their shape (it may be uploaded), the set's misses file names
// both whole, local and git-ignored, with the next step the report gives
// each, and says keploy test wrote it, and the log points to that file by
// path. In both switch modes: neither call counts against a test. A later run
// of the set with no such call removes the file.
func TestAStartupLookupOnlyInTheReplayIsWholeInTheMissesFileAndShapedInTheReport(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			provenOn(t, proven)
			repo := t.TempDir()
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatalf("this test needs git, to read what git ignores: %v", err)
			}
			if out, err := exec.Command(git, "init", "-q", repo).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, out)
			}
			keployDir := filepath.Join(repo, "keploy")
			if err := os.MkdirAll(filepath.Join(keployDir, "test-set-0"), 0o755); err != nil {
				t.Fatal(err)
			}
			// keploy mock replay's misses file of the set, as its last run
			// left it: a keploy test run never writes or removes it.
			mockFile := filepath.Join(keployDir, "test-set-0", localfile.MissesFile)
			mockWas := []byte("# Written by `keploy mock replay`: each call no recorded mock answered in the last replay of this set\nset: test-set-0\nby: keploy mock replay\n")
			if err := os.WriteFile(mockFile, mockWas, 0o600); err != nil {
				t.Fatal(err)
			}
			mockStays := func(when string) {
				t.Helper()
				if b, err := os.ReadFile(mockFile); err != nil || !bytes.Equal(b, mockWas) {
					t.Errorf("%s, keploy mock replay's misses file holds %q (%v), want it byte for byte", when, b, err)
				}
			}
			core, logs := observer.New(zap.InfoLevel)
			r := &Replayer{logger: zap.New(core), config: &config.Config{Path: keployDir}}
			lookup := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A orders-db.staging.internal.", HostTag: "#ab12", BeforeSet: true}
			// A call the agent kept as evidence keeps one: shaped, whole in
			// its Whole.
			conn := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/tenants/acme/config", Destination: "10.20.30.40:8080",
				PathTag: "#1f2e", Background: true, NextSteps: "the matcher's own next step"}.Charged()
			conn.BeforeSet = true
			if rest := r.setCalls.take([]models.UnmatchedCall{lookup, conn}); len(rest) != 0 {
				t.Fatalf("the first test's read kept %+v as its own", rest)
			}

			report := r.outsideTestMisses("test-set-0")
			if len(report) != 2 {
				t.Fatalf("the report names %+v, want the lookup and the connection", report)
			}
			for _, m := range report {
				if strings.Contains(m.Call+m.Destination, "orders-db") || strings.Contains(m.Call, "acme") {
					t.Errorf("the report names %+v whole, want its shape", m)
				}
			}
			if report[1].Destination != "10.20.30.40:8080" {
				t.Errorf("the report names the connection's IP as %q, want it whole", report[1].Destination)
			}

			file := filepath.Join(keployDir, "test-set-0", localfile.TestMissesFile)
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("the set's misses file was not written: %v", err)
			}
			if !strings.HasPrefix(string(raw), "# Written by `keploy test`: ") {
				t.Errorf("the misses file's header is\n%s", strings.SplitN(string(raw), "\n", 2)[0])
			}
			var got localfile.MissesReport
			if err := yaml.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.By != "keploy test" || got.Set != "test-set-0" || len(got.Misses) != 2 {
				t.Fatalf("the misses file holds %+v", got)
			}
			if c := got.Misses[0]; c.Call != "A orders-db.staging.internal." || c.NextStep != models.OutsideTestNextStep(lookup) || c.HostTag != "#ab12" {
				t.Errorf("the misses file names the lookup as %+v, want it whole with its report's next step", c)
			}
			if c := got.Misses[1]; c.Call != "GET /v1/tenants/acme/config" || c.Destination != "10.20.30.40:8080" || c.PathTag != "#1f2e" || c.NextStep != models.OutsideTestNextStep(conn) {
				t.Errorf("the misses file names the connection as %+v, want it whole with its report's next step", c)
			}
			for _, name := range []string{localfile.TestMissesFile, localfile.TempPrefixOf(localfile.TestMissesFile) + "0.tmp"} {
				out, err := exec.Command(git, "-C", filepath.Join(keployDir, "test-set-0"), "check-ignore", "-q", name).CombinedOutput()
				if err != nil {
					t.Errorf("git does not ignore %s: %v: %s", name, err, out)
				}
			}
			said := logs.FilterMessageSnippet("misses file").All()
			if len(said) != 1 || said[0].ContextMap()["misses_file"] != file {
				t.Errorf("the log points to %+v, want the misses file by path", said)
			}
			mockStays("after a keploy test run wrote its misses file")

			r.setCalls.reset()
			if r.outsideTestMisses("test-set-0") != nil {
				t.Fatal("a run with no outside-test miss names one")
			}
			if _, err := os.Lstat(file); !os.IsNotExist(err) {
				t.Errorf("the last run's misses file stands after a run that missed no call outside every test: %v", err)
			}
			mockStays("after a keploy test run with no outside-test miss")
		})
	}
}

// A keploy test run of a set whose application looked a name up as it
// started, which the recording holds no answer for, passes in either switch
// mode (a call outside every test is served, not required), names the
// lookup by its shape in the set's report, and writes it whole in keploy
// test's own misses file, in place of its last run's. keploy mock replay's
// misses file of the set is left byte for byte, by the removal at the set's
// start and by the write at its end.
func TestRunTestSet_AStartupLookupLeavesMockReplaysMissesFileAsItWas(t *testing.T) {
	for _, proven := range []bool{false, true} {
		t.Run(fmt.Sprintf("proven=%v", proven), func(t *testing.T) {
			provenOn(t, proven)
			h := newPartialRunHarness(t, 2, 0)
			dir := filepath.Join(h.replayer.config.Path, "test-set-0")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			mockFile := filepath.Join(dir, localfile.MissesFile)
			mockWas := []byte("set: test-set-0\nby: keploy mock replay\nmisses:\n    - protocol: HTTP\n      call: GET /v1/reset\n      count: 1\n")
			if err := os.WriteFile(mockFile, mockWas, 0o600); err != nil {
				t.Fatal(err)
			}
			testFile := filepath.Join(dir, localfile.TestMissesFile)
			if err := os.WriteFile(testFile, []byte("set: test-set-0\nby: keploy test\nmisses:\n    - protocol: DNS\n      call: A an-earlier-run.example.\n      count: 1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			h.instr.mockErrorsFor = misses(func() ([]models.UnmatchedCall, error) {
				return []models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A orders-db.staging.internal.", HostTag: "#ab12",
					BeforeSet: true, Kind: models.MissOutsideTest}}, nil
			})

			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %s; a start-up lookup outside every test fails nothing", status)
			}
			if got := h.report.report.OutsideTestMisses; len(got) != 1 || got[0].Call != "A {}.{}.{}." || got[0].When != models.OutsideBeforeFirstTest {
				t.Fatalf("the report names %+v, want the lookup by its shape, made before the first test", got)
			}
			if b, err := os.ReadFile(mockFile); err != nil || !bytes.Equal(b, mockWas) {
				t.Errorf("keploy mock replay's misses file holds %q (%v) after a keploy test run, want it byte for byte", b, err)
			}
			raw, err := os.ReadFile(testFile)
			if err != nil {
				t.Fatalf("keploy test's misses file was not written: %v", err)
			}
			var got localfile.MissesReport
			if err := yaml.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.By != "keploy test" || len(got.Misses) != 1 || got.Misses[0].Call != "A orders-db.staging.internal." {
				t.Errorf("keploy test's misses file holds %+v, want this run's lookup whole in place of the last run's", got)
			}
		})
	}
}
