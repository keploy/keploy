package replay

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// setRecordAgainer is the run's hooks with SetRecordAgainer: a replay whose
// sets are recorded again where they are kept, by set (a set it holds no
// entry for it does not say), and the sets it was asked of, in order.
type setRecordAgainer struct {
	TestHooks
	by    map[string]utils.RecordAgain
	mu    sync.Mutex
	asked []string
}

func (h *setRecordAgainer) RecordSetAgain(testSetID string) (utils.RecordAgain, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, testSetID)
	a, ok := h.by[testSetID]
	return a, ok
}

// A set the hooks say how to record again is recorded again as they say,
// never by the command the replay would print for the folder it read: their
// step in words when they print no command, their command when they do, and,
// when they give neither, the step that says keploy cannot print one. A set
// they do not say is recorded again by the replay's command. They are never
// asked of a set the run cannot name.
func TestASetRecordAgainerNamesHowItsSetIsRecordedAgain(t *testing.T) {
	const say = "record this set again in the cluster for app shop/orders"
	hooks := &setRecordAgainer{by: map[string]utils.RecordAgain{
		"test-set-1": {Say: say},
		"test-set-2": {Command: "cluster-record test-set-2"},
		"test-set-4": {Command: "cluster-record test-set-4", Say: say},
		"test-set-3": {},
	}}
	r := &Replayer{config: &config.Config{Path: "/w/keploy", Command: "go run ."}, setRunID: "test-run-2", hookImpl: hooks}
	for _, c := range []struct {
		set  string
		call models.UnmatchedCall
		want string
	}{
		{"test-set-1", models.UnmatchedCall{Between: true}, say},
		{"test-set-1", models.UnmatchedCall{Between: true, ClosestMock: "mock-4"}, "if the application changed this call on purpose, " + say},
		{"test-set-2", models.UnmatchedCall{Between: true}, "record the test set again in its place: cluster-record test-set-2"},
		{"test-set-4", models.UnmatchedCall{Between: true}, "record the test set again in its place: cluster-record test-set-4"},
		{"test-set-3", models.UnmatchedCall{Between: true}, "record the test set test-set-3 again under its own name, with the command that recorded it; keploy cannot print a command that records it in its place"},
		{"test-set-5", models.UnmatchedCall{Between: true}, "record the test set again in its place: " + utils.RecordSetAgain("/w/keploy", "", "", "test-set-5", "test-run-2", "go run .").Text()},
		{"", models.UnmatchedCall{Between: true}, "record the earlier test set that made this call again, with the command that recorded it; keploy cannot name that set, and recording this set again does not hold the call"},
	} {
		core, logs := observer.New(zapcore.ErrorLevel)
		zap.New(core).Error("call outside every test", r.outsideFields(c.set, "", c.call)...)
		got := logs.All()[0].ContextMap()["next_step"]
		if got != c.want {
			t.Fatalf("set %q: next step %q; want %q", c.set, got, c.want)
		}
		if s := got.(string); c.set != "test-set-5" && (strings.Contains(s, "mkdir") || strings.Contains(s, "keploy record")) {
			t.Fatalf("set %q: next step %q prints the command for the folder the replay read", c.set, s)
		}
	}
	if want := []string{"test-set-1", "test-set-1", "test-set-2", "test-set-4", "test-set-3", "test-set-5"}; !slices.Equal(hooks.asked, want) {
		t.Fatalf("the hooks were asked of %q; want %q, never of a set the run cannot name", hooks.asked, want)
	}
	for _, c := range []struct {
		a    utils.RecordAgain
		want string
	}{
		{utils.RecordAgain{Say: say}, say},
		{utils.RecordAgain{Command: "cluster-record test-set-2", Say: say}, "cluster-record test-set-2"},
		{utils.RecordAgain{}, ""},
	} {
		if got := c.a.Text(); got != c.want {
			t.Fatalf("%+v reads %q; want %q", c.a, got, c.want)
		}
	}
}

// outsideCallsSayer is the run's hooks with OutsideCallsLogger and
// EarlierSetCallHook (and, as sayingAgainer, SetRecordAgainer): what they
// were handed to say, in order. silent hooks are handed them and say none
// (a composed hook value none of whose hooks says such calls).
type outsideCallsSayer struct {
	TestHooks
	record *utils.RecordAgain
	silent bool
	mu     sync.Mutex
	said   []outsideCallsSaid
	told   []string
}

type outsideCallsSaid struct {
	set    string
	calls  []models.UnmatchedCall
	record utils.RecordAgain
}

func (h *outsideCallsSayer) LogOutsideCalls(set string, calls []models.UnmatchedCall, record utils.RecordAgain) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.said = append(h.said, outsideCallsSaid{set: set, calls: slices.Clone(calls), record: record})
	return !h.silent
}

