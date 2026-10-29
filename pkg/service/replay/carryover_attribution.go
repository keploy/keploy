package replay

import (
	"sort"
	"strings"
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
//
// A flagged mock consumed before the set's first test is not attributed
// here: RunTestSet keeps it and maps it to the startup section, whatever its
// window (splitCarriedConsumed). An unflagged per-test mock the app's startup
// asked for (requested before the set's first test) is the startup section's,
// whichever drain caught it (bootBand), unless that drain is the test whose
// window its answer came in: that test's window filter keeps it there.
type carryOverAttribution struct {
	sched *models.WindowSchedule
	// mapped holds the tests whose consumed mocks this run has already put in
	// the mapping. A mock is mapped to its own test only if that test is one of
	// them: an entry for a test this run did not map would replace that test's
	// recorded mapping with this one mock.
	mapped map[string]struct{}
	// consumedSome holds the tests mapConsumed was given at least one
	// consumed mock for that is not the startup section's, in any call (a
	// RetryPassing cycle calls it again). A mapped test outside it consumed
	// nothing but startup traffic, if anything (consumedNothing).
	consumedSome map[string]struct{}
	// early holds, by owning test, the flagged mocks consumed while that test
	// was not mapped yet: a carry-over SEND the app published before its own
	// test ran (the lookahead lets it land up to a few windows early). They
	// are added to the owner's entry when the owner is mapped, and to none if
	// it never is.
	early map[string][]models.MockState
	// bootEnd is the set's first recorded test's start
	// (firstRecordedTestStart): a mock requested before it is the app's
	// startup traffic (bootBand). The agent stages the startup band by it,
	// or by an earlier cutoff when the replay reports the first test deleted
	// (recordedSetShape); a mock between the two is listed, never staged.
	bootEnd time.Time
	// bootListed: the startup section lists startup traffic of the app's
	// (bootBand), which it writes in recorded order (sortByRecordedTime).
	bootListed bool
}

func newCarryOverAttribution(testCases []*models.TestCase) *carryOverAttribution {
	return &carryOverAttribution{
		sched:        models.NewWindowSchedule(recordedTestWindows(testCases)),
		mapped:       make(map[string]struct{}),
		consumedSome: make(map[string]struct{}),
		early:        make(map[string][]models.MockState),
		bootEnd:      firstRecordedTestStart(testCases),
	}
}

// consumedNothing names the tests whose consumption mapConsumed was given and
// that consumed no mock but the startup section's: not one their window filter
// dropped, not one another test owns. Only these may get an entry with no mock
// (addConsumedNothingEntries). A test whose drain caught only startup traffic
// (a boot consumer still fetching, a flagged mock released before the first
// test) is one: the section, in the same file, lists those mocks, and a replay
// that loads by name serves them to every test. A test whose every consumed
// mock went to another test's window, or to none, keeps what record gave it,
// as before.
func (a *carryOverAttribution) consumedNothing() map[string]struct{} {
	out := make(map[string]struct{}, len(a.mapped))
	for id := range a.mapped {
		if _, some := a.consumedSome[id]; !some {
			out[id] = struct{}{}
		}
	}
	return out
}

// bootBand is the per-test mocks of consumed, caught by the drain of the
// running test, that the app's startup asked for and that the startup section
// must list whatever their lifetime, as it lists the reusable ones: a boot
// consumer's fetches (bootBandOf). A replay that loads by name (a
// mapping-mode test, a fetch of the mapped mocks alone) reaches no other: the
// window filter maps such a mock to no test, and nothing re-stages it by its
// recorded time.
func (a *carryOverAttribution) bootBand(consumed []models.MockState, running string) []models.MockState {
	if a == nil {
		return nil
	}
	return bootBandOf(consumed, a.sched, a.bootEnd, running)
}

// bootBandOf is the per-test mocks of consumed the app's startup asked for
// (requested before bootEnd, the set's first recorded test's start; not
// reusable, not flagged carry-over, not DNS) that the drain that caught them
// does not own. drain is the test whose drain this is, "" for the set's setup
// drain before any test. A replay answers such a fetch as soon as the app asks
// for it, whenever its answer came at record, so the drain that caught it is
// when a replay asks for it: the startup section's, unless the drain is the
// test whose window its answer came in, whose window filter keeps it in that
// test's list. Listing it under that test from another drain would expect the
// test to consume what a replay serves before it. Nothing when bootEnd is zero.
//
// Left out:
//   - the reusable tiers, as before (the window filter keeps them in each
//     test's list);
//   - a per-test mock recorded inside a test's window and consumed before
//     the first test (a readiness gate's). It is its test's. The startup
//     band is staged by recorded time (DiskMocks.LoadBefore), so listing it
//     there would not serve it at boot either, and it would drop it from its
//     test's report (dropStartupMockInfos, report-only).
func bootBandOf(consumed []models.MockState, sched *models.WindowSchedule, bootEnd time.Time, drain string) []models.MockState {
	if bootEnd.IsZero() {
		return nil
	}
	var out []models.MockState
	for _, st := range consumed {
		if !bootAsked(st, bootEnd) {
			continue
		}
		if drain != "" && answerOwner(st, sched) == drain {
			continue
		}
		out = append(out, st)
	}
	return out
}

// withoutStartupTraffic is what running consumed less the app's startup
// traffic: what the mapping's startup section lists (startupNames) and the
// boot band of running's drain (bootBand). A streaming test's mismatch
// compares what it consumed with its own entry, and startup traffic it
// consumed (a boot consumer still catching up, which the startup section's
// names, merged into its load, let it reach) is no other test's: it is no
// divergence of its mapping, as the regular path drops startup names from both
// sides (dropStartupMockInfos). Without that, a wrong answer was demoted to
// OBSOLETE for it. consumed itself when there is nothing to drop.
func (a *carryOverAttribution) withoutStartupTraffic(consumed []models.MockState, startupNames []string, running string) []models.MockState {
	drop := make(map[string]bool, len(startupNames))
	for _, n := range startupNames {
		drop[n] = true
	}
	for _, st := range a.bootBand(consumed, running) {
		drop[st.Name] = true
	}
	if len(drop) == 0 {
		return consumed
	}
	out := make([]models.MockState, 0, len(consumed))
	for _, st := range consumed {
		if !drop[st.Name] {
			out = append(out, st)
		}
	}
	return out
}

// sortByRecordedTime orders entries by recorded request time, stably (an
// entry without one first, as recorded).
func sortByRecordedTime(entries []models.MockEntry) {
	at := func(e models.MockEntry) time.Time {
		t, _ := models.ParseMockTimestamp(e.ReqTimestampMock)
		return t
	}
	sort.SliceStable(entries, func(i, j int) bool { return at(entries[i]).Before(at(entries[j])) })
}

// bootAsked reports whether st is a per-test mock the app's startup asked
// for: placed before bootEnd (requested, or answered for a mock placed at its
// answer, MockState.AnswerPlaced), not reusable, not flagged carry-over, not
// DNS.
func bootAsked(st models.MockState, bootEnd time.Time) bool {
	if isReusableTierState(st) || st.CarryOver || strings.EqualFold(string(st.Kind), string(models.DNS)) || st.ReqTimestampMock == "" {
		return false
	}
	placed := st.ReqTimestampMock
	if st.AnswerPlaced && st.ResTimestampMock != "" {
		placed = st.ResTimestampMock
	}
	at, err := models.ParseMockTimestamp(placed)
	return err == nil && !at.IsZero() && at.Before(bootEnd)
}

// answerOwner is the test of the earliest recorded window st's answer came in,
// "" for none.
func answerOwner(st models.MockState, sched *models.WindowSchedule) string {
	res, err := models.ParseMockTimestamp(st.ResTimestampMock)
	if err != nil || res.IsZero() {
		return ""
	}
	if idx := sched.IndexOf(res); idx >= 0 {
		return sched.Window(idx).TestCase
	}
	return ""
}

// releaseIndex is the recorded window st belongs to (-1: before every window).
// ok is false when st is not flagged, has no usable timestamp, or there are no
// windows.
func (a *carryOverAttribution) releaseIndex(st models.MockState) (int, bool) {
	if a == nil || !st.CarryOver || a.sched.Len() == 0 || st.ReqTimestampMock == "" {
		return 0, false
	}
	placed := st.ReqTimestampMock // a mock placed at its answer, by its answer
	if st.AnswerPlaced && st.ResTimestampMock != "" {
		placed = st.ResTimestampMock
	}
	t, err := models.ParseMockTimestamp(placed)
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
	// A startup mock the running test's drain caught (a boot consumer still
	// fetching what the app's startup was recorded fetching) is the startup
	// section's, unless its answer came in the running test's window: the
	// window filter below keeps that one in the test's list.
	if boot := a.bootBand(own, running); len(boot) > 0 {
		startup = append(startup, boot...)
		a.bootListed = true
	}
	// startup is part of consumed, each mock once: a consumed mock outside it
	// is one the startup section does not list (consumedNothing).
	if len(consumed) > len(startup) {
		a.consumedSome[running] = struct{}{}
	}
	upsertActualTestMockMapping(mapping, running, own, tcReq, tcResp)
	// A call in flight when the running test began (sent in the gap before
	// it, answered after it ended) is its own, though the window filter
	// above drops it: no request or response time of it is in the window.
	if late := a.inFlightAnsweredLate(own, tcReq, tcResp); len(late) > 0 {
		upsertActualTestMockMapping(mapping, running, late, time.Time{}, time.Time{})
	}
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

// inFlightAnsweredLate is the unflagged per-test mocks of consumed that were in
// flight when the test of window [tcReq, tcResp] began (models.WindowSchedule.
// InFlightInto) and answered after it ended: the test's own, which the window
// filter drops. One answered within the window the filter keeps, as it keeps
// every DNS, session and connection mock (upsertActualTestMockMapping), so
// none of those is listed twice. A connection failure is no test's but its
// own window's (models.InFlightEligible).
func (a *carryOverAttribution) inFlightAnsweredLate(consumed []models.MockState, tcReq, tcResp time.Time) []models.MockState {
	if a == nil || a.sched.Len() == 0 || tcReq.IsZero() || tcResp.IsZero() {
		return nil
	}
	var out []models.MockState
	for _, st := range consumed {
		// A mock placed at its answer (MockState.AnswerPlaced) is its
		// answer's window's, wherever it was sent: the in-flight rule is not
		// its own (models.InFlightEligible).
		if st.CarryOver || st.AnswerPlaced || st.ReqTimestampMock == "" || st.ResTimestampMock == "" {
			continue
		}
		if st.Lifetime != models.LifetimePerTest || strings.EqualFold(string(st.Kind), string(models.DNS)) || models.WindowBound(st.Kind) {
			continue
		}
		req, rerr := models.ParseMockTimestamp(st.ReqTimestampMock)
		res, serr := models.ParseMockTimestamp(st.ResTimestampMock)
		if rerr != nil || serr != nil {
			continue
		}
		if res.After(tcResp) && a.sched.InFlightInto(req, res, tcReq) {
			out = append(out, st)
		}
	}
	return out
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
