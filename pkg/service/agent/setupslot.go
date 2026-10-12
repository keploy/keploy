package agent

import (
	"sort"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// SETUP SLOTS: whose turn a setup copy is.
//
// A spec's suite recording is one list holding every beforeAll and afterAll
// of the spec AND every request that fell between two test windows. At replay
// all of it is one pool, used up oldest first, so it only answers right while
// the replay makes exactly the recorded calls in the recorded order. One extra
// call anywhere takes the copy recorded for a LATER hook, and that hook then
// gets the copy after its own. Measured on filtering.spec.ts: the "status code"
// test's last click (Reset all filters) refetches the suite list ~0.3 s after
// the test ends. At record that request landed inside the next test's window;
// at replay it landed in the gap and used up -49, the copy the next describe's
// beforeAll recorded. That beforeAll then took -87 (the third describe's), and
// the third fell back to an empty list: filtering:1106 failed and the rest of
// its serial describe never ran.
//
// The recording says whose turn each copy is. Order the spec's tests by when
// their own recording started; a setup copy's SLOT is how many of them had
// started when it was recorded: 0 is the setup before the first test, k the
// hooks and gap after test k. At replay the agent knows which test ended last,
// so a setup copy from a later slot belongs to a hook that has not run yet: it
// is tagged FenceLater and the HTTP matcher never uses it up. If it is the only
// exact answer the request has, it still answers, without being used up, so
// its own hook gets it later and no new miss is created.
//
// Tests with no recording of their own take no place in the order on either
// side (record or replay), so they never shift a slot.

// placeSetup computes the spec's test order and every suite copy's slot.
// ownerOf maps each of the spec's mock names to the scope that recorded it.
func (sp *specPool) placeSetup(ownerOf map[string]string, mocks []*models.Mock) {
	start := map[string]time.Time{}
	for _, m := range mocks {
		if m == nil || m.Spec.ReqTimestampMock.IsZero() {
			continue
		}
		o := ownerOf[m.Name]
		if o == "" || isSuiteScope(o) {
			continue
		}
		if t, ok := start[o]; !ok || m.Spec.ReqTimestampMock.Before(t) {
			start[o] = m.Spec.ReqTimestampMock
		}
	}
	tests := make([]string, 0, len(start))
	for t := range start {
		tests = append(tests, t)
	}
	sort.Slice(tests, func(i, j int) bool {
		if !start[tests[i]].Equal(start[tests[j]]) {
			return start[tests[i]].Before(start[tests[j]])
		}
		return tests[i] < tests[j]
	})
	sp.index = make(map[string]int, len(tests))
	sp.starts = make([]time.Time, len(tests))
	for i, t := range tests {
		sp.index[t] = i + 1
		sp.starts[i] = start[t]
	}
	sp.setupSlot = map[string]int{}
	for _, m := range mocks {
		if m == nil || m.Spec.ReqTimestampMock.IsZero() || !isSuiteScope(ownerOf[m.Name]) {
			continue
		}
		sp.setupSlot[m.Name] = sp.slotAt(m.Spec.ReqTimestampMock)
	}
}

// slotAt returns how many of the spec's tests had started recording by ts.
func (sp *specPool) slotAt(ts time.Time) int {
	return sort.Search(len(sp.starts), func(i int) bool { return sp.starts[i].After(ts) })
}

func isSuiteScope(name string) bool {
	return len(name) > len("__suite__:") && name[:len("__suite__:")] == "__suite__:"
}

// cachedSpecPool returns a spec's plan if one was built, without building it.
func (a *Agent) cachedSpecPool(spec string) *specPool {
	a.specPoolMu.RLock()
	defer a.specPoolMu.RUnlock()
	return a.specPools[spec]
}

// setFenceSetup records that a spec's setup pool (hooks, gaps) is being staged.
func (a *Agent) setFenceSetup(spec string) {
	a.fenceMu.Lock()
	a.fenceScope, a.fenceOwn, a.fenceSetup = "", nil, spec
	a.fenceMu.Unlock()
}

// noteTestEnded moves the spec's replay position to the test that just ended.
// A test with no recording of its own has no place in the order and moves
// nothing.
func (a *Agent) noteTestEnded(name string) {
	spec := specOfScope(name)
	sp := a.cachedSpecPool(spec)
	if sp == nil {
		return
	}
	k, ok := sp.index[name]
	if !ok {
		return
	}
	a.fenceMu.Lock()
	if a.specPos == nil {
		a.specPos = map[string]int{}
	}
	a.specPos[spec] = k
	a.fenceMu.Unlock()
}

// markSetupLater tags a staged setup pool: every mock carries the setup
// period's scope (so the matcher's repeat-last works inside hooks and gaps),
// and every suite copy of the spec from a slot after pos is FenceLater.
func (a *Agent) markSetupLater(mocks []*models.Mock, spec string, pos int) {
	sp := a.cachedSpecPool(spec)
	scope := "setup:" + spec + "#" + strconv.Itoa(pos)
	later := 0
	for _, m := range mocks {
		if m == nil {
			continue
		}
		m.TestModeInfo.FenceScope = scope
		if sp == nil || m.TestModeInfo.SharedInSpec || m.Kind != models.HTTP {
			continue
		}
		if s, ok := sp.setupSlot[m.Name]; ok && s > pos {
			m.TestModeInfo.FenceLater = true
			later++
		}
	}
	a.logger.Info("KPSLOT setup pool staged",
		zap.String("spec", spec),
		zap.Int("after_test", pos),
		zap.Int("later_setup_copies", later))
}
