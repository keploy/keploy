package replay

import (
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
// proven switch off each call made before the set's first test, which fails
// nothing; with it on those that count against no test, since failing takes
// the others, and the set's end read's that count against none, last. An
// answered call is never one.
func TestTheSetsOutsideTestMissesFailNothingInEitherMode(t *testing.T) {
	dns := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A db.example.", BeforeSet: true}
	call := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /config", BeforeSet: true}
	answered := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /ok", BeforeSet: true, Answer: "by_kind"}
	lateDNS := models.UnmatchedCall{Protocol: "DNS", ActualSummary: "A late.example."}
	lateCall := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /late"}
	lateBackground := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /tick", Background: true}

	var s setCalls
	if rest := s.take([]models.UnmatchedCall{dns, call, answered}); len(rest) != 0 {
		t.Fatalf("the first test's read kept %+v as its own", rest)
	}
	s.keepAfter([]models.UnmatchedCall{lateDNS, lateCall, lateBackground})

	names := func(calls []models.UnmatchedCall) string {
		out := ""
		for _, c := range calls {
			out += c.ActualSummary + ";"
		}
		return out
	}
	if got, want := names(s.outside(false)), "A db.example.;GET /config;A late.example.;GET /tick;"; got != want {
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

// Each outside-test miss is said at WARN, by its shape and tags, with its
// next step; past outsideLogMax, one line says how many more the report
// names; and a last line points to the set's misses file, by path, when one
// was written.
func TestEachOutsideTestMissIsSaidWithItsNextStep(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := &Replayer{logger: zap.New(core)}
	misses := models.OutsideTestMissesOf([]models.UnmatchedCall{{Protocol: "DNS", ActualSummary: "A db.example.", HostTag: "#ab12", BeforeSet: true}})
	r.logOutsideTestMisses("test-set-0", misses, "")
	e := logs.FilterMessageSnippet("outside every test").All()
	if len(e) != 1 || logs.Len() != 1 {
		t.Fatalf("got %d lines, want 1 and no misses file's: %+v", len(e), logs.All())
	}
	f := e[0].ContextMap()
	if f["call"] != "A {}.{}." || f["destination"] != "{}.{}. #ab12" || f["when"] != models.OutsideBeforeFirstTest || f["next_step"] != misses[0].NextStep || f["test-set"] != "test-set-0" {
		t.Errorf("the line reads %+v", f)
	}

	logs.TakeAll()
	var many []models.UnmatchedCall
	for i := 0; i < outsideLogMax+3; i++ {
		many = append(many, models.UnmatchedCall{Protocol: "HTTP", ActualSummary: fmt.Sprintf("GET /c%d", i), Destination: fmt.Sprintf("10.0.0.%d:80", i), BeforeSet: true})
	}
	const file = "keploy/test-set-0/last-replay-misses.yaml"
	r.logOutsideTestMisses("test-set-0", models.OutsideTestMissesOf(many), file)
	if n := logs.Len(); n != outsideLogMax+2 {
		t.Fatalf("got %d lines for %d misses, want %d", n, len(many), outsideLogMax+2)
	}
	if more := logs.FilterMessageSnippet("more calls").All(); len(more) != 1 || more[0].ContextMap()["more"] != int64(3) {
		t.Fatalf("no line says 3 more: %+v", more)
	}
	if last := logs.All()[outsideLogMax+1]; last.ContextMap()["misses_file"] != file {
		t.Fatalf("the last line does not point to the misses file: %+v", last)
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

			file := filepath.Join(keployDir, "test-set-0", localfile.MissesFile)
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
			for _, name := range []string{localfile.MissesFile, localfile.TempPrefixOf(localfile.MissesFile) + "0.tmp"} {
				out, err := exec.Command(git, "-C", filepath.Join(keployDir, "test-set-0"), "check-ignore", "-q", name).CombinedOutput()
				if err != nil {
					t.Errorf("git does not ignore %s: %v: %s", name, err, out)
				}
			}
			said := logs.FilterMessageSnippet("misses file").All()
			if len(said) != 1 || said[0].ContextMap()["misses_file"] != file {
				t.Errorf("the log points to %+v, want the misses file by path", said)
			}

			r.setCalls.reset()
			if r.outsideTestMisses("test-set-0") != nil {
				t.Fatal("a run with no outside-test miss names one")
			}
			if _, err := os.Lstat(file); !os.IsNotExist(err) {
				t.Errorf("the last run's misses file stands after a run that missed no call outside every test: %v", err)
			}
		})
	}
}
