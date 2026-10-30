package mock

import (
	"slices"
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

type bootPlan struct {
	tests   map[string][]models.MockEntry
	boots   []models.BootSpec
	perTest []capturedMock
}

func classify(tests, starts []models.ScopeWindow, suites []models.SuiteSpan, mocks []capturedMock) bootPlan {
	byRef := make(map[string]int, len(starts))
	boots := make([]models.BootSpec, 0, len(starts))
	for _, s := range starts {
		byRef[s.Ref] = len(boots)
		boots = append(boots, models.BootSpec{Dir: s.Dir, Key: s.Name, Program: s.Program, Place: s.Place, N: s.N, Ready: !s.Ready.IsZero(), Mocks: []models.MockEntry{}})
	}
	runner := map[string]int{}
	out := bootPlan{tests: map[string][]models.MockEntry{}}
	for _, mk := range mocks {
		entry := models.MockEntry{Name: mk.name, Kind: mk.kind}
		if i, ok := byRef[mk.ref]; ok && mk.ref != "" {
			s := starts[i]
			if mk.boot || (!s.Ready.IsZero() && mk.ts.Before(s.Ready)) {
				boots[i].Mocks = append(boots[i].Mocks, entry)
				continue
			}
			test := testAt(tests, s.Worker, mk.chain, mk.ts)
			if test == "" {
				boots[i].Mocks = append(boots[i].Mocks, entry)
				continue
			}
			entry.Start = s.Name
			out.tests[test] = append(out.tests[test], entry)
			out.perTest = append(out.perTest, mk)
			continue
		}
		if test := testAt(tests, mk.pid, mk.chain, mk.ts); test != "" {
			out.tests[test] = append(out.tests[test], entry)
			out.perTest = append(out.perTest, mk)
			continue
		}
		dir := suiteAt(suites, mk.ts)
		j, ok := runner[dir]
		if !ok {
			j = len(boots)
			runner[dir] = j
			boots = append(boots, models.BootSpec{Dir: dir})
		}
		boots[j].Mocks = append(boots[j].Mocks, entry)
	}
	out.boots = boots
	return out
}

// testAt names the test whose window holds a per-test call made at at, or ""
// when none, or no one, does. worker is the process the call is attributed
// to (the mock's own pid, or the worker that started its app), 0 if unknown;
// chain the call's process and its ancestors (models.Mock.SourceChain), nil
// if unknown.
//
// Under the proven switch it asks models.CallOwner, the rule the Generic
// recorder's held first exchange asks (syncMock.SyncMockManager.TestSpanAt),
// with the same chain: a call the recorder states as one test's, unmarked,
// is that test's here, and one it could not place (marked manyTests) is no
// test's here either. The worker is on the chain: with no chain read, the
// chain is the worker alone, and a start's worker the chain read at a later
// accept no longer shows (its app was reparented) is put on it, since the
// start registry saw the app under that worker.
// Without the switch, a window whose PID is the worker's exactly owns it,
// the latest begun of several; with none, the latest begun of the windows
// holding at, when they are all one worker's.
func testAt(tests []models.ScopeWindow, worker uint32, chain []uint32, at time.Time) string {
	if provenswitch.On() {
		return ownerAt(tests, worker, chain, at)
	}
	var mine, all []models.ScopeWindow
	for _, w := range tests {
		if at.Before(w.Start) || at.After(w.End) {
			continue
		}
		all = append(all, w)
		if worker != 0 && w.PID == worker {
			mine = append(mine, w)
		}
	}
	pick := mine
	if len(pick) == 0 {
		pid := uint32(0)
		for i, w := range all {
			if i > 0 && w.PID != pid {
				return ""
			}
			pid = w.PID
		}
		pick = all
	}
	if len(pick) == 0 {
		return ""
	}
	sort.SliceStable(pick, func(i, j int) bool { return pick[i].Start.After(pick[j].Start) })
	return pick[0].Name
}

// ownerAt is testAt under the proven switch: models.CallOwner of the windows
// holding at.
func ownerAt(tests []models.ScopeWindow, worker uint32, chain []uint32, at time.Time) string {
	if worker != 0 && !slices.Contains(chain, worker) {
		chain = append(slices.Clip(chain), worker)
	}
	var holding []models.ScopeWindow
	var cands []models.OwnerCandidate
	for _, w := range tests {
		if at.Before(w.Start) || at.After(w.End) {
			continue
		}
		holding = append(holding, w)
		cands = append(cands, models.OwnerCandidate{PID: w.PID, Start: w.Start})
	}
	if i := models.CallOwner(cands, chain); i >= 0 {
		return holding[i].Name
	}
	return ""
}

func suiteAt(suites []models.SuiteSpan, at time.Time) string {
	best := ""
	var start time.Time
	for _, s := range suites {
		if at.Before(s.Start) || (!s.End.IsZero() && at.After(s.End)) {
			continue
		}
		if best == "" || s.Start.After(start) {
			best, start = s.Dir, s.Start
		}
	}
	return best
}
