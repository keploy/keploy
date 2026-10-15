package replay

import (
	"fmt"
	"reflect"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Under the proven switch a replay never writes mappings.yaml, whatever its
// tests consumed: only record writes it (design v14 §5.2, U4; S12). Each case
// also runs with the switch off, where the run creates the file it always
// has: for a test that did not consume its recorded call, that file leaves
// it out, and under the proven model it would then own none of its calls.

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

func TestRunTestSetUnderTheProvenSwitchCreatesNoMappings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		consumed map[string][]models.MockState
		update   bool
		// streamed names the tests replayed in the streaming pass.
		streamed []string
		// ignored names the tests the run skips.
		ignored []string
		// wantOff is the file the run creates with the switch off (with it
		// on no file is created).
		wantOff map[string][]string
	}{
		{
			name: "every test consumed the mocks of its window: the control",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-2", 2010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
		},
		{
			// test-2's identical call was answered by test-1's mock, which a
			// replay without mappings may serve it.
			name: "test-2's call was answered by test-1's mock",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-1", 1010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}},
		},
		{
			name: "test-2 no longer makes its call",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}},
		},
		{
			name:     "a streamed test consumed the mocks of its window: the control",
			streamed: []string{"test-2"},
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-2", 2010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}},
		},
		{
			name:     "a streamed test no longer makes its call",
			streamed: []string{"test-2"},
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
			},
			wantOff: map[string][]string{"test-1": {"mock-1"}},
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
		},
		{
			name: "--update-test-mapping creates no file either",
			consumed: map[string][]models.MockState{
				"test-1": {mongoAt("mock-1", 1010)},
				"test-2": {mongoAt("mock-1", 1010)},
			},
			update:  true,
			wantOff: map[string][]string{"test-1": {"mock-1"}},
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
					want = nil
				}
				if got := mappedOnDisk(t, db); !reflect.DeepEqual(got, want) {
					t.Fatalf("mappings.yaml maps %v; want %v (nil: no file)", got, want)
				}
				// Not writing it is the switch's rule, not a hold: no warning.
				if held := logs.FilterMessage("not writing mappings.yaml").All(); len(held) != 0 {
					t.Fatalf("want no warning, got %+v", held)
				}
			})
		}
	}
}

// A set keploy record mapped keeps its file as record wrote it under the
// switch, --update-test-mapping or not: the replay writes nothing, whatever
// its tests consumed (test-2 left its recorded call unconsumed and fails for
// it; test-1 consumed a mock more than it owns).
func TestRunTestSetUnderTheProvenSwitchARecordedMappingStaysAsRecordWroteIt(t *testing.T) {
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
	want := map[string][]string{"test-1": {"mock-1"}, "test-2": {"mock-2"}}
	if got := mappedOnDisk(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("mappings.yaml maps %v; want %v", got, want)
	}
}
