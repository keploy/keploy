package replay

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// httpAt is a recorded HTTP mock whose request was recorded ms milliseconds
// after attr0 (see pinWindows: test-i's window is [i s, i s + 100 ms]).
func httpAt(name string, ms int) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		ReqTimestampMock: attrAt(ms), ResTimestampMock: attrAt(ms + 1),
	}}
}

// exactOpened is each test case's exact-serving call: test:owned,owned.
func exactOpened(h *prRun) []string {
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	var opened []string
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Test != "" {
			opened = append(opened, p.Exact.Test+":"+strings.Join(p.Exact.Owned, ","))
		}
	}
	return opened
}

// Under the switch a set with no mappings.yaml (a recording made without
// the switch: keploy record derives one only under it) owns what its test
// windows held: each test case the calls recorded inside its own window, as
// keploy record derives mappings.yaml under the switch. A call recorded
// outside every window is the set's worker scope (D2), not the pool's. Each
// test case is judged for what it owns; nothing is written to mappings.yaml.
func TestUnderTheSwitchASetWithNoMappingsOwnsWhatItsWindowsHeld(t *testing.T) {
	for _, tc := range []struct {
		name    string
		made    map[string][]models.MockState
		results map[string]models.TestStatus
	}{
		{"each test case makes its recorded call",
			map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-b", 2010)}},
			map[string]models.TestStatus{"test-1": models.TestStatusPassed, "test-2": models.TestStatusPassed}},
		{"test-2 does not make its recorded call",
			map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}},
			map[string]models.TestStatus{"test-1": models.TestStatusPassed, "test-2": models.TestStatusFailed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010), httpAt("mock-b", 2010)}
			db := mappingFile(t, h, nil)
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, tc.made)}
			_ = h.run(t)
			if want := []string{"test-1:mock-a", "test-2:mock-b"}; !reflect.DeepEqual(exactOpened(h), want) {
				t.Fatalf("opened %v, want %v", exactOpened(h), want)
			}
			if pool, worker := stagedPoolAndWorker(h); len(pool) != 0 || !reflect.DeepEqual(worker, []string{"mock-boot@"}) {
				t.Fatalf("pool %v and worker scope %v; want no pool and mock-boot in the worker scope at the set's start", pool, worker)
			}
			for test, want := range tc.results {
				if got := statusOf(t, h, test); got != want {
					t.Fatalf("%s: %s, want %s", test, got, want)
				}
			}
			if on := mappedOnDisk(t, db); on != nil {
				t.Fatalf("the replay wrote mappings.yaml: %v", on)
			}
			if n := len(logs.FilterMessageSnippet("owns the calls recorded inside its own window").All()); n != 1 {
				t.Fatalf("said how the set's ownership was derived %d times, want once", n)
			}
			if n := len(logs.FilterMessageSnippet("no test case of this test set owns a recorded call").All()); n != 0 {
				t.Fatal("said the set owns nothing")
			}
		})
	}
}

// A mappings.yaml that lists every test case with no mock is the set's
// mapping, not a missing one: its test cases own nothing, whatever their
// windows held.
func TestUnderTheSwitchAnAllEmptyMappingIsNotDerivedFromWindows(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010)}
	mappingFile(t, h, map[string][]models.MockEntry{"test-1": {}, "test-2": {}})
	_ = h.run(t)
	if want := []string{"test-1:", "test-2:"}; !reflect.DeepEqual(exactOpened(h), want) {
		t.Fatalf("opened %v, want %v", exactOpened(h), want)
	}
}

// skippingMockDB is a mock store whose decoders skipped documents
// (models.TestSetMocks.Skipped, SkippedWindowed): a kind this keploy cannot
// read.
type skippingMockDB struct {
	*prMockDB
	skipped []models.WindowedMock
	// session is the set's session pool.
	session []*models.Mock
}

func (m skippingMockDB) GetTestSetMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) (models.TestSetMocks, error) {
	set := models.TestSetMocks{Filtered: m.filtered, AllPerTest: m.filtered, Unfiltered: m.session, AllSession: m.session, SkippedWindowed: m.skipped}
	for _, w := range m.skipped {
		if set.Skipped == nil {
			set.Skipped = map[string]models.Kind{}
		}
		set.Skipped[w.Name] = w.Kind
	}
	return set, nil
}

