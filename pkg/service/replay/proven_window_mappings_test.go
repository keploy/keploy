package replay

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Under the proven switch a replay never creates mappings.yaml from a run in
// which a test left a mock recorded in its window unconsumed (design v14 row
// S3). Each case also runs with the switch off, where the run creates the file
// it always has: for the cases the switch holds back, that file leaves out the
// test that did not consume its recorded call, which then owns none of its
// calls under the proven model.

// mongoMockAt is a recorded per-test Mongo mock whose request went ms
// milliseconds after attr0 (pinWindows: test-i's window is [i s, i s + 100 ms]).
func mongoMockAt(name string, ms int) *models.Mock {
	return &models.Mock{
		Version: models.GetVersion(), Name: name, Kind: models.Mongo,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": "mocks"},
			ReqTimestampMock: attrAt(ms),
			ResTimestampMock: attrAt(ms + 1),
		},
	}
}

// drainedByWindow is consumedByWindow with the agent's read semantics: what a
// window consumed is handed over once, and a second read of the same window
// (the streaming pass reads before and after the stream) gets nothing more.
func drainedByWindow(h *prRun, byTest map[string][]models.MockState) func() []models.MockState {
	answer := consumedByWindow(h, byTest)
	var mu sync.Mutex
	handed := map[string]bool{}
	return func() []models.MockState {
		h.instr.mu.Lock()
		after := h.instr.lastParams.AfterTime.String()
		h.instr.mu.Unlock()
		mu.Lock()
		defer mu.Unlock()
		if handed[after] {
			return nil
		}
		handed[after] = true
		return answer()
	}
}

// carriedAt is mongoAt for a mock the agent reports carried over: consumed
// outside the window it was recorded in.
func carriedAt(name string, ms int) models.MockState {
	m := mongoAt(name, ms)
	m.CarryOver = true
	return m
}

func TestRunTestSetUnderTheProvenSwitchCreatesNoMappingsFromARunThatLeftAWindowMockUnconsumed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		consumed map[string][]models.MockState
		update   bool
		// streamed names the tests replayed in the streaming pass.
		streamed []string
		// ignored names the tests the run skips.
		ignored []string
		// wantOff is the file the run creates with the switch off; wantOn
		// with it on (nil: no file).
		wantOff, wantOn map[string][]string
		// wantLeft is the tests the hold names.
		wantLeft []string
	}{
		{
			name: "every test consumed the mocks of its window: the control",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-2", 2010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
			wantOn:  map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
		},
		{
			// test-2's identical call was answered by test-1's mock, which a
			// replay without mappings may serve it.
			name: "test-2's call was answered by test-1's mock",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-1", 1010)},
			},
			wantOff:  map[string][]string{"test-1": {"mock-1"}},
			wantLeft: []string{"test-2"},
		},
		{
			name: "test-2 no longer makes its call",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
			},
			wantOff:  map[string][]string{"test-1": {"mock-1"}},
			wantLeft: []string{"test-2"},
		},
		{
			name:     "a streamed test consumed the mocks of its window: the control",
			streamed: []string{"test-2"},
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-2", 2010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
			wantOn:  map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
		},
		{
			name:     "a streamed test no longer makes its call",
			streamed: []string{"test-2"},
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
			},
			wantOff:  map[string][]string{"test-1": {"mock-1"}},
			wantLeft: []string{"test-2"},
		},
		{
			// A carried-over mock (a pushed message) recorded in test-2's
			// window and consumed while test-1 ran is test-2's by the run's
			// attribution: test-2 did not leave it unconsumed.
			name: "a carried-over mock test-2's window recorded, consumed while test-1 ran",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010), carriedAt("mock-2", 2010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
			wantOn:  map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
		},
		{
			// An ignored test gets no entry in the file the run creates, as
			// a test that did not pass gets none: what it recorded is not
			// cut down, so it holds nothing.
			name:    "test-2 is ignored and owns a call",
			ignored: []string{"test-2"},
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}},
			wantOn:  map[string][]string{"test-1": {"mock-1"}},
		},
		{
			name: "--update-test-mapping creates no file either",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-1", 1010)},
			},
			update:   true,
			wantOff:  map[string][]string{"test-1": {"mock-1"}},
			wantLeft: []string{"test-2"},
		},
	} {
		for _, on := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/switch %v", tc.name, on), func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				core, logs := observer.New(zap.WarnLevel)
				h := newPartialRunHarness(t, 2, 0)
				pinWindows(h)
				h.replayer.logger = zap.New(core)
				h.replayer.config.Test.UpdateTestMapping = tc.update
				h.streaming(tc.streamed...)
				if tc.ignored != nil {
					h.replayer.config.Test.IgnoredTests = map[string][]string{"test-set-0": tc.ignored}
				}
				// The recording: a DNS lookup and a handshake in test-1's
				// window are not test mocks, and nothing consumes them here.
				lookup := mongoMockAt("lookup", 1005)
				lookup.Kind = models.DNS
				handshake := mongoMockAt("handshake", 1006)
				handshake.Spec.Metadata["type"] = "config"
				h.mocks.filtered = []*models.Mock{lookup, handshake, mongoMockAt("mock-1", 1010), mongoMockAt("mock-2", 2010)}
				db := mappingFile(t, h, nil)
				h.replayer.hookImpl = prHooks{consumedFor: drainedByWindow(h, tc.consumed)}
				_ = h.run(t)

				want := tc.wantOff
				if on {
					want = tc.wantOn
				}
				if got := mappedOnDisk(t, db); !reflect.DeepEqual(got, want) {
					t.Fatalf("mappings.yaml maps %v; want %v (nil: no file)", got, want)
				}
				held := logs.FilterMessage("not writing mappings.yaml").All()
				if !on || tc.wantLeft == nil {
					if len(held) != 0 {
						t.Fatalf("want no hold, got %+v", held)
					}
					return
				}
				if len(held) != 1 {
					t.Fatalf("want one \"not writing mappings.yaml\" warning, got %+v", logs.All())
				}
				ctxMap := held[0].ContextMap()
				if reason := fmt.Sprint(ctxMap["reason"]); !strings.Contains(reason, "KEPLOY_PROVEN=1") {
					t.Fatalf("the hold's reason is %q; want the proven switch's", reason)
				}
				if got := fmt.Sprint(ctxMap["testsThatLeftARecordedCallUnconsumed"]); got != fmt.Sprint(tc.wantLeft) {
					t.Fatalf("the hold names %s; want %v", got, tc.wantLeft)
				}
				if got, named := ctxMap["testsWhoseConsumptionWasNotRead"]; named {
					t.Fatalf("the hold names %v as not read; every test ran", got)
				}
			})
		}
	}
}

