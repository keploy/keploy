package replay

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// twoSetsDB is a test DB of two test sets that hold the same tests.
type twoSetsDB struct{ *bpTestDB }

func (twoSetsDB) GetAllTestSetIDs(context.Context) ([]string, error) {
	return []string{"test-set-0", "test-set-1"}, nil
}

// attempts counts the attempts the run made at its test sets, as it says
// each one ("running", with the attempt).
func (b *bpRun) attempts() int {
	return b.logs.FilterLevelExact(zap.InfoLevel).FilterMessage("running").Len()
}

// A run none of whose tests got a verdict, every test set or every test of
// it ignored, verified nothing: it fails, with the switch on or off, as a run
// with no tests to run does (P249). It exited 0, "replay completed", where
// enterprise's own run fails as "no test ran". A run that skips one set and
// runs another passes as before.
func TestARunNoTestOfWhichGotAVerdictFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		on      bool
		ignored []string
	}{
		{"the set ignored whole, switch off", false, []string{}},
		{"the set ignored whole, switch on", true, []string{}},
		{"every test ignored, switch off", false, []string{"test-1", "test-2"}},
		{"every test ignored, switch on", true, []string{"test-1", "test-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, tc.on)
			app := newBPApp(t)
			run := newBasePathRun(t, app.URL, "test-1", "test-2")
			run.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": tc.ignored}

			code, err := run.start(t)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if code == 0 {
				t.Fatal("a run none of whose tests got a verdict exited 0")
			}
			if said := run.said(zap.ErrorLevel, "no test ran"); len(said) != 1 {
				t.Fatalf("the run said %d times that no test ran, want once", len(said))
			}
			if got := app.asked(); len(got) != 0 {
				t.Fatalf("an ignored set's tests were sent: %v", got)
			}
		})
	}

	// A run that failed already (no tests to run) says so, not that no test
	// ran: the rule is for a run that would otherwise pass.
	t.Run("a failed run", func(t *testing.T) {
		setSwitch(t, false)
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL)
		code, err := run.start(t)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if said := run.said(zap.ErrorLevel, "no test ran"); code == 0 || len(said) != 0 {
			t.Fatalf("exited %d, said no test ran %d times; want non-zero, and never", code, len(said))
		}
	})

	t.Run("one set of two ignored", func(t *testing.T) {
		setSwitch(t, false)
		app := newBPApp(t)
		run := newBasePathRun(t, app.URL, "test-1", "test-2")
		run.replayer.testDB = twoSetsDB{run.tests}
		run.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": {}}

		code, err := run.start(t)
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		if code != 0 || len(run.said(zap.ErrorLevel, "no test ran")) != 0 {
			t.Fatalf("a run that ran test-set-1 and passed exited %d, or said no test ran", code)
		}
		if got := app.asked(); len(got) != 2 {
			t.Fatalf("test-set-1's tests were sent %d times, want 2", len(got))
		}
	})
}

// A set IGNORED under the switch, every test of it skipped one by one, ran
// to find that none had a verdict: a flaky check's further attempts only run
// it again, app and all, to find the same (P006). It is run once. Without the
// switch an IGNORED set is one the run skips whole, which starts nothing,
// and its attempts are as they were.
func TestAnIgnoredSetIsNotRunAgainForAFlakyCheck(t *testing.T) {
	for _, tc := range []struct {
		name    string
		on      bool
		ignored []string
		want    int
	}{
		{"switch on, every test ignored", true, []string{"test-1", "test-2"}, 1},
		{"switch off, the set ignored whole", false, []string{}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, tc.on)
			app := newBPApp(t)
			run := newBasePathRun(t, app.URL, "test-1", "test-2")
			run.replayer.config.Test.MaxFlakyChecks = 3
			run.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": tc.ignored}

			if _, err := run.start(t); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := run.attempts(); got != tc.want {
				t.Fatalf("the set was run %d times, want %d", got, tc.want)
			}
		})
	}
}

// A run that fails only because no test got a verdict (a hook quarantined
// every test, under the switch, after they ran and missed) failed no test on
// a mock difference: it says that no test ran, and does not print the
// mismatch table as a run that failed on mock differences does (P249).
func TestARunThatVerifiedNothingPrintsNoMismatchTable(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.hookImpl = verdictHooks{set: func(rs []models.TestResult) {
		for i := range rs {
			rs[i].Status = models.TestStatusIgnored
		}
	}}
	// Each test missed a call; the app made none after the last test (that
	// fails the set: unclaimed_calls_test.go).
	charges := func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "GET /charges"}}, nil
	}
	h.instr.mockErrorsFor = misses(charges, charges)
	core, logs := observer.New(zap.InfoLevel)
	h.replayer.logger = zap.New(core)
	h.replayer.config.Command = "./app"
	h.replayer.config.CommandType = string(utils.Native)
	h.replayer.config.Test.MaxFlakyChecks = 1
	saved := utils.ErrCode
	t.Cleanup(func() { utils.ErrCode = saved })
	utils.ErrCode = 0
	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()

	if err := h.replayer.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if n := logs.FilterMessageSnippet("no test ran").Len(); utils.ErrCode != 1 || n != 1 {
		t.Fatalf("exit code %d, said no test ran %d times; want 1, once", utils.ErrCode, n)
	}
	if n := logs.FilterMessage("Some testsets failed due to mock differences.").Len(); n != 0 {
		t.Fatalf("a run that verified nothing said %d times that mock differences failed it", n)
	}
}
