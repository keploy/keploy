package replay

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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
