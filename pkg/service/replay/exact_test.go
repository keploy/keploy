package replay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
			h.instr.mu.Unlock()
			if !on {
				for _, p := range params {
					if p.Exact != nil {
						t.Fatalf("switch off: a call asked for exact serving: %+v", p)
					}
				}
				return
			}
			var got []models.ExactServing
			for _, p := range params {
				if p.Exact == nil {
					t.Fatalf("under the switch a call was not exact serving's: %+v", p)
				}
				got = append(got, *p.Exact)
			}
			want := []models.ExactServing{
				{Stage: true, Pool: []string{"mock-boot"}},
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

// A test case owns, of its mapping, a call in flight when it began only as
// one recorded before its window (models.DeriveWindowMappings gives a test
// nothing else from before it): noteAhead lists those, of the mocks the
// agent holds, for the pool call before it to name.
func TestNoteAheadListsATestCasesMappedCallsInFlight(t *testing.T) {
	at := func(ms int) time.Time { return attrAt(ms) }
	entry := func(name string, ms int) models.MockEntry {
		return models.MockEntry{Name: name, Kind: string(models.HTTP), ReqTimestampMock: models.FormatMockTimestamp(at(ms))}
	}
	mappings := map[string][]models.MockEntry{
		"test-1": {entry("own-1", 1050)},
		"test-2": {entry("fetch", 1500), entry("own-2", 2050), entry("gone", 1600), {Name: "untimed", Kind: string(models.HTTP)}},
	}
	held := []*models.Mock{{Name: "own-1", Kind: models.HTTP}, {Name: "fetch", Kind: models.HTTP}, {Name: "own-2", Kind: models.HTTP}, {Name: "untimed", Kind: models.HTTP}}
	cases := []*models.TestCase{
		{Name: "test-1", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at(1000)}, HTTPResp: models.HTTPResp{Timestamp: at(1100)}},
		{Name: "test-2", Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: at(2000)}, HTTPResp: models.HTTPResp{Timestamp: at(2100)}},
	}
	p := newExactPlan(mappings, held)
	p.noteAhead(mappings, cases)
	if got := p.ahead["test-2"]; len(got) != 1 || got[0] != "fetch" {
		t.Fatalf("test-2's calls in flight = %v; want [fetch] (own-2 is in its window, gone is not held, untimed has no time)", got)
	}
	if got := p.ahead["test-1"]; len(got) != 0 {
		t.Fatalf("test-1's calls in flight = %v; want none", got)
	}
}

// The pool call after a test case names the next one the loop opens: not one
// the selection leaves out, nor an ignored or a refused one; after the loop's
// last, the first deferred streaming test case.
func TestNextExactTestIsTheNextOneTheLoopOpens(t *testing.T) {
	tc := func(name string) *models.TestCase { return &models.TestCase{Name: name} }
	rest := []*models.TestCase{tc("a"), tc("b"), tc("c"), tc("d")}
	selected := map[string]bool{"b": true, "c": true, "d": true}
	ignored := map[string]bool{"b": true}
	refused := map[string][]string{"c": {"a placeholder refused"}}
	if got := nextExactTest(rest, selected, ignored, refused); got != "d" {
		t.Fatalf("next = %q; want d", got)
	}
	// After the loop's last served test case what opens next is not known
	// yet (a retry cycle's first, the streaming pass's): serveExactAhead
	// names it once it is.
	if got := nextExactTest(rest[:3], selected, ignored, refused); got != "" {
		t.Fatalf("next after the loop's last = %q; want none", got)
	}
	if got := nextExactTest(nil, nil, nil, refused); got != "" {
		t.Fatalf("next of nothing = %q", got)
	}
}