// Under the switch, a set with no mappings.yaml owns what its windows held,
// documents this keploy cannot read included: a test case whose window holds
// one owns that recorded call and, since keploy cannot serve it, is judged
// for it; one that never recorded it is not.
func TestUnderTheSwitchATestOwnsADocumentTheDecodersSkipped(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-b", 2010)}
	h.replayer.mockDB = skippingMockDB{prMockDB: h.mocks, skipped: []models.WindowedMock{
		{Name: "mock-x", Kind: "Aerospike", At: attrAt(1020), Res: attrAt(1021), TestMock: true},
	}}
	mappingFile(t, h, nil)
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
		"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-b", 2010)},
	})}
	_ = h.run(t)
	for test, want := range map[string]models.TestStatus{"test-1": models.TestStatusFailed, "test-2": models.TestStatusPassed} {
		if got := statusOf(t, h, test); got != want {
			t.Fatalf("%s: %s, want %s", test, got, want)
		}
	}
}

// A mappings.yaml keploy test wrote lists what a replay's passing tests
// consumed, not what each test recorded. Under the switch it does not say
// what a test owns: its windows do. A test that recorded two calls, made one
// in a replay with the switch off (which wrote the file), and makes one
// again under it, fails for the other.
func TestUnderTheSwitchAMappingKeployTestWroteIsNotTrusted(t *testing.T) {
	mocks := func() []*models.Mock { return []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-b", 1020)} }
	made := map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}}

	setSwitch(t, false)
	off := newPartialRunHarness(t, 1, 0)
	pinWindows(off)
	off.mocks.filtered = mocks()
	db := mappingFile(t, off, nil)
	off.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(off, made)}
	_ = off.run(t)
	if on := mappedOnDisk(t, db); !reflect.DeepEqual(on, map[string][]string{"test-1": {"mock-a"}}) {
		t.Fatalf("the replay with the switch off wrote %v, want test-1: mock-a", on)
	}
	if src, err := db.GetSource(context.Background(), "test-set-0"); err != nil || src != models.MappingSourceKeployTest {
		t.Fatalf("the file's source is %q (%v), want %q", src, err, models.MappingSourceKeployTest)
	}

	setSwitch(t, true)
	on := newPartialRunHarness(t, 1, 0)
	pinWindows(on)
	core, logs := observer.New(zap.InfoLevel)
	on.replayer.logger = zap.New(core)
	on.mocks.filtered = mocks()
	on.replayer.mappingDB = db
	on.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(on, made)}
	_ = on.run(t)
	if want := []string{"test-1:mock-a,mock-b"}; !reflect.DeepEqual(exactOpened(on), want) {
		t.Fatalf("opened %v, want %v", exactOpened(on), want)
	}
	if got := statusOf(t, on, "test-1"); got != models.TestStatusFailed {
		t.Fatalf("test-1: %s, want FAILED: it did not make mock-b", got)
	}
	if n := len(logs.FilterField(zap.String("reason", "the test set's mappings.yaml was written by keploy test, from what a replay consumed, not by keploy record")).All()); n != 1 {
		t.Fatalf("said why the windows decide %d times, want once", n)
	}
}

// Under the switch a mappings.yaml keploy record wrote, which names no
// source, is the set's ownership, as recorded.
func TestUnderTheSwitchAMappingKeployRecordWroteIsTrusted(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 1, 0)
	pinWindows(h)
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-b", 1020)}
	db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": {{Name: "mock-a", Kind: string(models.HTTP)}}})
	if src, _ := db.GetSource(context.Background(), "test-set-0"); src != "" {
		t.Fatalf("keploy record's file names source %q", src)
	}
	_ = h.run(t)
	if want := []string{"test-1:mock-a"}; !reflect.DeepEqual(exactOpened(h), want) {
		t.Fatalf("opened %v, want %v", exactOpened(h), want)
	}
}