func (h *outsideCallsSayer) EarlierSetCall(set, readIn string, _ models.UnmatchedCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.told = append(h.told, set+"<"+readIn)
}

// sayingAgainer is an outsideCallsSayer that also says how its sets are
// recorded again.
type sayingAgainer struct {
	*outsideCallsSayer
}

func (h sayingAgainer) RecordSetAgain(string) (utils.RecordAgain, bool) { return *h.record, true }

// Hooks that say calls outside every test themselves are handed the set's
// such calls, all of them at once and no other call, with how the set is
// recorded again as the replay's line would name it (the hooks' own answer
// when they give one), and the replay says none of them itself; the set's
// other calls it still says. A set with no such call hands them nothing.
// Hooks that are handed them and say none leave the replay to say each.
func TestAnOutsideCallsLoggerSaysTheSetsCallsInPlaceOfTheReplay(t *testing.T) {
	before := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "insert on events", Destination: "mongo:27017", Kind: models.MissOutsideTest, BeforeSet: true}
	between := models.UnmatchedCall{Protocol: "Mongo", ActualSummary: "delete on sessions", Destination: "mongo:27017", Kind: models.MissOutsideTest, Between: true, MadeAfter: "test-1", MadeBefore: "test-2"}
	late := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /orders", Destination: "orders:80", Kind: models.MissInTest, Late: true, MadeIn: "test-2"}
	held := models.UnmatchedCall{Protocol: "Kafka", ActualSummary: "Fetch orders", Destination: "kafka:9092", Kind: models.MissInTest, Late: true, Held: true}
	cfg := &config.Config{Path: "/w/keploy", Command: "go run ."}
	own := utils.RecordSetAgain("/w/keploy", "", "", "test-set-0", "test-run-1", "go run .")
	if own.Command == "" {
		t.Fatal("no command records test-set-0 again; the row reads nothing")
	}

	// Without the hooks, the replay says each call.
	core, logs := observer.New(zapcore.ErrorLevel)
	(&Replayer{logger: zap.New(core), config: cfg, setRunID: "test-run-1"}).logSetCalls("test-set-0", []models.UnmatchedCall{before, between, late, held})
	if n, k := logs.Len(), len(outsideLines(logs)); n != 4 || k != 2 {
		t.Fatalf("with no hooks %d lines, %d outside every test; want 4 and 2", n, k)
	}

	for _, c := range []struct {
		name   string
		hooks  func(*outsideCallsSayer) TestHooks
		record utils.RecordAgain
	}{
		{"the replay's command", func(s *outsideCallsSayer) TestHooks { return s }, own},
		{"the hooks' own answer", func(s *outsideCallsSayer) TestHooks { return sayingAgainer{s} }, utils.RecordAgain{Say: "record this set again in the cluster for app shop/orders"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sayer := &outsideCallsSayer{record: &c.record}
			core, logs := observer.New(zapcore.ErrorLevel)
			r := &Replayer{logger: zap.New(core), config: cfg, setRunID: "test-run-1", hookImpl: c.hooks(sayer)}
			r.logSetCalls("test-set-0", []models.UnmatchedCall{before, between, late, held})
			if k := len(outsideLines(logs)); k != 0 {
				t.Fatalf("the replay said %d calls outside every test itself; the hooks say them", k)
			}
			if n := logs.Len(); n != 2 || !strings.Contains(logs.All()[0].Message, "before its first test") || !strings.Contains(logs.All()[1].Message, "held a call") {
				t.Fatalf("the replay said %+v; want the call before the first test and the held call", logs.All())
			}
			if len(sayer.said) != 1 {
				t.Fatalf("the hooks were handed %d batches; want the set's calls once", len(sayer.said))
			}
			got := sayer.said[0]
			if got.set != "test-set-0" || len(got.calls) != 2 || got.calls[0].ActualSummary != "delete on sessions" || got.calls[1].ActualSummary != "POST /orders" {
				t.Fatalf("the hooks were handed %+v; want test-set-0's delete between tests and its late POST", got)
			}
			if got.record != c.record {
				t.Fatalf("the hooks were handed record %+v; want %+v", got.record, c.record)
			}
			r.logSetCalls("test-set-0", []models.UnmatchedCall{before})
			if len(sayer.said) != 1 {
				t.Fatalf("a set with no call outside every test handed the hooks %+v", sayer.said[1:])
			}
		})
	}

	silent := &outsideCallsSayer{silent: true}
	core, logs = observer.New(zapcore.ErrorLevel)
	(&Replayer{logger: zap.New(core), config: cfg, setRunID: "test-run-1", hookImpl: silent}).logSetCalls("test-set-0", []models.UnmatchedCall{before, between, late, held})
	if n, k := logs.Len(), len(outsideLines(logs)); n != 4 || k != 2 || len(silent.said) != 1 {
		t.Fatalf("hooks that said none: %d lines, %d outside every test, handed %d batches; want 4, 2 and 1", n, k, len(silent.said))
	}
}

