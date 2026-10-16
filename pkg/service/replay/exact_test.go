package replay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A test case owns what its mapping lists; every stored mock no mapping
// lists is the set's pool, once.
func TestNewExactPlan(t *testing.T) {
	m := func(names ...string) []*models.Mock {
		var out []*models.Mock
		for _, n := range names {
			out = append(out, &models.Mock{Name: n})
		}
		return out
	}
	p := newExactPlan(map[string][]models.MockEntry{"test-1": {{Name: "mock-1"}}, "test-2": {}}, m("mock-0", "mock-1"), m("mock-2", "mock-0"))
	if want := map[string][]string{"test-1": {"mock-1"}, "test-2": {}}; !reflect.DeepEqual(p.owned, want) {
		t.Fatalf("owned %v, want %v", p.owned, want)
	}
	if want := []string{"mock-0", "mock-2"}; !reflect.DeepEqual(p.pool, want) {
		t.Fatalf("pool %v, want %v", p.pool, want)
	}
}

// A test case owns the stored mocks its mapping lists; one the agent does not
// hold (a connection failure, which keploy never hands it, or one the store
// pruned) is not asked of the agent.
func TestAMockTheAgentDoesNotHoldIsNotAskedOfIt(t *testing.T) {
	p := newExactPlan(map[string][]models.MockEntry{"test-1": {{Name: "mock-1"}, {Name: "mock-cf"}, {Name: "mock-gone"}}}, []*models.Mock{{Name: "mock-1", Kind: models.HTTP}})
	if want := map[string][]string{"test-1": {"mock-1"}}; !reflect.DeepEqual(p.owned, want) || !p.ownsTestMocks {
		t.Fatalf("owned %v (test mocks %v), want %v", p.owned, p.ownsTestMocks, want)
	}
	// A mapping of handshakes and DNS lookups alone owns no test mock.
	dns := &models.Mock{Name: "mock-2", Kind: models.DNS}
	if p := newExactPlan(map[string][]models.MockEntry{"test-1": {{Name: "mock-2"}}}, []*models.Mock{dns}); p.ownsTestMocks {
		t.Fatal("a DNS lookup counted as a test mock")
	}
}

// Under the switch keploy test serves each test case exactly its own mocks:
// the set's staging call stages it and hands the agent the pool, each test
// case's call opens it with its mapping, and the call after its consumption
// is read serves the pool again. No call narrows the pool by a recorded
// window. A test case the mapping omits owns none. With the switch off no
// call asks for exact serving.
func TestUnderTheSwitchEachTestCaseIsServedExactly(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 2, 0)
			pinWindows(h)
			h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}, {Name: "mock-boot", Kind: models.HTTP}}
			mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a")})
			h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %q", status)
			}
			h.instr.mu.Lock()
			params := append([]models.MockFilterParams(nil), h.instr.allParams...)
			begins := append([]models.TestSetBegin(nil), h.instr.begins...)
			h.instr.mu.Unlock()
			if len(begins) != 1 {
				t.Fatalf("the set began %d times, want once", len(begins))
			}
			if !on {
				if begins[0].Exact != nil {
					t.Fatalf("switch off: the set's begin asked for exact serving: %+v", begins[0])
				}
				for _, p := range params {
					if p.Exact != nil {
						t.Fatalf("switch off: a call asked for exact serving: %+v", p)
					}
				}
				return
			}
			if begins[0].Exact == nil || !reflect.DeepEqual(begins[0].Exact.Pool, []string{"mock-boot"}) {
				t.Fatalf("the set began with exact serving %+v, want the pool [mock-boot]", begins[0].Exact)
			}
			var got []models.ExactServing
			for _, p := range params {
				if p.Exact == nil {
					t.Fatalf("under the switch a call was not exact serving's: %+v", p)
				}
				got = append(got, *p.Exact)
			}
			want := []models.ExactServing{
				{Test: "test-1", Owned: []string{"mock-a"}}, {},
				{Test: "test-2"}, {},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("calls %+v, want %+v", got, want)
			}
		})
	}
}