// Window ownership reads both pools: a test mock DeriveLifetime files in the
// session pool (an untagged HTTP mock under the lax fallback) is owned by the
// test whose window holds it. And it is derived from every test case of the
// set, run or not: in a run of test-2 alone, the call test-1 recorded is
// test-1's, neither test-2's nor the pool's.
func TestUnderTheSwitchWindowOwnershipReadsBothPoolsAndEveryTestCase(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Test.SelectedTests = map[string][]string{"test-set-0": {"test-2"}}
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010)}
	h.replayer.mockDB = skippingMockDB{prMockDB: h.mocks, session: []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-b", 2010)}}
	mappingFile(t, h, nil)
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-2": {mongoAt("mock-b", 2010)}})}
	_ = h.run(t)
	if want := []string{"test-2:mock-b"}; !reflect.DeepEqual(exactOpened(h), want) {
		t.Fatalf("opened %v, want %v", exactOpened(h), want)
	}
	if pool, worker := stagedPoolAndWorker(h); len(pool) != 0 || !reflect.DeepEqual(worker, []string{"mock-boot@"}) {
		t.Fatalf("pool %v and worker scope %v; want no pool and mock-boot in the worker scope at the set's start: test-1's call is test-1's", pool, worker)
	}
}

// stagedPoolAndWorker is the set's staging call's pool and worker scope
// (models.ExactServing), each worker entry "name@place".
func stagedPoolAndWorker(h *prRun) (pool, worker []string) {
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Stage {
			pool, worker = p.Exact.Pool, nil
			for _, e := range p.Exact.Worker {
				worker = append(worker, e.Name+"@"+e.Place)
			}
		}
	}
	return pool, worker
}

// fastWorkerWait makes the set's end's wait for its worker scope
// (awaitWorkerScope) give up at its first wake-up with nothing answered, and
// wake at once: a harness's application makes no call of its own.
func fastWorkerWait(t *testing.T) {
	t.Helper()
	prevLimit, prevWake := workerStallLimit, workerWake
	t.Cleanup(func() { workerStallLimit, workerWake = prevLimit, prevWake })
	workerStallLimit = 1
	workerWake = func(context.Context) (<-chan time.Time, func()) {
		tk := time.NewTicker(time.Millisecond)
		return tk.C, tk.Stop
	}
}

// genericAt is a recorded Generic exchange whose request was recorded ms
// milliseconds after attr0, as its recorder states it: a connection's
// preamble session, a later exchange per-test (type "config" either way).
func genericAt(name string, ms int, stated string) *models.Mock {
	return &models.Mock{Name: name, Kind: models.GENERIC, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "config", models.MetadataLifetimeKey: stated},
		ReqTimestampMock: attrAt(ms), ResTimestampMock: attrAt(ms + 1),
	}}
}

// genericState is a Generic exchange the agent reports consumed under exact
// serving: a test mock though its type is "config" (MockState.DataPlane).
func genericState(name string, ms int) models.MockState {
	m := mongoAt(name, ms)
	m.Kind, m.Type, m.DataPlane = models.GENERIC, "config", true
	return m
}

// Under the switch a test owns the Generic exchanges its window holds after
// their connection's preamble, and is judged for them: one it does not make
// fails it, and one it makes counts though its type tag is "config". The
// preamble is the pool's.
func TestUnderTheSwitchATestIsJudgedForItsGenericExchanges(t *testing.T) {
	for _, tc := range []struct {
		name string
		made map[string][]models.MockState
		want models.TestStatus
	}{
		{"test-1 makes its exchange", map[string][]models.MockState{"test-1": {genericState("mock-get", 1020)}}, models.TestStatusPassed},
		{"test-1 does not make it", map[string][]models.MockState{}, models.TestStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.mocks.filtered = []*models.Mock{
				genericAt("mock-hello", 1010, models.MetadataLifetimeSession),
				genericAt("mock-get", 1020, models.MetadataLifetimePerTest),
			}
			mappingFile(t, h, nil)
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, tc.made)}
			_ = h.run(t)
			if want := []string{"test-1:mock-get"}; !reflect.DeepEqual(exactOpened(h), want) {
				t.Fatalf("opened %v, want %v", exactOpened(h), want)
			}
			if got := statusOf(t, h, "test-1"); got != tc.want {
				t.Fatalf("test-1: %s, want %s", got, tc.want)
			}
		})
	}
}

