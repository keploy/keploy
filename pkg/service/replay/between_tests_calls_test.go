package replay

import (
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A call the application makes between two tests, while no test's window is
// open and no test's view serves, that no mock answers is outside every test.
// Under the proven switch the agent hands it to the next test's read marked
// Between; it fails the set (the run is red), named with the tests it was
// made between, and charges no test: the test framework opens each test, and
// the next test did not make it. It failed the next test.

// betweenWrite is the second test's read: the write the application made
// after the first test ended, which no recording answered.
func betweenWrite() ([]models.UnmatchedCall, error) {
	return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "delete on sessions", Destination: "mongo:27017",
		Kind: models.MissOutsideTest, Between: true}}, nil
}

// observeSetLogs has h's replayer log to an observer, at ERROR, and gives it
// the application's command and a keploy.yml folder that is not the cwd.
func observeSetLogs(h *prRun) *observer.ObservedLogs {
	core, logs := observer.New(zapcore.ErrorLevel)
	h.replayer.logger = zap.New(core)
	h.replayer.config.Command = "go run ./cmd/api"
	h.replayer.config.ConfigPath = "/w/conf"
	return logs
}

// recordAgainOf is the command h's lines name to record set again in its
// place: remove its folder in h's keploy folder, then keploy record under
// its name with the app's command (single-quoted), the keploy folder's
// parent (the tests' keploy folder is a temporary one, not under the cwd)
// and the keploy.yml folder observeSetLogs gave.
func recordAgainOf(h *prRun, set string) string {
	return "rm -rf -- " + utils.ShellWord(filepath.Join(h.replayer.config.Path, set)) +
		" && keploy record -c 'go run ./cmd/api' --path " + utils.ShellWord(filepath.Dir(h.replayer.config.Path)) +
		" --config-path /w/conf --metadata name=" + set
}

// outsideLines are the ERROR lines of logs said in the documented form of a
// call outside every test.
func outsideLines(logs *observer.ObservedLogs) []observer.LoggedEntry {
	var out []observer.LoggedEntry
	for _, e := range logs.All() {
		if e.ContextMap()["kind"] == models.KindOutsideEveryTest {
			out = append(out, e)
		}
	}
	return out
}

func TestRunTestSet_ACallBetweenTwoTestsFailsTheSetAndNoTest(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 3, 0)
	logs := observeSetLogs(h)
	h.instr.mockErrorsFor = misses(nil, betweenWrite)

	status := h.run(t)

	rep := h.report.report
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed {
			t.Fatalf("test %s is %s; the call between the tests is no test's", r.TestCaseID, r.Status)
		}
		if len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("the call between the tests was charged to %s: %+v", r.TestCaseID, r.FailureInfo.UnmatchedCalls)
		}
	}
	if status != models.TestSetStatusFailed || rep.Status != string(models.TestSetStatusFailed) {
		t.Fatalf("status %s (report %s); a call between two tests that no mock answered must fail the set", status, rep.Status)
	}
	if len(rep.UnclaimedCalls) != 1 {
		t.Fatalf("unclaimed calls %+v; want the delete", rep.UnclaimedCalls)
	}
	if c := rep.UnclaimedCalls[0]; !c.Between || c.MadeAfter != "test-1" || c.MadeBefore != "test-2" {
		t.Fatalf("the delete reads %+v; want it Between test-1 and test-2", c)
	}
	for _, want := range []string{"between two tests", models.KindOutsideEveryTest, "Mongo delete on sessions to mongo:27017", "made after test test-1 ended and before test test-2 began"} {
		if !strings.Contains(rep.FailureReason, want) {
			t.Fatalf("failure reason %q does not say %q", rep.FailureReason, want)
		}
	}
	if strings.Contains(rep.FailureReason, "decided only after") {
		t.Fatalf("failure reason %q says a late in-test call; there is none", rep.FailureReason)
	}
	lines := outsideLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d outside_every_test lines; want one: %+v", len(lines), logs.All())
	}
	f := lines[0].ContextMap()
	for k, want := range map[string]string{
		"test-set": "test-set-0", "protocol": "Mongo", "call": "delete on sessions", "destination": "mongo:27017",
		"after": "test-1", "before": "test-2", "closest": "", "made_in": "",
		"facts":     "no recording of this call in the set",
		"next_step": "record the test set again in its place: " + recordAgainOf(h, "test-set-0"),
	} {
		if got := f[k]; got != want {
			t.Errorf("line field %s = %v; want %q", k, got, want)
		}
	}
	if h.replayer.notJudged("test-set-0") {
		t.Fatal("the set is marked not judged; it was judged, FAILED")
	}
}

