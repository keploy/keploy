package replay

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// switchModes are the two ways a run reaches a skip with a reason: the proven
// switch on (its gate, or keploy test --judge's rounds), or off (a caller that
// skips for a reason, as a cloud replay does).
var switchModes = []struct {
	name string
	on   bool
}{{"switch on", true}, {"switch off", false}}

func setProvenSwitch(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv(provenswitch.Env, "1")
	} else {
		t.Setenv(provenswitch.Env, "")
	}
}

// quarantineHeading is the heading of the summary's quarantined block, as it
// was before skips had kinds.
func quarantineHeading(on bool, n string) string {
	if on {
		return "\n\n\tQuarantined tests, skipped by keploy (KEPLOY_PROVEN=1): " + n + "\n"
	}
	return "\n\n\tTests skipped by keploy for a reason: " + n + "\n"
}

// A run whose skips are all quarantines (no caller named a kind) prints the
// summary it printed before skips had kinds, byte for byte: its heading, then
// each test by set and test, its reason on one line.
func TestQuarantinedSummary_AQuarantineOnlyRunPrintsWhatItDid(t *testing.T) {
	for _, m := range switchModes {
		t.Run(m.name, func(t *testing.T) {
			setProvenSwitch(t, m.on)
			h := newPartialRunHarness(t, 1, 0)
			h.replayer.config.Test.SkipReasons = map[string]map[string]string{
				"set-b": {"t-1": "manual: waits\non PAY-12"},
				"set-a": {"t-2": "flaky/flipped: order", "t-1": "flaky/replay_only: why"},
			}
			h.replayer.noteQuarantined("set-b", "t-1", "manual: waits\non PAY-12")
			h.replayer.noteQuarantined("set-a", "t-2", "flaky/flipped: order")
			h.replayer.noteQuarantined("set-a", "t-1", "flaky/replay_only: why")
			want := quarantineHeading(m.on, "3") +
				"\t\tset-a/t-1: flaky/replay_only: why\n" +
				"\t\tset-a/t-2: flaky/flipped: order\n" +
				"\t\tset-b/t-1: manual: waits on PAY-12\n"
			if got := h.replayer.quarantinedSummary(); got != want {
				t.Fatalf("summary\n%q\nwant what it printed before kinds\n%q", got, want)
			}
		})
	}
}

// The headings of the summary's blocks of skips that are no quarantine.
const (
	thisRunOnlyHeading     = "\n\tTests skipped by keploy for this run only, not quarantined: "
	untilRerecordedHeading = "\n\tTests skipped by keploy until they are re-recorded, not quarantined: "
)