// A call no mock answered that is a recording its test does not own is named
// with the first other test that owns it; one its own test owns too, one no
// test owns, and one the parser only found near (not NotOwned), are not. A
// changed call naming an earlier recording (Earlier) names its owner as
// EarlierIn.
func TestMarkRecordedInNamesTheTestThatOwnsTheRecording(t *testing.T) {
	results := []models.TestResult{{TestCaseID: "test-2", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
		{ActualSummary: "auth", ClosestMock: "mock-auth", NotOwned: true},
		{ActualSummary: "get", ClosestMock: "mock-get", NotOwned: true},
		{ActualSummary: "boot", ClosestMock: "mock-boot", NotOwned: true},
		{ActualSummary: "near", ClosestMock: "mock-auth"},
		{ActualSummary: "put", ClosestMock: "mock-get", Earlier: "mock-auth"},
	}}}}
	plan := &exactPlan{owned: map[string][]string{"test-3": {"mock-auth"}, "test-1": {"mock-auth"}, "test-2": {"mock-get"}, "test-4": {"mock-get"}}}
	named := markRecordedIn(results, plan.ownersOf())
	calls := results[0].FailureInfo.UnmatchedCalls
	if calls[0].RecordedIn != "test-1" || calls[1].RecordedIn != "" || calls[2].RecordedIn != "" || calls[3].RecordedIn != "" || calls[4].RecordedIn != "" || len(named) != 1 {
		t.Fatalf("recorded_in %q %q %q %q %q, named %v", calls[0].RecordedIn, calls[1].RecordedIn, calls[2].RecordedIn, calls[3].RecordedIn, calls[4].RecordedIn, named)
	}
	// A changed call that names an earlier recording of its method and path
	// names that recording's first owner (models.UnmatchedCall.NameEarlierIn).
	if calls[4].EarlierIn != "test-1" || calls[3].EarlierIn != "" {
		t.Fatalf("earlier_in %q %q; want test-1 for the call naming mock-auth as earlier, none for the near one", calls[4].EarlierIn, calls[3].EarlierIn)
	}
}

// The set's worker scope is the set's to make (D3): a set whose application
// made its boot call, the worker scope's, in a test passes; one whose
// application never made it fails, though every test passed, naming the call.
func TestUnderTheSwitchTheSetFailsWhenItsWorkerScopeIsNotMade(t *testing.T) {
	for _, tc := range []struct {
		name string
		made map[string][]models.MockState
		want models.TestSetStatus
	}{
		{"the boot call made in a test", map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010), mongoAt("mock-boot", 500)}}, models.TestSetStatusPassed},
		{"the boot call never made", map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}}, models.TestSetStatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
			mappingFile(t, h, nil)
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, tc.made)}
			if got := h.run(t); got != tc.want {
				t.Fatalf("set %s, want %s", got, tc.want)
			}
			if got := statusOf(t, h, "test-1"); got != models.TestStatusPassed {
				t.Fatalf("test-1: %s, want PASSED: the worker scope is no test's", got)
			}
			named := len(logs.FilterMessageSnippet("never made 1 of the calls the test set's worker scope recorded").All())
			if want := map[models.TestSetStatus]int{models.TestSetStatusPassed: 0, models.TestSetStatusFailed: 1}[tc.want]; named != want {
				t.Fatalf("named the missing worker call %d time(s), want %d", named, want)
			}
		})
	}
}

// workerNamed is how many times the set's end named a worker-scope call its
// application never made.
func workerNamed(logs *observer.ObservedLogs) int {
	return len(logs.FilterMessageSnippet("of the calls the test set's worker scope recorded").All())
}

// The application's boot call, the worker scope's at the set's start, made as
// it starts, before the first test: the set's setup drain reads it, and the
// set's end counts it. It was re-armed as a readiness probe's consume is,
// withheld from what the set consumed, and the set failed for a call it made.
func TestUnderTheSwitchABootCallMadeBeforeTheFirstTestIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name string
		boot models.MockState
	}{
		{"mongo", mongoAt("mock-boot", 500)},
		{"http stated per-test", models.MockState{Name: "mock-boot", Kind: models.HTTP, Usage: models.Updated, Lifetime: models.LifetimePerTest,
			ReqTimestampMock: models.FormatMockTimestamp(attrAt(500)), ResTimestampMock: models.FormatMockTimestamp(attrAt(501))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
			mappingFile(t, h, nil)
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
				"":       {tc.boot},
				"test-1": {mongoAt("mock-a", 1010)},
			})}
			if got := h.run(t); got != models.TestSetStatusPassed {
				t.Fatalf("the boot call was made before the first test, yet the set is %s (named missing %d time(s))", got, workerNamed(logs))
			}
		})
	}
}