// A call between tests that came close to a recording names the recording
// and the fields it differs in (their shapes, never a value), and says to
// record again only if the application changed it on purpose.
func TestRunTestSet_ACallBetweenTestsNamesItsClosestRecordingsFields(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	logs := observeSetLogs(h)
	h.instr.mockErrorsFor = misses(nil, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "PUT /carts/9f1c2d3e-0a1b-4c5d-8e9f-a0b1c2d3e4f5", Destination: "carts:8080",
			Kind: models.MissOutsideTest, Between: true, ClosestMock: "mock-7", Differs: []string{"body.items[0].qty"}}}, nil
	})

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	lines := outsideLines(logs)
	if len(lines) != 1 {
		t.Fatalf("%d outside_every_test lines; want one", len(lines))
	}
	f := lines[0].ContextMap()
	if f["after"] != "test-1" || f["before"] != "test-2" || f["closest"] != "mock-7" {
		t.Fatalf("line %+v; want it between test-1 and test-2, closest mock-7", f)
	}
	if got := f["call"]; got != "PUT /carts/{}" {
		t.Errorf("call %v; want its shape, the id left out", got)
	}
	if got := f["facts"]; got != "the closest recording is mock-7, which it differs from in body.items[0].qty" {
		t.Errorf("facts %v", got)
	}
	if got := f["next_step"]; got != "if the application changed this call on purpose, record the test set again in its place: "+recordAgainOf(h, "test-set-0") {
		t.Errorf("next_step %v", got)
	}
}

// Several calls between tests fail the set once, each named.
func TestRunTestSet_SeveralCallsBetweenTestsFailTheSetOnceEachNamed(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 3, 0)
	logs := observeSetLogs(h)
	h.instr.mockErrorsFor = misses(nil, betweenWrite, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "audit:80", Kind: models.MissOutsideTest, Between: true}}, nil
	})

	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 2 {
		t.Fatalf("unclaimed calls %+v; want the delete and the audit post", rep.UnclaimedCalls)
	}
	if c := rep.UnclaimedCalls[1]; c.MadeAfter != "test-2" || c.MadeBefore != "test-3" {
		t.Fatalf("the audit post reads %+v; want it between test-2 and test-3", c)
	}
	if !strings.Contains(rep.FailureReason, "made 2 call(s) between two tests") || strings.Count(rep.FailureReason, "between two tests") != 1 {
		t.Fatalf("failure reason %q; want one clause naming both calls", rep.FailureReason)
	}
	if n := len(outsideLines(logs)); n != 2 {
		t.Fatalf("%d outside_every_test lines; want one per call", n)
	}
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed {
			t.Fatalf("test %s is %s; want every test passed", r.TestCaseID, r.Status)
		}
	}
}

// A call between tests that is the same request as one of the set's
// background recordings fails nothing (unchanged): the application's
// background work made it.
func TestRunTestSet_ABackgroundCallBetweenTestsLeavesTheSetGreen(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	logs := observeSetLogs(h)
	h.instr.mockErrorsFor = misses(nil, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "find on locks", Kind: models.MissOutsideTest, Between: true, Background: true}}, nil
	})
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s; a background call between tests fails no set", status)
	}
	if calls := h.report.report.UnclaimedCalls; len(calls) != 0 {
		t.Fatalf("unclaimed calls %+v; want none", calls)
	}
	if n := len(outsideLines(logs)); n != 0 {
		t.Fatalf("%d outside_every_test lines for a background call", n)
	}
}

