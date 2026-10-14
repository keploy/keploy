package replay

import (
	"fmt"
	"reflect"
	"strings"
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

func TestRunTestSetUnderTheProvenSwitchCreatesNoMappingsFromARunThatLeftAWindowMockUnconsumed(t *testing.T) {
	for _, tc := range []struct {
		name     string
		consumed map[string][]models.MockState
		update   bool
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
				// The recording: a DNS lookup and a handshake in test-1's
				// window are not test mocks, and nothing consumes them here.
				lookup := mongoMockAt("lookup", 1005)
				lookup.Kind = models.DNS
				handshake := mongoMockAt("handshake", 1006)
				handshake.Spec.Metadata["type"] = "config"
				h.mocks.filtered = []*models.Mock{lookup, handshake, mongoMockAt("mock-1", 1010), mongoMockAt("mock-2", 2010)}
				db := mappingFile(t, h, nil)
				h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, tc.consumed)}
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

// windowUse's own rule: a test that owns a mock of its recorded window and
// whose consumption the run did not read (it did not run, or the read failed)
// holds the create as surely as one that left the mock unconsumed; a test that
// owns none never does; and a set whose mocks were not read cannot show
// anything. With the switch off it holds nothing.
func TestWindowUseHoldCreate(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newPartialRunHarness(t, 3, 0)
	pinWindows(h)
	cases := h.cases
	mocks := []*models.Mock{mongoMockAt("mock-1", 1010), mongoMockAt("mock-2", 2010)}

	w := newWindowUse()
	if reason, _, _ := w.holdCreate(cases, nil); !strings.Contains(reason, "were not read") {
		t.Fatalf("a set whose mocks were not read: reason %q", reason)
	}
	w.load(cases, mocks)
	w.observe("test-1", []models.MockState{mongoAt("mock-1", 1010)})
	// test-3 owns nothing; not running it holds nothing.
	reason, left, unseen := w.holdCreate(cases, nil)
	if reason == "" || left != nil || !reflect.DeepEqual(unseen, []string{"test-2"}) {
		t.Fatalf("test-2 did not run: reason %q, left %v, unseen %v; want test-2 unseen", reason, left, unseen)
	}
	w.observe("test-2", []models.MockState{mongoAt("mock-2", 2010)})
	if reason, _, _ := w.holdCreate(cases, nil); reason != "" {
		t.Fatalf("every owned mock consumed, yet held: %q", reason)
	}
	if _, _, unseen := w.holdCreate(cases, []string{"test-2", "test-3"}); !reflect.DeepEqual(unseen, []string{"test-2"}) {
		t.Fatalf("test-2's consumption was not read: unseen %v; want [test-2]", unseen)
	}

	t.Setenv(provenswitch.Env, "")
	if off := newWindowUse(); off != nil {
		t.Fatalf("the switch is off and windowUse still reads: %+v", off)
	}
	var off *windowUse
	off.load(cases, mocks)
	off.observe("test-1", nil)
	if reason, _, _ := off.holdCreate(cases, nil); reason != "" {
		t.Fatalf("the switch is off and the create is held: %q", reason)
	}
}