// A run given a quarantine, a skip for this run only (keploy test --judge's
// rerun skipping a test it suggested quarantining) and a skip until the test
// is re-recorded (a cloud replay skipping a test with no mapping entry)
// lists each under its own heading, the quarantine first: neither of the
// others is called quarantined. Each IGNORED row keeps its reason and carries
// its kind, and each skip is said, at Debug, as what it is.
func TestRunTestSet_EachSkipIsSaidAsItsKind(t *testing.T) {
	const quarantine, rerun, rerecord = "manual: waits on PAY-12", "flaky/flipped: keploy\ntest --judge suggested it", "no mapping entry: re-record it"
	for _, m := range switchModes {
		t.Run(m.name, func(t *testing.T) {
			setProvenSwitch(t, m.on)
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {"test-2", "test-3", "test-4"}}
			h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-2": quarantine, "test-3": rerun, "test-4": rerecord})
			h.replayer.config.Test.SkipKinds = map[string]map[string]models.SkipKind{"test-set-0": {"test-3": models.SkipThisRunOnly, "test-4": models.SkipUntilRerecorded}}
			core, logs := observer.New(zap.DebugLevel)
			h.replayer.logger = zap.New(core)

			h.run(t)
			want := map[string]models.TestResult{
				"test-2": {SkipReason: quarantine, SkipKind: models.SkipQuarantined},
				"test-3": {SkipReason: rerun, SkipKind: models.SkipThisRunOnly},
				"test-4": {SkipReason: rerecord, SkipKind: models.SkipUntilRerecorded},
			}
			for _, row := range h.report.report.Tests {
				w, skipped := want[row.TestCaseID]
				if !skipped {
					if row.SkipReason != "" || row.SkipKind != "" {
						t.Fatalf("a test that ran carries a skip: %+v", row)
					}
					continue
				}
				if row.Status != models.TestStatusIgnored || row.SkipReason != w.SkipReason || row.SkipKind != w.SkipKind {
					t.Fatalf("row %+v; want IGNORED with reason %q and kind %q", row, w.SkipReason, w.SkipKind)
				}
				delete(want, row.TestCaseID)
			}
			if len(want) != 0 {
				t.Fatalf("no row for %v", want)
			}

			wantSummary := quarantineHeading(m.on, "1") +
				"\t\ttest-set-0/test-2: manual: waits on PAY-12\n" +
				thisRunOnlyHeading + "1\n" +
				"\t\ttest-set-0/test-3: flaky/flipped: keploy test --judge suggested it\n" +
				untilRerecordedHeading + "1\n" +
				"\t\ttest-set-0/test-4: no mapping entry: re-record it\n"
			if got := h.replayer.quarantinedSummary(); got != wantSummary {
				t.Fatalf("summary\n%q\nwant\n%q", got, wantSummary)
			}

			quarantineLine := "skipping a test for a reason"
			if m.on {
				quarantineLine = "skipping a quarantined test (KEPLOY_PROVEN=1)"
			}
			wantSaid := map[string]string{"test-2": quarantineLine, "test-3": "skipping a test for this run only", "test-4": "skipping a test until it is re-recorded"}
			said := logs.FilterMessageSnippet("skipping a ").All()
			if len(said) != len(wantSaid) {
				t.Fatalf("said %d lines, want %d: %+v", len(said), len(wantSaid), said)
			}
			for _, e := range said {
				tc, _ := e.ContextMap()["testcase"].(string)
				if e.Message != wantSaid[tc] {
					t.Fatalf("%s said %q, want %q", tc, e.Message, wantSaid[tc])
				}
			}
		})
	}
}

// A run whose only skips are of one kind that is no quarantine prints that
// kind's block alone: no quarantine heading, nor another kind's.
func TestQuarantinedSummary_SkipsOfOneKindHaveNoOtherHeading(t *testing.T) {
	for _, k := range []struct {
		kind    models.SkipKind
		heading string
	}{{models.SkipThisRunOnly, thisRunOnlyHeading}, {models.SkipUntilRerecorded, untilRerecordedHeading}} {
		for _, m := range switchModes {
			t.Run(string(k.kind)+"/"+m.name, func(t *testing.T) {
				setProvenSwitch(t, m.on)
				h := newPartialRunHarness(t, 1, 0)
				h.replayer.config.Test.SkipReasons = skipReasonsFor("set-a", map[string]string{"t-2": "why two", "t-1": "why one"})
				h.replayer.config.Test.SkipKinds = map[string]map[string]models.SkipKind{"set-a": {"t-1": k.kind, "t-2": k.kind}}
				h.replayer.noteQuarantined("set-a", "t-2", "why two")
				h.replayer.noteQuarantined("set-a", "t-1", "why one")
				want := "\n" + k.heading + "2\n" +
					"\t\tset-a/t-1: why one\n" +
					"\t\tset-a/t-2: why two\n"
				if got := h.replayer.quarantinedSummary(); got != want {
					t.Fatalf("summary\n%q\nwant\n%q", got, want)
				}
			})
		}
	}
}