// The worker scope's calls at the set's start are its application's boot:
// required only when keploy started the application for the set. One kept
// alive from an earlier set (--keep-app-alive), or one keploy did not start,
// booted before the set's run, and its boot calls are not required of it.
func TestUnderTheSwitchABootCallIsNotRequiredOfAnAppKeployDidNotStart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		serve   bool
	}{
		{"kept alive from an earlier set", "./app", true},
		{"started outside keploy", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.replayer.config.Command = tc.command
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
			mappingFile(t, h, nil)
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}
			run := h.run
			if tc.serve {
				run = h.runServing
			}
			if got := run(t); got != models.TestSetStatusPassed || workerNamed(logs) != 0 {
				t.Fatalf("set %s, missing named %d time(s); want PASSED: the boot was not this set's", got, workerNamed(logs))
			}
		})
	}
}

// A call the application makes as keploy stops it (a deregistration, a
// flush on its way out) is read with what the app consumed as it stopped, and
// the worker scope is judged with it. One the recording made as keploy record
// stopped its application (MockEntry.AtStop) is not waited for before the
// stop, and is required once the application is stopped: never made, the set
// fails, naming it. A set with no mappings.yaml (its windows give its worker
// scope) does not say which calls the stop made: one recorded after its last
// test ended is not waited for either, so it replays as fast as with
// mappings.yaml, and is required all the same: never made, the set fails,
// saying why.
// With background serving enabled or not: the recorded set's mappings.yaml
// carries recordedWith: s12-routing, so with it enabled the set is served by
// the background rules, which wait for and require none of its worker scope
// (a call never made is said, not required). A set with no mappings.yaml is
// never served so: enabled or not, it is judged as above.
func TestUnderTheSwitchAShutdownCallIsJudgedOnceTheAppIsStopped(t *testing.T) {
	for _, s12 := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			recorded bool // mappings.yaml's worker scope says the call is the shutdown's
			made     bool
			want     models.TestSetStatus
		}{
			{"made as it stops, the scope derived", false, true, models.TestSetStatusPassed},
			{"made as it stops, recorded at the stop", true, true, models.TestSetStatusPassed},
			{"never made, recorded at the stop", true, false, models.TestSetStatusFailed},
			{"never made, the scope derived", false, false, models.TestSetStatusFailed},
		} {
			t.Run(fmt.Sprintf("background serving %v, %s", s12, tc.name), func(t *testing.T) {
				setSwitch(t, true)
				enableS12(t, &s12)
				fastWorkerWait(t)
				waits := 0
				wake := workerWake
				workerWake = func(ctx context.Context) (<-chan time.Time, func()) {
					waits++
					return wake(ctx)
				}
				h := newPartialRunHarness(t, 1, 0)
				pinWindows(h)
				h.replayer.config.Command = "./app"
				stopped := make(chan struct{})
				h.instr.stopped = stopped
				core, logs := observer.New(zap.InfoLevel)
				h.replayer.logger = zap.New(core)
				h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-deregister", 3000)}
				db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a")})
				if tc.recorded {
					if err := db.UpsertWorker(context.Background(), "test-set-0", []models.MockEntry{
						{Name: "mock-deregister", Kind: string(models.HTTP), Place: "test-1", AtStop: true},
					}); err != nil {
						t.Fatal(err)
					}
					if err := db.MarkRecordedWith(context.Background(), "test-set-0", models.RecordedWithS12Routing); err != nil {
						t.Fatal(err)
					}
				} else {
					// No mappings.yaml: the replay derives the worker scope
					// from the windows, which do not say what was the
					// shutdown's.
					if err := db.Delete(context.Background(), "test-set-0"); err != nil {
						t.Fatal(err)
					}
				}
				want := tc.want
				background := s12 && tc.recorded
				if background {
					want = models.TestSetStatusPassed
				}
				inner := consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})
				drained := false
				h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
					select {
					case <-stopped:
						if tc.made && !drained {
							drained = true
							return []models.MockState{mongoAt("mock-deregister", 3000)}
						}
						return nil
					default:
						return inner()
					}
				}}
				if got := h.run(t); got != want {
					t.Fatalf("set %s, want %s (missing named %d time(s))", got, want, workerNamed(logs))
				}
				if h.replayer.exact == nil || h.replayer.exact.background != background {
					t.Fatalf("the set was served by the background rules: %v; want %v", h.replayer.exact != nil && h.replayer.exact.background, background)
				}
				if want := map[models.TestSetStatus]int{models.TestSetStatusPassed: 0, models.TestSetStatusFailed: 1}[want]; workerNamed(logs) != want {
					t.Fatalf("missing named %d time(s), want %d", workerNamed(logs), want)
				}
				if said, want := len(logs.FilterMessageSnippet("background recordings of this test set that its end had not read answered").All()), map[bool]int{true: 1}[background && !tc.made]; said != want {
					t.Fatalf("the set said %d time(s) a background call went unread, want %d", said, want)
				}
				if waited := len(logs.FilterMessageSnippet("waiting for the test set's worker scope").All()); waited != 0 || waits != 0 {
					t.Fatalf("the set's end waited (%d line(s), %d wait(s)) for a call the application makes only as it is stopped", waited, waits)
				}
				said := logs.FilterMessageSnippet("without waiting for them").All()
				if want := map[bool]int{true: 1}[!tc.recorded && !tc.made]; len(said) != want {
					t.Fatalf("the failure said %d time(s) the set has no mappings.yaml to tell the stop's calls apart, want %d", len(said), want)
				}
				for _, l := range logs.FilterMessageSnippet("test-set failed: the application never made").All() {
					next, _ := l.ContextMap()["next_step"].(string)
					if derived := !tc.recorded; derived != strings.Contains(next, "marks the calls the application makes as it is stopped") {
						t.Fatalf("next_step %q; the scope derived %v", next, derived)
					}
				}
			})
		}
	}
}