// An earlier set's call that fails the run is handed to hooks that say such
// calls themselves, under the set it was made in, with how that set is
// recorded again, in place of the replay's line; the hooks are still told it
// (EarlierSetCallHook). A call read in a run's first set names no set, and
// no command.
func TestAnOutsideCallsLoggerSaysAnEarlierSetsCallInPlaceOfTheReplay(t *testing.T) {
	core, logs := observer.New(zap.ErrorLevel)
	sayer := &outsideCallsSayer{}
	r := &Replayer{logger: zap.New(core), hookImpl: sayer, config: &config.Config{Path: "/w/keploy", Command: "go run ."}, setRunID: "test-run-1"}
	r.earlierSetCalls.begin("test-set-a")
	r.earlierSetCalls.begin("test-set-b")
	calls, _ := lateOfSetA()
	r.takeEarlierSetCalls(calls)
	if n := logs.Len(); n != 0 {
		t.Fatalf("the replay said %+v itself; the hooks say it", logs.All())
	}
	want := utils.RecordSetAgain("/w/keploy", "", "", "test-set-a", "test-run-1", "go run .")
	if len(sayer.said) != 1 || sayer.said[0].set != "test-set-a" || len(sayer.said[0].calls) != 1 ||
		sayer.said[0].calls[0].ActualSummary != "POST /orders" || sayer.said[0].record.Command != want.Command || want.Command == "" {
		t.Fatalf("the hooks were handed %+v; want test-set-a's POST /orders and the command that records test-set-a again", sayer.said)
	}
	if !slices.Equal(sayer.told, []string{"test-set-a<test-set-b"}) {
		t.Fatalf("the hooks were told %q; want the call of test-set-a read in test-set-b", sayer.told)
	}
	r.earlierSetCalls.reset()
	r.earlierSetCalls.begin("test-set-c")
	r.takeEarlierSetCalls(calls)
	if len(sayer.said) != 2 || sayer.said[1].set != "" || sayer.said[1].record != (utils.RecordAgain{}) {
		t.Fatalf("the hooks were handed %+v; want the call read in a run's first set under no set, with no command", sayer.said)
	}
	if logs.Len() != 0 {
		t.Fatalf("the replay said %+v itself", logs.All())
	}
	sayer.silent = true
	r.earlierSetCalls.begin("test-set-d")
	r.takeEarlierSetCalls(calls)
	if len(sayer.said) != 3 || len(outsideLines(logs)) != 1 {
		t.Fatalf("hooks that said none: handed %d, the replay said %+v; want it handed and said by the replay", len(sayer.said), logs.All())
	}
}

// Through a whole set's run: hooks that say calls outside every test are
// handed the call made between two tests, with the command that records the
// set again for the run's id, and the replay's own line is not said; the set
// still fails and no test does.
func TestRunTestSet_AnOutsideCallsLoggerSaysTheCallBetweenTwoTests(t *testing.T) {
	provenOn(t, true)
	h := newPartialRunHarness(t, 3, 0)
	logs := observeSetLogs(h)
	sayer := &outsideCallsSayer{TestHooks: h.replayer.hookImpl}
	h.replayer.hookImpl = sayer
	h.instr.mockErrorsFor = misses(nil, betweenWrite)

	status := h.run(t)

	if status != models.TestSetStatusFailed {
		t.Fatalf("status %s; the call between two tests fails the set", status)
	}
	for _, r := range h.report.report.Tests {
		if r.Status != models.TestStatusPassed {
			t.Fatalf("test %s is %s; the call between the tests is no test's", r.TestCaseID, r.Status)
		}
	}
	if k := len(outsideLines(logs)); k != 0 {
		t.Fatalf("the replay said %d outside_every_test lines itself; the hooks say them", k)
	}
	if len(sayer.said) != 1 || sayer.said[0].set != "test-set-0" || len(sayer.said[0].calls) != 1 {
		t.Fatalf("the hooks were handed %+v; want test-set-0's delete", sayer.said)
	}
	c := sayer.said[0].calls[0]
	if !c.Between || c.MadeAfter != "test-1" || c.MadeBefore != "test-2" || c.ActualSummary != "delete on sessions" {
		t.Fatalf("the hooks were handed %+v; want the delete between test-1 and test-2", c)
	}
	if got := sayer.said[0].record.Text(); got != recordAgainOf(h, "test-set-0") {
		t.Fatalf("the hooks were handed record %q; want %q", got, recordAgainOf(h, "test-set-0"))
	}
}