// A run lists none of the last run's skips: each Start begins with no skips
// noted. Else a replayer whose Start runs again would list the last run's
// skips in the next run's summary and, as a test's first note is the one
// kept, say a later run's quarantine of a test with the kind an earlier run
// gave its skip.
func TestStart_ARunListsNoneOfTheLastRunsSkips(t *testing.T) {
	for _, m := range switchModes {
		t.Run(m.name, func(t *testing.T) {
			setProvenSwitch(t, m.on)
			app := newBPApp(t)
			run := newBasePathRun(t, app.URL, "test-1", "test-2")
			cfg := run.replayer.config
			cfg.Test.IgnoredTests = map[string][]string{"test-set-0": {}}

			// The first run skips test-1 for this run only and quarantines test-2.
			cfg.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-1": "why one", "test-2": "why two"})
			cfg.Test.SkipKinds = map[string]map[string]models.SkipKind{"test-set-0": {"test-1": models.SkipThisRunOnly}}
			var err error
			out := captureStdout(t, func() { _, err = run.start(t) })
			if err != nil {
				t.Fatalf("the first Start: %v", err)
			}
			first := quarantineHeading(m.on, "1") +
				"\t\ttest-set-0/test-2: why two\n" +
				thisRunOnlyHeading + "1\n" +
				"\t\ttest-set-0/test-1: why one\n"
			if got := run.replayer.quarantinedSummary(); got != first || !strings.Contains(out, first) {
				t.Fatalf("the first run's summary\n%q\nwant\n%q\nprinted:\n%s", got, first, out)
			}

			// The next run quarantines test-1, its caller naming no kind, and
			// gives test-2 no reason.
			cfg.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-1": "manual: why one"})
			cfg.Test.SkipKinds = nil
			out = captureStdout(t, func() { _, err = run.start(t) })
			if err != nil {
				t.Fatalf("the next Start: %v", err)
			}
			next := quarantineHeading(m.on, "1") + "\t\ttest-set-0/test-1: manual: why one\n"
			if got := run.replayer.quarantinedSummary(); got != next || !strings.Contains(out, next) || strings.Contains(out, "why two") || strings.Contains(out, strings.TrimPrefix(thisRunOnlyHeading, "\n")) {
				t.Fatalf("the next run's summary\n%q\nwant only its own skip, a quarantine\n%q\nprinted:\n%s", got, next, out)
			}
		})
	}
}

// A kind names what a skip with a reason is: a test the run skips with no
// reason carries no kind, whatever config.Test.SkipKinds holds for it, and is
// neither said nor listed. That holds for a test of a set the run runs in
// part and of a set it ignores whole, whose rows ignoredSetResults builds on a
// path of its own. With the switch off, such a set has rows only when its
// caller gave a reason for one of its tests, so test-1 has one there.
func TestRunTestSet_ASkipKindWithNoReasonSaysNothing(t *testing.T) {
	for _, c := range []struct {
		name    string
		ignored []string
		reasons map[string]string
		summary func(on bool) string
	}{
		{"a set run in part", []string{"test-2"}, nil, func(bool) string { return "" }},
		{"a set ignored whole", []string{}, map[string]string{"test-1": "why one"}, func(on bool) string {
			return quarantineHeading(on, "1") + "\t\ttest-set-0/test-1: why one\n"
		}},
	} {
		for _, m := range switchModes {
			t.Run(c.name+"/"+m.name, func(t *testing.T) {
				setProvenSwitch(t, m.on)
				h := newPartialRunHarness(t, 2, 0)
				h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": c.ignored}
				h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", c.reasons)
				h.replayer.config.Test.SkipKinds = map[string]map[string]models.SkipKind{"test-set-0": {"test-2": models.SkipThisRunOnly}}
				core, logs := observer.New(zap.DebugLevel)
				h.replayer.logger = zap.New(core)

				h.run(t)
				for _, e := range logs.FilterMessageSnippet("skipping a ").All() {
					if tc, _ := e.ContextMap()["testcase"].(string); tc == "test-2" {
						t.Fatalf("the test skipped with no reason was said: %q", e.Message)
					}
				}
				found := false
				for _, row := range h.report.report.Tests {
					if row.TestCaseID == "test-2" {
						found = true
						if row.Status != models.TestStatusIgnored || row.SkipReason != "" || row.SkipKind != "" {
							t.Fatalf("row %+v; want IGNORED with no reason and no kind", row)
						}
					}
				}
				if !found {
					t.Fatal("no row for the ignored test")
				}
				if got, want := h.replayer.quarantinedSummary(), c.summary(m.on); got != want {
					t.Fatalf("summary %q; want %q", got, want)
				}
			})
		}
	}
}

