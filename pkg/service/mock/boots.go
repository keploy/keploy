package mock

import (
	"sort"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

type bootPlan struct {
	tests   map[string][]models.MockEntry
	boots   []models.BootSpec
	perTest []capturedMock
}

// classify files each recorded mock under the test that made it, the app
// start it belongs to, or, for any other call made outside every test, the
// start-up of the test folder whose process made it (owners: worker PID to
// folder). A call whose process belongs to no known worker is filed with no
// folder: the start-up every set is served. It is never placed by time.
func classify(tests, starts []models.ScopeWindow, owners map[uint32]string, mocks []capturedMock) bootPlan {
	byRef := make(map[string]int, len(starts))
	boots := make([]models.BootSpec, 0, len(starts))
	for _, s := range starts {
		byRef[s.Ref] = len(boots)
		boots = append(boots, models.BootSpec{Dir: s.Dir, Key: s.Name, Program: s.Program, Place: s.Place, N: s.N, Ready: !s.Ready.IsZero(), Mocks: []models.MockEntry{}})
	}
	runner := map[string]int{}
	out := bootPlan{tests: map[string][]models.MockEntry{}}
	for _, mk := range mocks {
		entry := models.MockEntry{Name: mk.name}
		if i, ok := byRef[mk.ref]; ok && mk.ref != "" {
			s := starts[i]
			if mk.boot || (!s.Ready.IsZero() && mk.ts.Before(s.Ready)) {
				boots[i].Mocks = append(boots[i].Mocks, entry)
				continue
			}
			test := testAt(tests, s.Worker, mk.ts)
			if test == "" {
				boots[i].Mocks = append(boots[i].Mocks, entry)
				continue
			}
			entry.Start = s.Name
			out.tests[test] = append(out.tests[test], entry)
			out.perTest = append(out.perTest, mk)
			continue
		}
		who := mk.worker
		if who == 0 {
			who = mk.pid
		}
		if test := testAt(tests, who, mk.ts); test != "" && (!mk.boot || len(starts) > 0) {
			out.tests[test] = append(out.tests[test], entry)
			out.perTest = append(out.perTest, mk)
			continue
		}
		dir := owners[who]
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

func testAt(tests []models.ScopeWindow, worker uint32, at time.Time) string {
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

// workerDirs maps each worker PID that sent a scope mark to the folder its
// marks named. A PID seen with two folders maps to none.
func workerDirs(windows []models.ScopeWindow) map[uint32]string {
	out := map[uint32]string{}
	for _, w := range windows {
		if w.App || w.PID == 0 || w.Dir == "" {
			continue
		}
		if d, ok := out[w.PID]; ok && d != w.Dir {
			out[w.PID] = ""
			continue
		}
		out[w.PID] = w.Dir
	}
	return out
}