// A miss the next test's read carries that is not marked Between (made in a
// test, or with no mark) is that test's, as before: it fails the test.
func TestRunTestSet_AnUnmarkedMissInTheNextTestsReadIsThatTests(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(nil, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "delete on sessions", Kind: models.MissInTest}}, nil
	})
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s, want FAILED", status)
	}
	rep := h.report.report
	if len(rep.UnclaimedCalls) != 0 {
		t.Fatalf("unclaimed calls %+v; the test's own miss is no set's", rep.UnclaimedCalls)
	}
	for _, r := range rep.Tests {
		want := models.TestStatusPassed
		if r.TestCaseID == "test-2" {
			want = models.TestStatusFailed
		}
		if r.Status != want {
			t.Fatalf("test %s is %s, want %s", r.TestCaseID, r.Status, want)
		}
	}
}

// A call between tests is named with the test read before it and the test
// whose read carried it, and the next set starts with no test read: a set
// stopped before its end read keeps none of its tests for the next.
func TestSetCallsPlaceACallBetweenTestsByTheReads(t *testing.T) {
	var s setCalls
	s.takeOf(nil, "test-1")
	gap, _ := betweenWrite()
	if rest := s.takeOf(gap, "test-2"); len(rest) != 0 {
		t.Fatalf("the test keeps %+v; want nothing", rest)
	}
	if got := s.failing(); len(got) != 1 || got[0].MadeAfter != "test-1" || got[0].MadeBefore != "test-2" {
		t.Fatalf("the set holds %+v; want the delete between test-1 and test-2", got)
	}
	s.reset()
	s.takeOf(gap, "test-1")
	if got := s.failing(); len(got) != 1 || got[0].MadeAfter != "" || got[0].MadeBefore != "test-1" {
		t.Fatalf("after the reset the set holds %+v; want it named before test-1 alone", got)
	}
	if w := betweenWhere(s.failing()[0]); w != "made before test test-1 began" {
		t.Fatalf("where %q", w)
	}
	if got := (&Replayer{}).recordAgainCommand("test-set-0"); got != "" {
		t.Fatalf("record command with no config %q; want none", got)
	}
	if got := betweenNextStep(models.UnmatchedCall{ClosestMock: "mock-1"}, "", ""); got != "record the earlier test set that made this call again, with the command that recorded it; keploy cannot name that set, and recording this set again does not hold the call" {
		t.Fatalf("next step with no command %q; want the earlier set's", got)
	}
	// A set keploy names, whose name keploy record would not record under
	// (it reads 123 as a number), is recorded again under its own name: the
	// step names that set, never an earlier one, and prints no command that
	// would record another set.
	cannot := "record the test set 123 again under its own name, with the command that recorded it; keploy cannot print a command that records it in its place"
	for _, c := range []struct {
		call models.UnmatchedCall
		want string
	}{
		{models.UnmatchedCall{ClosestMock: "mock-1"}, "if the application changed this call on purpose, " + cannot},
		{models.UnmatchedCall{}, cannot},
	} {
		if got := betweenNextStep(c.call, "123", ""); got != c.want {
			t.Fatalf("next step for set 123 %q; want %q", got, c.want)
		}
	}
}

// What keploy knows of a call between tests is said from its closest
// recording alone: a recording with no differing field named does not match,
// and fields named with no recording are no closest recording.
func TestBetweenFactsSayTheClosestRecording(t *testing.T) {
	for _, c := range []struct {
		call models.UnmatchedCall
		want string
	}{
		{models.UnmatchedCall{ClosestMock: "mock-3"}, "the closest recording is mock-3, which it does not match"},
		{models.UnmatchedCall{Differs: []string{"body.qty"}}, "no recording of this call in the set"},
		{models.UnmatchedCall{ClosestMock: "mock-3", Differs: []string{"body.qty"}}, "the closest recording is mock-3, which it differs from in body.qty"},
	} {
		if got := betweenFacts(c.call); got != c.want {
			t.Errorf("betweenFacts(%+v) = %q; want %q", c.call, got, c.want)
		}
	}
}

