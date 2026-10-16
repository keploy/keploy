package replay

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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
// outside every window is the pool's. Each test case is judged for what it
// owns; nothing is written to mappings.yaml.
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
			h.instr.mu.Lock()
			var pool []string
			for _, p := range h.instr.allParams {
				if p.Exact != nil && p.Exact.Stage {
					pool = p.Exact.Pool
				}
			}
			h.instr.mu.Unlock()
			if want := []string{"mock-boot"}; !reflect.DeepEqual(pool, want) {
				t.Fatalf("pool %v, want %v", pool, want)
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
	h.instr.mu.Lock()
	var pool []string
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Stage {
			pool = p.Exact.Pool
		}
	}
	h.instr.mu.Unlock()
	if want := []string{"mock-boot"}; !reflect.DeepEqual(pool, want) {
		t.Fatalf("pool %v, want %v: test-1's call is test-1's", pool, want)
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
// test owns, and one the parser only found near (not NotOwned), are not.
func TestMarkRecordedInNamesTheTestThatOwnsTheRecording(t *testing.T) {
	results := []models.TestResult{{TestCaseID: "test-2", FailureInfo: models.FailureInfo{UnmatchedCalls: []models.UnmatchedCall{
		{ActualSummary: "auth", ClosestMock: "mock-auth", NotOwned: true},
		{ActualSummary: "get", ClosestMock: "mock-get", NotOwned: true},
		{ActualSummary: "boot", ClosestMock: "mock-boot", NotOwned: true},
		{ActualSummary: "near", ClosestMock: "mock-auth"},
	}}}}
	plan := &exactPlan{owned: map[string][]string{"test-3": {"mock-auth"}, "test-1": {"mock-auth"}, "test-2": {"mock-get"}, "test-4": {"mock-get"}}}
	named := markRecordedIn(results, plan.ownersOf())
	calls := results[0].FailureInfo.UnmatchedCalls
	if calls[0].RecordedIn != "test-1" || calls[1].RecordedIn != "" || calls[2].RecordedIn != "" || calls[3].RecordedIn != "" || len(named) != 1 {
		t.Fatalf("recorded_in %q %q %q %q, named %v", calls[0].RecordedIn, calls[1].RecordedIn, calls[2].RecordedIn, calls[3].RecordedIn, named)
	}
}