// A set the run ignores whole has one IGNORED row a test, each with its
// reason and its kind.
func TestRunTestSet_FullyIgnoredSetRowsCarryTheirKind(t *testing.T) {
	for _, m := range switchModes {
		t.Run(m.name, func(t *testing.T) {
			setProvenSwitch(t, m.on)
			h := newPartialRunHarness(t, 4, 0)
			h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {}}
			h.replayer.config.Test.SkipReasons = skipReasonsFor("test-set-0", map[string]string{"test-1": "manual: why one", "test-3": "why three", "test-4": "why four"})
			h.replayer.config.Test.SkipKinds = map[string]map[string]models.SkipKind{"test-set-0": {"test-3": models.SkipThisRunOnly, "test-4": models.SkipUntilRerecorded}}

			if status := h.run(t); status != models.TestSetStatusIgnored {
				t.Fatalf("status = %s, want IGNORED", status)
			}
			want := map[string]models.TestResult{
				"test-1": {SkipReason: "manual: why one"},
				"test-2": {},
				"test-3": {SkipReason: "why three", SkipKind: models.SkipThisRunOnly},
				"test-4": {SkipReason: "why four", SkipKind: models.SkipUntilRerecorded},
			}
			rows := h.report.report.Tests
			if len(rows) != len(want) {
				t.Fatalf("rows %+v; want one a test", rows)
			}
			for _, row := range rows {
				w := want[row.TestCaseID]
				if row.Status != models.TestStatusIgnored || row.SkipReason != w.SkipReason || row.SkipKind != w.SkipKind {
					t.Fatalf("row %+v; want IGNORED with reason %q and kind %q", row, w.SkipReason, w.SkipKind)
				}
			}
			wantSummary := quarantineHeading(m.on, "1") +
				"\t\ttest-set-0/test-1: manual: why one\n" +
				thisRunOnlyHeading + "1\n" +
				"\t\ttest-set-0/test-3: why three\n" +
				untilRerecordedHeading + "1\n" +
				"\t\ttest-set-0/test-4: why four\n"
			if got := h.replayer.quarantinedSummary(); got != wantSummary {
				t.Fatalf("summary\n%q\nwant\n%q", got, wantSummary)
			}
		})
	}
}

// A kind keploy does not know is listed with the quarantined, as a skip of no
// kind is: no skip with a reason drops out of the summary.
func TestQuarantinedSummary_AnUnknownKindIsListedAsBefore(t *testing.T) {
	for _, m := range switchModes {
		t.Run(m.name, func(t *testing.T) {
			setProvenSwitch(t, m.on)
			h := newPartialRunHarness(t, 1, 0)
			h.replayer.config.Test.SkipReasons = skipReasonsFor("set-a", map[string]string{"t-1": "why one"})
			h.replayer.config.Test.SkipKinds = map[string]map[string]models.SkipKind{"set-a": {"t-1": models.SkipKind("a_later_kind")}}
			h.replayer.noteQuarantined("set-a", "t-1", "why one")
			if got, want := h.replayer.quarantinedSummary(), quarantineHeading(m.on, "1")+"\t\tset-a/t-1: why one\n"; got != want {
				t.Fatalf("summary\n%q\nwant\n%q", got, want)
			}
		})
	}
}