// A call the application made in a test that keploy decided only after that
// test's end was read reaches a later test's read marked Late. It fails the
// set (the run is red) in the same documented form, named under the test it
// was made in (made_in), and charges no test: neither the test it was made
// in, whose end was read without it, nor the test whose read took it. With
// no mark naming its test, it is named under the test read before it.
func TestRunTestSet_ALateInTestMissFailsTheSetNamedUnderItsTest(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 3, 0)
	logs := observeSetLogs(h)
	h.instr.mockErrorsFor = misses(nil, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "find on carts", Destination: "mongo:27017",
			Kind: models.MissInTest, Late: true}}, nil
	}, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /charge", Destination: "pay:443",
			Kind: models.MissInTest, Late: true, MadeIn: "test-1", ClosestMock: "mock-2", Differs: []string{"body.qty"}}}, nil
	})

	status := h.run(t)

	rep := h.report.report
	for _, r := range rep.Tests {
		if r.Status != models.TestStatusPassed || len(r.FailureInfo.UnmatchedCalls) != 0 {
			t.Fatalf("test %s is %s with %+v; a late call is no test's", r.TestCaseID, r.Status, r.FailureInfo.UnmatchedCalls)
		}
	}
	if status != models.TestSetStatusFailed || rep.Status != string(models.TestSetStatusFailed) {
		t.Fatalf("status %s (report %s); a late in-test call that no mock answered must fail the set", status, rep.Status)
	}
	if len(rep.UnclaimedCalls) != 2 {
		t.Fatalf("unclaimed calls %+v; want the find and the charge", rep.UnclaimedCalls)
	}
	if c := rep.UnclaimedCalls[0]; !c.Late || c.MadeIn != "test-1" || c.MadeAfter != "test-1" || c.MadeBefore != "test-2" {
		t.Fatalf("the find reads %+v; want it Late, made in test-1 (the test read before it), read at test-2", c)
	}
	if c := rep.UnclaimedCalls[1]; !c.Late || c.MadeIn != "test-1" || c.MadeAfter != "test-2" || c.MadeBefore != "test-3" {
		t.Fatalf("the charge reads %+v; want it Late, made in test-1 (its mark), read at test-3", c)
	}
	for _, want := range []string{"2 call(s) in a test that no mock answered and keploy decided only after that test ended", models.KindOutsideEveryTest,
		"made in test test-1, decided after that test ended, read at test test-2's end", "read at test test-3's end"} {
		if !strings.Contains(rep.FailureReason, want) {
			t.Fatalf("failure reason %q does not say %q", rep.FailureReason, want)
		}
	}
	lines := outsideLines(logs)
	if len(lines) != 2 {
		t.Fatalf("%d outside_every_test lines; want two: %+v", len(lines), logs.All())
	}
	for i, want := range []map[string]string{
		{"protocol": "Mongo", "call": "find on carts", "made_in": "test-1", "after": "test-1", "before": "test-2", "closest": "",
			"facts": "no recording of this call in the set", "next_step": "record the test set again in its place: " + recordAgainOf(h, "test-set-0")},
		{"protocol": "HTTP", "call": "POST /charge", "made_in": "test-1", "after": "test-2", "before": "test-3", "closest": "mock-2",
			"facts":     "the closest recording is mock-2, which it differs from in body.qty",
			"next_step": "if the application changed this call on purpose, record the test set again in its place: " + recordAgainOf(h, "test-set-0")},
	} {
		if !strings.Contains(lines[i].Message, "decided it only after that test ended") {
			t.Errorf("line %d says %q; want the late in-test form", i, lines[i].Message)
		}
		f := lines[i].ContextMap()
		for k, w := range want {
			if got := f[k]; got != w {
				t.Errorf("line %d field %s = %v; want %q", i, k, got, w)
			}
		}
	}
}

// A late in-test call that is the same request as one of the set's
// background recordings fails nothing (unchanged): the application's
// background work made it. One a parser held for a later test is said as
// held, and one made before the set's first test as made then: neither is
// said as a call outside every test.
func TestRunTestSet_ABackgroundOrHeldLateCallIsNoOutsideLine(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.instr.mockErrorsFor = misses(nil, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "ping", Kind: models.MissInTest, Late: true, MadeIn: "test-1", Background: true}}, nil
	})
	if status := h.run(t); status != models.TestSetStatusPassed {
		t.Fatalf("status %s; a background repeat fails nothing", status)
	}

	h = newPartialRunHarness(t, 2, 0)
	logs := observeSetLogs(h)
	h.instr.mockErrorsFor = misses(func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "Mongo", ActualSummary: "hello", Kind: models.MissOutsideTest, BeforeSet: true}}, nil
	}, func() ([]models.UnmatchedCall, error) {
		return []models.UnmatchedCall{{Protocol: "Kafka", ActualSummary: "fetch orders", Kind: models.MissInTest, Late: true, MadeIn: "test-1", Held: true}}, nil
	})
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %s; want FAILED for the held call", status)
	}
	if lines := outsideLines(logs); len(lines) != 0 {
		t.Fatalf("a held call was said outside every test: %+v", lines)
	}
	if !strings.Contains(h.report.report.FailureReason, "held for a later test's recording") {
		t.Fatalf("failure reason %q; want the held clause", h.report.report.FailureReason)
	}
}

