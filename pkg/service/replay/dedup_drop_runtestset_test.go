package replay

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// dropRecMockDB is the harness's mock store with DropMocks: it records each
// call and reports every named mock deleted.
type dropRecMockDB struct {
	*prMockDB
	mu    sync.Mutex
	calls [][]string
}

func (m *dropRecMockDB) DropMocks(_ context.Context, _ string, names []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, append([]string(nil), names...))
	return names, nil
}

func (m *dropRecMockDB) dropCalls() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// dupRunHooks names duplicates (DuplicateNamer), answers each test's
// consumption by its window, and fails the read of failRead's consumption.
type dupRunHooks struct {
	prHooks
	dups     []string
	failRead string
	h        *prRun
	byTest   map[string][]models.MockState
}

func (d dupRunHooks) GetDuplicateTestCaseNames(string) []string { return d.dups }

func (d dupRunHooks) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	d.h.instr.mu.Lock()
	after := d.h.instr.windowAfter
	d.h.instr.mu.Unlock()
	for _, tc := range d.h.cases {
		if tc.HTTPReq.Timestamp.Equal(after) {
			if tc.Name == d.failRead {
				return nil, errors.New("the agent did not answer the consumption read")
			}
			return d.byTest[tc.Name], nil
		}
	}
	return nil, nil
}

// A whole run with dynamic dedup's duplicates (k8s-proxy auto-replay's
// flags): the prune deletes the mocks only the duplicate consumed, its
// mapping line goes, and the mapping write does not put it back. Nothing is
// dropped, and the line stays, when a test got no answer, when what a test
// consumed could not be read, under KEPLOY_PROVEN, or with hooks that name no
// duplicates.
func TestRunTestSetDropsTheDuplicatesMocksAndLine(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noNamer  bool
		noAnswer string
		failRead string
		proven   bool
		wantDrop bool
	}{
		{name: "duplicates dropped", wantDrop: true},
		{name: "a test got no answer", noAnswer: "test-3"},
		{name: "a consumption read failed", failRead: "test-3"},
		{name: "KEPLOY_PROVEN", proven: true},
		{name: "hooks name no duplicates", noNamer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setSwitch(t, tc.proven)
			h := newMappingRunHarness(t, 3)
			pinWindows(h)
			h.replayer.config.Test.UpdateTestMapping = true
			h.replayer.config.Test.RemoveUnusedMocks = true
			mocks := &dropRecMockDB{prMockDB: h.mocks}
			h.replayer.mockDB = mocks
			db := mappingFile(t, h, map[string][]models.MockEntry{
				"test-1": mongoEntries("mock-a"), "test-2": mongoEntries("mock-b"), "test-3": mongoEntries("mock-c"),
			})
			byTest := map[string][]models.MockState{
				"test-1": {mongoAt("mock-a", 1010)}, "test-2": {mongoAt("mock-b", 2010)}, "test-3": {mongoAt("mock-c", 3010)},
			}
			base := prHooks{}
			if tc.noAnswer != "" {
				base.simErr = map[string]error{tc.noAnswer: prClientTimeoutErr(t)}
			}
			if tc.noNamer {
				base.consumedFor = consumedByWindow(h, byTest)
				h.replayer.hookImpl = base
			} else {
				h.replayer.hookImpl = dupRunHooks{prHooks: base, dups: []string{"test-2"}, failRead: tc.failRead, h: h, byTest: byTest}
			}
			_ = h.run(t)

			calls := mocks.dropCalls()
			lines := mappedOnDisk(t, db)
			if tc.wantDrop {
				if !reflect.DeepEqual(calls, [][]string{{"mock-b"}}) {
					t.Fatalf("DropMocks calls %v; want one, for mock-b, the duplicate's own mock", calls)
				}
				if _, ok := lines["test-2"]; ok || len(lines["test-1"]) == 0 || len(lines["test-3"]) == 0 {
					t.Fatalf("mappings.yaml %v; want test-2's line gone (and not written back) and the kept tests' lines there", lines)
				}
				return
			}
			if len(calls) != 0 {
				t.Fatalf("DropMocks called %v; want no call", calls)
			}
			if !reflect.DeepEqual(lines["test-2"], []string{"mock-b"}) {
				t.Fatalf("test-2's line is %v; want it kept as recorded", lines["test-2"])
			}
		})
	}
}
