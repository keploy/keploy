package replay

import (
	"reflect"
	"strings"
	"testing"

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