// Where a late call was made is said from what keploy knows: the test it was
// made in, when named, and the test whose read took it, when one did.
func TestLateWhereSaysWhatKeployKnows(t *testing.T) {
	for _, c := range []struct {
		call models.UnmatchedCall
		want string
	}{
		{models.UnmatchedCall{Late: true}, "made in a test of the set, decided after that test ended"},
		{models.UnmatchedCall{Late: true, MadeIn: "test-1"}, "made in test test-1, decided after that test ended"},
		{models.UnmatchedCall{Late: true, MadeIn: "test-1", MadeBefore: "test-2"}, "made in test test-1, decided after that test ended, read at test test-2's end"},
	} {
		if got := lateWhere(c.call); got != c.want {
			t.Errorf("lateWhere(%+v) = %q; want %q", c.call, got, c.want)
		}
	}
}

// A set the replay failed for its calls outside every test alone says so in
// its report (OutsideOnly), whether every test of it passed or one failed for
// a call of its own: those calls fail the run and no test, so a reader that
// charges a set's own failure to all its tests charges them nothing. A set
// that fails for something else as well (a call before its first test, one a
// parser held for a later test's recording, one after its last test), or
// that holds no call outside every test, does not say it.
func TestRunTestSet_ASetFailedOnlyOutsideEveryTestSaysSo(t *testing.T) {
	read := func(c models.UnmatchedCall) func() ([]models.UnmatchedCall, error) {
		return func() ([]models.UnmatchedCall, error) { return []models.UnmatchedCall{c}, nil }
	}
	before := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "hello", Kind: models.MissOutsideTest, BeforeSet: true}
	late := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /charge", Kind: models.MissInTest, Late: true, MadeIn: "test-1"}
	held := models.UnmatchedCall{Protocol: "Kafka", ActualSummary: "fetch orders", Kind: models.MissInTest, Late: true, MadeIn: "test-1", Held: true}
	own := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "find on carts", Kind: models.MissInTest}
	for _, tc := range []struct {
		name   string
		reads  []func() ([]models.UnmatchedCall, error)
		failed string
		want   bool
	}{
		{name: "a call between two tests", reads: []func() ([]models.UnmatchedCall, error){nil, betweenWrite}, want: true},
		{name: "a late in-test call", reads: []func() ([]models.UnmatchedCall, error){nil, read(late)}, want: true},
		{name: "a call between two tests and a test's own miss", reads: []func() ([]models.UnmatchedCall, error){nil, betweenWrite, read(own)}, failed: "test-3", want: true},
		{name: "a call between two tests and one before the first", reads: []func() ([]models.UnmatchedCall, error){read(before), betweenWrite}},
		{name: "a call between two tests and a held one", reads: []func() ([]models.UnmatchedCall, error){nil, betweenWrite, read(held)}},
		{name: "a call between two tests and one after the last", reads: []func() ([]models.UnmatchedCall, error){nil, betweenWrite, nil, afterLast}},
		{name: "a test's own miss alone", reads: []func() ([]models.UnmatchedCall, error){nil, read(own)}, failed: "test-2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provenOn(t, true)
			h := newPartialRunHarness(t, 3, 0)
			h.instr.mockErrorsFor = misses(tc.reads...)
			if status := h.run(t); status != models.TestSetStatusFailed {
				t.Fatalf("status %s, want FAILED", status)
			}
			rep := h.report.report
			if rep.OutsideOnly != tc.want {
				t.Fatalf("OutsideOnly %v, want %v (reason %q, calls %+v)", rep.OutsideOnly, tc.want, rep.FailureReason, rep.UnclaimedCalls)
			}
			for _, r := range rep.Tests {
				want := models.TestStatusPassed
				if r.TestCaseID == tc.failed {
					want = models.TestStatusFailed
				}
				if r.Status != want {
					t.Fatalf("test %s is %s, want %s", r.TestCaseID, r.Status, want)
				}
			}
		})
	}
}

