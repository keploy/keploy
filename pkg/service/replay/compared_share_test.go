package replay

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A run that set tests aside says how many it compared: the set's report
// carries its share, and the run's summary ends with its line, with and
// without ANSI, as record's quarantine left a PASSED set with 1 of its 3
// tests compared.
func TestARunThatSetTestsAsideEndsWithItsComparedShare(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, noANSI := range []bool{false, true} {
		h := newPartialRunHarness(t, 3, 0)
		h.replayer.config.DisableANSI = noANSI
		h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-2", "test-3"}}
		h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{
			"test-2": models.SkipReasonText("keploy", "unmocked_call", "Pulsar SEND had no recorded answer"),
			"test-3": models.SkipReasonText("keploy", "unmocked_call", "Redis GET had no recorded answer"),
		})
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %s, want PASSED", status)
		}
		want := &models.ComparedShare{Compared: 1, Total: 3, SetAside: 2, Reasons: []models.SetAsideReason{{Reason: "keploy/unmocked_call", Tests: 2}}}
		if got := h.report.report.SetAside; !reflect.DeepEqual(got, want) {
			t.Fatalf("the set's report carries share %+v, want %+v", got, want)
		}
		out := captureStdout(t, func() { h.replayer.printSummary(t.Context(), true) })
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if last := lines[len(lines)-1]; last != "1 of 3 compared; 2 set aside: 2 keploy/unmocked_call" {
			t.Fatalf("DisableANSI=%v: the summary's last line is %q, want the compared share:\n%s", noANSI, last, out)
		}
	}
}

// A run that set nothing aside says nothing more, and its report carries no
// share.
func TestARunThatSetNothingAsideHasNoShareLine(t *testing.T) {
	for _, noANSI := range []bool{false, true} {
		h := newPartialRunHarness(t, 2, 0)
		h.replayer.config.DisableANSI = noANSI
		if status := h.run(t); status != models.TestSetStatusPassed {
			t.Fatalf("status = %s, want PASSED", status)
		}
		if h.report.report.SetAside != nil {
			t.Fatalf("a set that compared all its tests carries share %+v", h.report.report.SetAside)
		}
		if out := captureStdout(t, func() { h.replayer.printSummary(t.Context(), true) }); strings.Contains(out, "compared;") {
			t.Fatalf("DisableANSI=%v: a run that set nothing aside printed a share:\n%s", noANSI, out)
		}
	}
}

// The run's share counts each set once, as its last run (a flaky check runs
// a set again), and a new run starts with none.
func TestRunComparedShareCountsASetAsItsLastRun(t *testing.T) {
	r := &Replayer{}
	r.noteComparedShare("test-set-0", models.NewComparedShare(0, 2, 2, map[string]int{"manual": 2}))
	r.noteComparedShare("test-set-1", models.NewComparedShare(0, 1, 1, map[string]int{"keploy/unmocked_call": 1}))
	got := r.runComparedShare(0, 3, 3)
	want := &models.ComparedShare{Compared: 0, Total: 3, SetAside: 3, Reasons: []models.SetAsideReason{{Reason: "manual", Tests: 2}, {Reason: "keploy/unmocked_call", Tests: 1}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("two sets: %+v, want %+v", got, want)
	}
	// test-set-0 ran again and set nothing aside: only test-set-1's reason is left.
	r.noteComparedShare("test-set-0", nil)
	if got := r.runComparedShare(2, 3, 1).Line(); got != "2 of 3 compared; 1 set aside: 1 keploy/unmocked_call" {
		t.Fatalf("after test-set-0 ran again with nothing set aside: %q", got)
	}
	r.forgetQuarantined()
	if got := r.runComparedShare(0, 1, 1).Line(); got != "0 of 1 compared; 1 set aside: 1 "+models.NoSkipReason {
		t.Fatalf("a new run kept the last run's reasons: %q", got)
	}
	if r.runComparedShare(3, 3, 0) != nil {
		t.Fatal("a run that set none aside has a share")
	}
	if got := comparedShareLine(nil); got != "" {
		t.Fatalf("a run with no share ends with %q, want nothing more", got)
	}
}

// The report a set's end writes carries its share capped: its first
// MaxShareReasons reasons, the rest counted as other reasons, so the report's
// head stays bounded however many reasons set its tests aside. The run's
// share, which its last line reads, keeps every reason.
func TestTheReportASetEndWritesCarriesItsShareCapped(t *testing.T) {
	r := &Replayer{}
	rep := &models.TestReport{TestSet: "test-set-0", Status: string(models.TestSetStatusPassed), Ignored: 7, Total: 7}
	for i := 1; i <= 7; i++ {
		rep.Tests = append(rep.Tests, models.TestResult{TestCaseID: fmt.Sprintf("test-%d", i), Status: models.TestStatusIgnored, SkipReason: fmt.Sprintf("reason-%d", i)})
	}
	r.noteSetEnd(rep)
	if got := rep.SetAside; got == nil || len(got.Reasons) != models.MaxShareReasons || got.OtherReasons != 2 || got.OtherTests != 2 || got.SetAside != 7 {
		t.Fatalf("the report's share: %+v, want %d reasons and 2 other reasons of 2 tests, of 7 set aside", got, models.MaxShareReasons)
	}
	if got := r.runComparedShare(0, 7, 7); len(got.Reasons) != 7 || got.OtherReasons != 0 {
		t.Fatalf("the run's share: %+v, want all 7 reasons", got)
	}
}