// With the switch off nothing of that applies: a set with no mappings.yaml
// requires none of the calls its recording made after its last test (they
// are served, not required), and its end waits for none of them. Made or
// never made, the set passes, naming none.
func TestWithTheSwitchOffACallAfterTheLastTestIsServedNotRequired(t *testing.T) {
	for _, made := range []bool{false, true} {
		t.Run(fmt.Sprintf("made %v", made), func(t *testing.T) {
			setSwitch(t, false)
			fastWorkerWait(t)
			waits := 0
			wake := workerWake
			workerWake = func(ctx context.Context) (<-chan time.Time, func()) {
				waits++
				return wake(ctx)
			}
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			stopped := make(chan struct{})
			h.instr.stopped = stopped
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-deregister", 3000)}
			db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a")})
			if err := db.Delete(context.Background(), "test-set-0"); err != nil {
				t.Fatal(err)
			}
			inner := consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})
			drained := false
			h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
				select {
				case <-stopped:
					if made && !drained {
						drained = true
						return []models.MockState{mongoAt("mock-deregister", 3000)}
					}
					return nil
				default:
					return inner()
				}
			}}
			if got := h.run(t); got != models.TestSetStatusPassed {
				t.Fatalf("set %s, want PASSED (missing named %d time(s))", got, workerNamed(logs))
			}
			if n := workerNamed(logs); n != 0 {
				t.Fatalf("missing named %d time(s), want none", n)
			}
			if waited := len(logs.FilterMessageSnippet("waiting for the test set's worker scope").All()); waited != 0 || waits != 0 {
				t.Fatalf("the set's end waited (%d line(s), %d wait(s))", waited, waits)
			}
			if said := len(logs.FilterMessageSnippet("without waiting for them").All()); said != 0 {
				t.Fatalf("the set said %d time(s) it required a call after its last test, want none", said)
			}
		})
	}
}