// failedOutsideOnly holds only for a FAILED set whose reason, its calls
// outside every test left out, is empty, that ends as its tests end without
// them, and that holds such a call.
func TestFailedOutsideOnlyNeedsEachOfItsFacts(t *testing.T) {
	between := []models.UnmatchedCall{{Protocol: "Mongo", Between: true}}
	if !failedOutsideOnly(models.TestSetStatusFailed, between, true, "") {
		t.Fatal("a FAILED set with only a call between two tests is not OutsideOnly")
	}
	for name, got := range map[string]bool{
		"PASSED":       failedOutsideOnly(models.TestSetStatusPassed, between, true, ""),
		"INTERNAL_ERR": failedOutsideOnly(models.TestSetStatusInternalErr, between, true, ""),
		"FAILED before the calls, no test failed": failedOutsideOnly(models.TestSetStatusFailed, between, false, ""),
		"a reason of its own":                     failedOutsideOnly(models.TestSetStatusFailed, between, true, "the run never reached mock.real"),
		"no call outside every test":              failedOutsideOnly(models.TestSetStatusFailed, []models.UnmatchedCall{{BeforeSet: true}}, true, ""),
		"a held late call":                        failedOutsideOnly(models.TestSetStatusFailed, []models.UnmatchedCall{{Late: true, Held: true}}, true, ""),
	} {
		if got {
			t.Errorf("%s: OutsideOnly; want not", name)
		}
	}
}

// A set whose worker scope the application never made fails for that too:
// it is not OutsideOnly, though a call between two tests is in it; with the
// worker scope made, the same set is.
func TestUnderTheSwitchASetWhoseWorkerScopeIsNotMadeIsNotOutsideOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		made map[string][]models.MockState
		want bool
	}{
		{"the boot call made in a test", map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010), mongoAt("mock-boot", 500)}}, true},
		{"the boot call never made", map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
			mappingFile(t, h, nil)
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, tc.made)}
			h.instr.mockErrorsFor = misses(nil, betweenWrite)
			if got := h.run(t); got != models.TestSetStatusFailed {
				t.Fatalf("set %s, want FAILED for the call between the tests", got)
			}
			rep := h.report.report
			if rep.OutsideOnly != tc.want {
				t.Fatalf("OutsideOnly %v, want %v (reason %q)", rep.OutsideOnly, tc.want, rep.FailureReason)
			}
			if worker := strings.Contains(rep.FailureReason, "worker scope"); worker == tc.want {
				t.Fatalf("reason %q: the worker scope said %v, want %v", rep.FailureReason, worker, !tc.want)
			}
		})
	}
}

// The ERROR line's next step names the set it fails: a set the run names,
// whose name keploy record would not record under (it reads 123 as a
// number), is recorded again under its own name, never named as an earlier
// set's; a call of an earlier set the run cannot name (set "") says that
// set. Mutation: hand betweenNextStep no set.
func TestOutsideFieldsRecordTheSetTheyNameAgain(t *testing.T) {
	r := &Replayer{config: &config.Config{Path: "/w/keploy", Command: "go run ."}}
	for _, c := range []struct{ set, want string }{
		{"123", "record the test set 123 again under its own name, with the command that recorded it; keploy cannot print a command that records it in its place"},
		{"", "record the earlier test set that made this call again, with the command that recorded it; keploy cannot name that set, and recording this set again does not hold the call"},
	} {
		core, logs := observer.New(zapcore.ErrorLevel)
		zap.New(core).Error("call outside every test", r.outsideFields(c.set, "", models.UnmatchedCall{Between: true})...)
		if got := logs.All()[0].ContextMap()["next_step"]; got != c.want {
			t.Fatalf("set %q: next step %q; want %q", c.set, got, c.want)
		}
	}
}