// The rule is about creating the file only. A set keploy record mapped under
// the switch keeps its file, written as #4773 writes it: a failed test keeps
// its recorded mapping, here test-2, which left its recorded call unconsumed
// and fails for it, and a passing test's entry is what it consumed.
func TestRunTestSetUnderTheProvenSwitchARecordedMappingStaysAsFailedTestsKeepIt(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.replayer.config.Test.UpdateTestMapping = true
	h.mocks.filtered = []*models.Mock{mongoMockAt("mock-1", 1010), mongoMockAt("mock-2", 2010), mongoMockAt("mock-3", 1020)}
	db := mappingFile(t, h, map[string][]models.MockEntry{
		"test-1": mongoEntries("mock-1"),
		"test-2": mongoEntries("mock-2"),
	})
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{
		"test-1": {mongoAt("mock-1", 1010), mongoAt("mock-3", 1020)},
		"test-2": {mongoAt("mock-1", 1010)},
	})}
	_ = h.run(t)

	if got := statusOf(t, h, "test-2"); got != models.TestStatusFailed {
		t.Fatalf("precondition: test-2 left its recorded call unconsumed and should fail under the switch, got %s", got)
	}
	want := map[string][]string{"test-1": {"mock-1", "mock-3"}, "test-2": {"mock-2"}}
	if got := mappedOnDisk(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("mappings.yaml maps %v; want %v", got, want)
	}
}

// windowUse's own rule: a test that ran, owns a mock of its recorded window,
// and whose consumption could not be read holds the create as surely as one
// whose attributed consumption lacks the mock; a carried-over mock its window
// recorded counts as consumed when the run attributes it to the test, whoever
// consumed it; a test that did not run (ignored, not selected) or owns none
// holds nothing; and a set whose mocks were not read cannot show anything.
// With the switch off it holds nothing.
func TestWindowUseHoldCreate(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0)
	pinWindows(h)
	cases := h.cases
	mocks := []*models.Mock{mongoMockAt("mock-1", 1010), mongoMockAt("mock-2", 2010)}
	attributed := func(byTest map[string][]string) *models.Mapping {
		m := &models.Mapping{}
		for _, tc := range cases {
			if names, ok := byTest[tc.Name]; ok {
				m.TestCases = append(m.TestCases, models.MappedTestCase{ID: tc.Name, Mocks: mongoEntries(names...)})
			}
		}
		return m
	}

	w := newWindowUse()
	if reason, _, _ := w.holdCreate(cases, nil, nil); !strings.Contains(reason, "did not load the set's mocks") {
		t.Fatalf("a set whose mocks were not read: reason %q", reason)
	}
	w.load(cases, mocks)
	w.ran("test-1")
	// test-2 owns mock-2 and did not run (ignored, or not selected): a file
	// this run creates gives it no entry, so it holds nothing.
	if reason, left, unseen := w.holdCreate(cases, nil, attributed(map[string][]string{"test-1": {"mock-1"}})); reason != "" {
		t.Fatalf("test-2 did not run and held the create: reason %q, left %v, unseen %v", reason, left, unseen)
	}
	w.ran("test-2")
	if _, left, _ := w.holdCreate(cases, nil, attributed(map[string][]string{"test-1": {"mock-1"}})); !reflect.DeepEqual(left, []string{"test-2"}) {
		t.Fatalf("test-2 ran and nothing is attributed to it: left %v; want [test-2]", left)
	}
	// mock-2 carried over and consumed while test-3 ran, attributed to test-2.
	if reason, _, _ := w.holdCreate(cases, nil, attributed(map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}})); reason != "" {
		t.Fatalf("every owned mock attributed to its test, yet held: %q", reason)
	}
	if _, _, unseen := w.holdCreate(cases, []string{"test-2", "test-3"}, attributed(map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}})); !reflect.DeepEqual(unseen, []string{"test-2"}) {
		t.Fatalf("test-2's consumption was not read: unseen %v; want [test-2]", unseen)
	}

	t.Setenv(provenswitch.Env, "")
	if off := newWindowUse(); off != nil {
		t.Fatalf("the switch is off and windowUse still reads: %+v", off)
	}
	var off *windowUse
	off.load(cases, mocks)
	off.ran("test-1")
	if reason, _, _ := off.holdCreate(cases, nil, nil); reason != "" {
		t.Fatalf("the switch is off and the create is held: %q", reason)
	}
}