// A set with no mappings.yaml whose application keploy does not stop (one
// kept for the next set): a call recorded after its last test ended is
// judged as one keploy record marked at its stop is. The set's end waits for
// it no more than it does once keploy stops the application, does not
// require it, made after the last test or never, and says which went
// unjudged and why, where a set whose mappings.yaml marks the call says
// nothing. A call the set's end read (one the application made before its
// last test's own read) is not unjudged. Once keploy stops the application,
// what it consumed as it stopped holds the call, and nothing goes unjudged.
func TestUnderTheSwitchACallAfterTheLastTestOfAnAppKeployDidNotStopGoesUnjudged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recorded bool // mappings.yaml's worker scope says the call is the shutdown's
		serve    bool // keploy keeps the application for the next set
		late     bool // the application makes the call at the read after its last test's own
		early    bool // the application makes the call before its last test's own read, which holds it
		unjudged bool
	}{
		{"kept alive, made after the last test, the scope derived", false, true, true, false, true},
		{"kept alive, never made, the scope derived", false, true, false, false, true},
		{"kept alive, read with the last test's calls, the scope derived", false, true, false, true, false},
		{"kept alive, never made, recorded at the stop", true, true, false, false, false},
		{"stopped by keploy, made as it stops, the scope derived", false, false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, true)
			fastWorkerWait(t)
			waits := 0
			wake := workerWake
			workerWake = func(ctx context.Context) (<-chan time.Time, func()) {
				waits++
				return wake(ctx)
			}
			h := newPartialRunHarness(t, 1, 0)
			pinWindows(h)
			h.replayer.config.Command = "./app"
			stopped := make(chan struct{})
			h.instr.stopped = stopped
			core, logs := observer.New(zap.InfoLevel)
			h.replayer.logger = zap.New(core)
			h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-late", 1500)}
			db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a")})
			if tc.recorded {
				if err := db.UpsertWorker(context.Background(), "test-set-0", []models.MockEntry{
					{Name: "mock-late", Kind: string(models.HTTP), Place: "test-1", AtStop: true},
				}); err != nil {
					t.Fatal(err)
				}
			} else if err := db.Delete(context.Background(), "test-set-0"); err != nil {
				t.Fatal(err)
			}
			inner := consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})
			var mu sync.Mutex
			reads, drained := 0, false
			h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
				select {
				case <-stopped:
					mu.Lock()
					defer mu.Unlock()
					if !tc.serve && !drained {
						drained = true
						return []models.MockState{mongoAt("mock-late", 1500)}
					}
					return nil
				default:
				}
				got := inner()
				if len(got) == 0 {
					return nil
				}
				mu.Lock()
				defer mu.Unlock()
				if reads++; reads > 1 && tc.late {
					return []models.MockState{mongoAt("mock-late", 1500)}
				}
				if reads == 1 && tc.early {
					return append(append([]models.MockState(nil), got...), mongoAt("mock-late", 1500))
				}
				return got
			}}
			run := h.run
			if tc.serve {
				run = h.runServing
			}
			if got := run(t); got != models.TestSetStatusPassed {
				t.Fatalf("set %s, want PASSED (missing named %d time(s))", got, workerNamed(logs))
			}
			if n := workerNamed(logs); n != 0 {
				t.Fatalf("missing named %d time(s), want none", n)
			}
			if waited := len(logs.FilterMessageSnippet("waiting for the test set's worker scope").All()); waited != 0 || waits != 0 {
				t.Fatalf("the set's end waited (%d line(s), %d wait(s)) for a call marked as its stop's", waited, waits)
			}
			said := logs.FilterMessageSnippet("went unjudged").All()
			if want := map[bool]int{true: 1}[tc.unjudged]; len(said) != want {
				t.Fatalf("the set said %d time(s) which calls went unjudged, want %d", len(said), want)
			}
			if !tc.unjudged {
				return
			}
			fields := said[0].ContextMap()
			mocks, _ := fields["mocks"].([]interface{})
			next, _ := fields["next_step"].(string)
			if len(mocks) != 1 || mocks[0] != "mock-late" || fields["count"] != int64(1) || !strings.Contains(next, "without --sync and with mapping on") {
				t.Fatalf("the line said %v; want mock-late named once, with the recording that marks the stop's calls as the next step", fields)
			}
		})
	}
}