// Under the switch a mappings.yaml that cannot be read refuses the set:
// nothing is replayed from a whole pool instead. disableMapping does not
// apply: the mapping is read all the same.
func TestUnderTheSwitchAnUnreadableMappingRefusesTheSet(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.config.DisableMapping = true
	mappingFile(t, h, nil)
	dir := filepath.Join(h.replayer.config.Path, "test-set-0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mappings.yaml"), []byte("tests: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", false)
	if err == nil || status != models.TestSetStatusFailed {
		t.Fatalf("status %q, err %v; want the set refused", status, err)
	}
	h.instr.mu.Lock()
	defer h.instr.mu.Unlock()
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Test != "" {
			t.Fatalf("a test case was served from an unreadable mapping: %+v", p)
		}
	}
}

// Under the switch a test case whose mapping names a mock the set does not
// have is run, never served that mock, and failed for it as a recorded call
// it did not make; the rest of the set runs.
func TestUnderTheSwitchAMappedMockTheSetLacksFailsItsTestCase(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.mocks.filtered = []*models.Mock{{Name: "mock-a", Kind: models.HTTP}}
	mappingFile(t, h, map[string][]models.MockEntry{"test-1": mongoEntries("mock-a", "mock-9")})
	h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {mongoAt("mock-a", 1010)}})}
	if status := h.run(t); status != models.TestSetStatusFailed {
		t.Fatalf("status %q, want the set failed", status)
	}
	h.instr.mu.Lock()
	var opened []string
	for _, p := range h.instr.allParams {
		if p.Exact != nil && p.Exact.Test != "" {
			opened = append(opened, p.Exact.Test+":"+strings.Join(p.Exact.Owned, ","))
		}
	}
	h.instr.mu.Unlock()
	if want := []string{"test-1:mock-a", "test-2:"}; !reflect.DeepEqual(opened, want) {
		t.Fatalf("opened %v, want %v", opened, want)
	}
	h.report.mu.Lock()
	defer h.report.mu.Unlock()
	got := map[string]models.TestStatus{}
	for _, r := range h.report.results {
		got[r.TestCaseID] = r.Status
	}
	if got["test-1"] != models.TestStatusFailed || got["test-2"] != models.TestStatusPassed {
		t.Fatalf("results %v; want test-1 failed for mock-9 and test-2 passed", got)
	}
}

// The settings exact serving deletes are ignored under the switch, and
// work as before without it: removeUnusedMocks prunes nothing (a passing
// test leaves none of its mocks unconsumed, and the pool's are other runs'),
// and disableStatefulMocks does not turn the pool's cursors off.
func TestUnderTheSwitchTheDeletedSettingsAreIgnored(t *testing.T) {
	for _, on := range []bool{false, true} {
		t.Run(switchName(on), func(t *testing.T) {
			setSwitch(t, on)
			h := newPartialRunHarness(t, 2, 0)
			h.replayer.config.Test.RemoveUnusedMocks = true
			h.replayer.config.Test.DisableStatefulMocks = true
			if status := h.run(t); status != models.TestSetStatusPassed {
				t.Fatalf("status %q", status)
			}
			want := 1
			if on {
				want = 0
			}
			if got := h.mocks.pruneCalls(); got != want {
				t.Fatalf("pruned %d times, want %d", got, want)
			}
			if got := h.replayer.statefulMocksDisabled(); got == on {
				t.Fatalf("disableStatefulMocks read %v", got)
			}
		})
	}
}

// Under the switch a set whose test cases own nothing says so once, with the
// fix: every call a test case makes to a dependency will miss.
func TestUnderTheSwitchASetThatOwnsNothingSaysSo(t *testing.T) {
	setSwitch(t, true)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	core, logs := observer.New(zap.WarnLevel)
	h.replayer.logger = zap.New(core)
	mappingFile(t, h, nil)
	_ = h.run(t)
	said := logs.FilterMessageSnippet("no test case of this test set owns a recorded call").All()
	if len(said) != 1 || !strings.Contains(fmt.Sprint(said[0].ContextMap()["next_step"]), "keploy record") {
		t.Fatalf("want one warning with the fix, got %+v", said)
	}
}
