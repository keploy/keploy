package proxy

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keyedSet stages n recordings in one test under exact serving, each filed by
// keys under its own name and under "even" or "odd", and returns the manager,
// the set, and a MockIndex that counts its Keys calls.
func keyedSet(t *testing.T, n int) (*MockManager, []*models.Mock, *integrations.MockIndex, *int) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	now := time.Now()
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		mocks[i] = r3Mock(fmt.Sprintf("mock-%d", i), now.Add(time.Duration(i)*time.Millisecond), "GET", fmt.Sprintf("/jobs/%d", i), "", fmt.Sprintf(`{"job":%d}`, i))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, mocks, models.BaseTime, now.Add(time.Hour))
	mm.ServeExactTest("T", mocks)
	calls := new(int)
	ix := &integrations.MockIndex{Keys: func(mk *models.Mock) []string {
		*calls++
		var i int
		if _, err := fmt.Sscanf(strings.TrimPrefix(mk.Name, "mock-"), "%d", &i); err != nil {
			t.Errorf("the index was asked for the keys of %q, which keyedSet did not make: %v", mk.Name, err)
		}
		parity := "even"
		if i%2 == 1 {
			parity = "odd"
		}
		// The parity twice: a mock is still visited once under it.
		return []string{mk.Name, parity, parity}
	}}
	return mm, mocks, ix, calls
}

func keyedWalk(t *testing.T, mm *MockManager, ix *integrations.MockIndex, key string) []string {
	t.Helper()
	var names []string
	if err := mm.RangeSessionMocksWithKey(ix, key, func(mk *models.Mock) bool {
		names = append(names, mk.Name)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return names
}

// TestAKeyedReadKeysTheOpenTestsMocksOncePerView: a keyed read of the
// session tier under exact serving (the MySQL matcher's PREPARE and CLOSE
// read) computes the keys of the open test's mocks once per view, never on a
// call. Counted, not timed: on a view of 300 mocks, 40 keyed reads after the
// first call Keys no more times than the first read did (main called it once
// per unanswered mock on every read: 300 x 40), and a consume between reads
// still drops the answered mock from them.
func TestAKeyedReadKeysTheOpenTestsMocksOncePerView(t *testing.T) {
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(false) // the verify mode recomputes every key: counted alone below
	const n = 300
	mm, _, ix, calls := keyedSet(t, n)

	if got := keyedWalk(t, mm, ix, "mock-7"); !slices.Equal(got, []string{"mock-7"}) {
		t.Fatalf("precondition: the walk under mock-7 gives %v", got)
	}
	first := *calls
	for r := 0; r < 40; r++ {
		key := fmt.Sprintf("mock-%d", r)
		if got := keyedWalk(t, mm, ix, key); !slices.Equal(got, []string{key}) {
			t.Fatalf("read %d: the walk under %s gives %v", r, key, got)
		}
	}
	if extra := *calls - first; extra != 0 {
		t.Fatalf("40 keyed reads after the first computed %d more keys; want 0 (the view keeps its mocks' keys)", extra)
	}
	odd := keyedWalk(t, mm, ix, "odd")
	if len(odd) != n/2 || odd[0] != "mock-1" || odd[len(odd)-1] != fmt.Sprintf("mock-%d", n-1) {
		t.Fatalf("the walk under odd gives %d mocks (%v...); want each odd one once, in recorded order", len(odd), odd[:min(3, len(odd))])
	}

	// A consume between reads: the answered mock is gone from the next
	// read, and no key is computed for it.
	v := mm.exactView()
	if verdict := mm.exactConsume(v.owned[v.index["mock-7"]], true); verdict != exactServed {
		t.Fatalf("precondition: consuming mock-7 gives %v", verdict)
	}
	before := *calls
	if got := keyedWalk(t, mm, ix, "mock-7"); len(got) != 0 {
		t.Fatalf("a keyed read after mock-7 answered still gives %v", got)
	}
	if got := keyedWalk(t, mm, ix, "odd"); len(got) != n/2-1 || slices.Contains(got, "mock-7") {
		t.Fatalf("the walk under odd after mock-7 answered gives %d mocks; want %d without it", len(got), n/2-1)
	}
	if extra := *calls - before; extra != 0 {
		t.Fatalf("keyed reads after a consume computed %d keys; want 0", extra)
	}

	// The next view (the test begins again) keys its own copies once.
	mm.ServeExactTest("T", mm.StagedMocks())
	before = *calls
	if got := keyedWalk(t, mm, ix, "mock-7"); !slices.Equal(got, []string{"mock-7"}) {
		t.Fatalf("the test begun again does not show mock-7 under its key: %v", got)
	}
	if got := *calls - before; got > 2*n {
		t.Fatalf("the first keyed read of a new view computed %d keys; want at most one per copy and one per staged mock (%d)", got, 2*n)
	}
}

// TestTheExactViewsKeptKeysAgreeWithAKeyedReadRecomputed: random consumes
// and begins interleaved with keyed reads, under the verify mode (TestMain):
// each read the view's kept keys give equals the read recomputed from every
// copy's keys (a difference panics), and the answered mocks are never given.
func TestTheExactViewsKeptKeysAgreeWithAKeyedReadRecomputed(t *testing.T) {
	if !exactViewVerify.Load() {
		t.Fatal("precondition: the rows run with the verify mode on")
	}
	const n = 60
	mm, _, ix, _ := keyedSet(t, n)
	rng := rand.New(rand.NewSource(7))
	answered := map[string]bool{}
	for step := 0; step < 400; step++ {
		switch op := rng.Intn(10); {
		case op < 5:
			key := []string{"even", "odd", fmt.Sprintf("mock-%d", rng.Intn(n))}[rng.Intn(3)]
			for _, name := range keyedWalk(t, mm, ix, key) {
				if answered[name] {
					t.Fatalf("step %d: a keyed read under %s gives %s, which answered", step, key, name)
				}
			}
		case op < 9:
			v := mm.exactView()
			name := fmt.Sprintf("mock-%d", rng.Intn(n))
			if mm.exactConsume(v.owned[v.index[name]], rng.Intn(2) == 0) == exactServed {
				answered[name] = true
			}
		default:
			mm.ServeExactTest("T", mm.StagedMocks())
			answered = map[string]bool{}
		}
	}
}

// TestTheVerifyModeRefusesAStaleKeptKey: the verify mode is what catches a
// kept key gone stale. A view whose index files a mock under a key its copy
// is not given panics on the read; with the verify mode off, the same read
// gives the stale mock, the false answer the mode exists to stop.
func TestTheVerifyModeRefusesAStaleKeptKey(t *testing.T) {
	mm, _, ix, _ := keyedSet(t, 4)
	v := mm.exactView()
	lists := v.ownedKeys(ix)
	lists["mock-1"] = append(lists["mock-1"], v.index["mock-2"])
	func() {
		defer func() {
			if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "exact view verify") {
				t.Fatalf("a stale kept key read under the verify mode: recovered %v; want the verify mode's panic", r)
			}
		}()
		keyedWalk(t, mm, ix, "mock-1")
	}()
	defer exactViewVerify.Store(exactViewVerify.Load())
	exactViewVerify.Store(false)
	if got := keyedWalk(t, mm, ix, "mock-1"); !slices.Equal(got, []string{"mock-1", "mock-2"}) {
		t.Fatalf("precondition: the stale read gives %v", got)
	}
}