// A set with no mappings.yaml whose worker call between two tests is never
// made fails, naming it, as one with mappings.yaml does: only the calls
// recorded after its last test ended are marked as ones the stop may make,
// so neither its reason nor its next step speaks of the stop's calls (the
// next step is to record again if the application no longer makes the call
// on its own). A call after the last test that the application makes as it
// is stopped is read then, and changes none of that.
func TestUnderTheSwitchAWorkerCallBetweenTestsOfASetWithNoMappingsIsNotTheStops(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Command = "./app"
	stopped := make(chan struct{})
	h.instr.stopped = stopped
	core, logs := observer.New(zap.InfoLevel)
	h.replayer.logger = zap.New(core)
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), httpAt("mock-between", 1500), httpAt("mock-b", 2010), httpAt("mock-late", 2500)}
	db := mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a"), "test-2": mongoEntries("mock-b")})
	if err := db.Delete(context.Background(), "test-set-0"); err != nil {
		t.Fatal(err)
	}
	inner := consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-b", 2010)}})
	drained := false
	h.replayer.hookImpl = prHooks{consumedFor: func() []models.MockState {
		select {
		case <-stopped:
			if !drained {
				drained = true
				return []models.MockState{mongoAt("mock-late", 2500)}
			}
			return nil
		default:
			return inner()
		}
	}}
	if got := h.run(t); got != models.TestSetStatusFailed {
		t.Fatalf("set %s, want FAILED: the worker call between the tests was never made", got)
	}
	failed := logs.FilterMessageSnippet("test-set failed: the application never made").All()
	if len(failed) != 1 {
		t.Fatalf("the set's end said %d time(s) which calls were never made, want once", len(failed))
	}
	fields := failed[0].ContextMap()
	mocks, _ := fields["mocks"].([]interface{})
	next, _ := fields["next_step"].(string)
	if len(mocks) != 1 || mocks[0] != "mock-between" {
		t.Fatalf("the set's end named %v; want [mock-between]", fields["mocks"])
	}
	if strings.Contains(failed[0].Message, "recorded after the set's last test ended") {
		t.Fatalf("the reason speaks of the calls after the last test: %q", failed[0].Message)
	}
	if !strings.HasPrefix(next, "if the application no longer makes these calls on its own") {
		t.Fatalf("next_step %q; want the one for a call the application stopped making on its own", next)
	}
	if n := len(logs.FilterMessageSnippet("went unjudged").All()); n != 0 {
		t.Fatalf("the set said %d time(s) a call went unjudged, though keploy stopped the application", n)
	}
}

// failingWorkerReads fails every read of what the agent consumed once the
// set's end has begun waiting for the worker scope (flipped by a log hook).
type failingWorkerReads struct {
	prHooks
	failing *atomic.Bool
}

func (f failingWorkerReads) GetConsumedMocks(ctx context.Context) ([]models.MockState, error) {
	if f.failing.Load() {
		return nil, errors.New("the agent did not answer")
	}
	return f.prHooks.GetConsumedMocks(ctx)
}

// A read of what the worker scope answered that fails is keploy's failure,
// never the application's: the set is not judged, and no call is named as
// never made.
func TestUnderTheSwitchAFailedReadOfTheWorkerScopeLeavesTheSetUnjudged(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 1, 0)
	pinWindows(h)
	h.replayer.config.Command = "./app"
	failing := &atomic.Bool{}
	core, logs := observer.New(zap.InfoLevel)
	h.replayer.logger = zap.New(core, zap.Hooks(func(e zapcore.Entry) error {
		if strings.Contains(e.Message, "waiting for the test set's worker scope") {
			failing.Store(true)
		}
		return nil
	}))
	h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
	mappingFile(t, h, nil)
	h.replayer.hookImpl = failingWorkerReads{prHooks: prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}, failing: failing}
	if got := h.run(t); got != models.TestSetStatusInternalErr {
		t.Fatalf("set %s, want INTERNAL_ERR: keploy could not read what the worker scope answered", got)
	}
	if !h.replayer.notJudged("test-set-0") || workerNamed(logs) != 0 {
		t.Fatalf("not judged %v, missing named %d time(s); want not judged and nothing named", h.replayer.notJudged("test-set-0"), workerNamed(logs))
	}
}

// A set that already failed is red whatever its worker scope does: its end
// does not wait for the worker scope's calls, nor names one as never made.
// Only a set that passed so far waits for them.
func TestUnderTheSwitchAFailedSetDoesNotWaitForItsWorkerScope(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 1, 0)
	pinWindows(h)
	h.replayer.config.Command = "./app"
	core, logs := observer.New(zap.InfoLevel)
	h.replayer.logger = zap.New(core)
	h.mocks.filtered = []*models.Mock{httpAt("mock-boot", 500), httpAt("mock-a", 1010)}
	mappingFile(t, h, nil)
	// test-1 never makes its call: it fails, and with it the set.
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{})}
	if got := h.run(t); got != models.TestSetStatusFailed {
		t.Fatalf("set %s, want FAILED: test-1 never made its call", got)
	}
	if got := statusOf(t, h, "test-1"); got != models.TestStatusFailed {
		t.Fatalf("test-1: %s, want FAILED", got)
	}
	if waited := len(logs.FilterMessageSnippet("waiting for the test set's worker scope").All()); waited != 0 || workerNamed(logs) != 0 {
		t.Fatalf("a failed set waited for its worker scope %d time(s) and named a call missing %d time(s); want neither", waited, workerNamed(logs))
	}
}
