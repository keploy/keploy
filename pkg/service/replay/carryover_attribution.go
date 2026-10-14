package replay

import (
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// carryOverAttribution decides where a mock consumed outside its own test
// window belongs. The agent flags such a consume MockState.CarryOver: a
// registered kind's carry-over mock (a SEND the app published while handling a
// server push), or its per-test mock consumed late through MarkMockAsUsed (the
// push itself, delivered after its window). See models.RegisterCarryOver.
//
// Such a mock belongs to the test whose recorded window releases its recorded
// time (models.WindowSchedule, the same rule the agent paces with), not to the
// test that happened to be running when the application got to it. Two
// consequences, both applied only to flagged mocks, so nothing else changes:
//
//   - Prune: consumed outside its own window, it is kept whatever the running
//     test's verdict, as the startup mocks consumed before the first test are.
//     Otherwise, under PreserveFailedMocks=false, a push delivered during a
//     failing test would be deleted from the recording although the app used
//     it. One never consumed is pruned by the normal rule.
//   - Mappings: it is mapped to its own window's test, where a mapping-mode
//     replay loads it (the carry-over tier keeps it reachable after that);
//     before every window, to the startup section. The window filter that
//     drops out-of-window mocks from the running test's list would otherwise
//     leave it mapped nowhere.
type carryOverAttribution struct {
	sched *models.WindowSchedule
	// mapped holds the tests whose consumed mocks this run has already put in
	// the mapping. A mock is mapped to its own test only if that test is one of
	// them: an entry for a test this run did not map would replace that test's
	// recorded mapping with this one mock.
	mapped map[string]struct{}
	// early holds, by owning test, the flagged mocks consumed while that test
	// was not mapped yet: a carry-over SEND the app published before its own
	// test ran (the lookahead lets it land up to a few windows early). They
	// are added to the owner's entry when the owner is mapped, and to none if
	// it never is.
	early map[string][]models.MockState
}

func newCarryOverAttribution(testCases []*models.TestCase) *carryOverAttribution {
	return &carryOverAttribution{
		sched:  models.NewWindowSchedule(recordedTestWindows(testCases)),
		mapped: make(map[string]struct{}),
		early:  make(map[string][]models.MockState),
	}
}

// releaseIndex is the recorded window st belongs to (-1: before every window).
// ok is false when st is not flagged, has no usable timestamp, or there are no
// windows.
func (a *carryOverAttribution) releaseIndex(st models.MockState) (int, bool) {
	if a == nil || !st.CarryOver || a.sched.Len() == 0 || st.ReqTimestampMock == "" {
		return 0, false
	}
	t, err := models.ParseMockTimestamp(st.ReqTimestampMock)
	if err != nil || t.IsZero() {
		return 0, false
	}
	return a.sched.ReleaseIndex(t), true
}

// consumedOutsideOwnWindow reports whether the running test consumed st
// outside st's own window: flagged, and owned by another test or by the
// startup band. A flagged mock whose owner cannot be told counts as outside:
// the agent saw it consumed outside the running window, and keeping it is the
// safe side of a prune.
func (a *carryOverAttribution) consumedOutsideOwnWindow(st models.MockState, running string) bool {
	if !st.CarryOver {
		return false
	}
	idx, ok := a.releaseIndex(st)
	if !ok || idx < 0 {
		return true
	}
	return a.sched.Window(idx).TestCase != running
}

// keepWhateverTheVerdict adds to passing every mock the running test consumed
// outside that mock's own window. Call it for every test, passed or not.
func (a *carryOverAttribution) keepWhateverTheVerdict(passing map[string]models.MockState, running string, consumed []models.MockState) {
	for _, st := range consumed {
		if a.consumedOutsideOwnWindow(st, running) {
			passing[st.Name] = st
		}
	}
}

// mapConsumed records what the running test consumed: its own mocks through
// upsertActualTestMockMapping's window filter, as before, and each flagged
// mock under the test (or startup section) it belongs to, unfiltered. A
// flagged mock whose test is not mapped yet is held for that test (early) and
// added when it is mapped.
func (a *carryOverAttribution) mapConsumed(mapping *models.Mapping, running string, tcReq, tcResp time.Time, consumed []models.MockState) {
	if mapping == nil || running == "" {
		return
	}
	a.mapped[running] = struct{}{}
	own := make([]models.MockState, 0, len(consumed))
	var startup []models.MockState
	byOwner := map[int][]models.MockState{}
	for _, st := range consumed {
		idx, ok := a.releaseIndex(st)
		switch {
		case !ok:
			own = append(own, st)
		case idx < 0:
			startup = append(startup, st)
		default:
			owner := a.sched.Window(idx).TestCase
			if _, mapped := a.mapped[owner]; !mapped {
				// Its test is not in the mappings yet: hold it for that test,
				// and leave it to the running test's window filter as well,
				// which is what happened before when its test is never mapped
				// this run (not selected, or not run).
				a.early[owner] = append(a.early[owner], st)
				own = append(own, st)
				continue
			}
			byOwner[idx] = append(byOwner[idx], st)
		}
	}
	upsertActualTestMockMapping(mapping, running, own, tcReq, tcResp)
	if held := a.early[running]; len(held) > 0 {
		// Zero window: no filter. These are the running test's own by the
		// release rule, consumed while an earlier test ran.
		upsertActualTestMockMapping(mapping, running, held, time.Time{}, time.Time{})
		delete(a.early, running)
	}
	owners := make([]int, 0, len(byOwner))
	for idx := range byOwner {
		owners = append(owners, idx)
	}
	sort.Ints(owners)
	for _, idx := range owners {
		// Zero window: no filter. The mock is its owner's by the release
		// rule, including one recorded in the gap after the owner's window.
		upsertActualTestMockMapping(mapping, a.sched.Window(idx).TestCase, byOwner[idx], time.Time{}, time.Time{})
	}
	addStartupMocks(mapping, startup)
}

// addStartupMocks appends to the startup section the mocks it does not list yet.
func addStartupMocks(mapping *models.Mapping, consumed []models.MockState) {
	if len(consumed) == 0 {
		return
	}
	have := make(map[string]struct{}, len(mapping.Startup))
	for _, e := range mapping.Startup {
		have[e.Name] = struct{}{}
	}
	var add []models.MockState
	for _, st := range consumed {
		if _, dup := have[st.Name]; dup || st.Name == "" {
			continue
		}
		have[st.Name] = struct{}{}
		add = append(add, st)
	}
	if len(add) == 0 {
		return
	}
	existing := mapping.Startup
	setStartupMocks(mapping, add)
	mapping.Startup = append(existing, mapping.Startup...)
}
