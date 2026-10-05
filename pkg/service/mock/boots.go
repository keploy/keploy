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
		if test := testAt(tests, mk.pid, mk.ts); test != "" {
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

func testAt(tests []models.ScopeWindow, worker uint32, at time.Time) string {
	var mine, any []models.ScopeWindow
	for _, w := range tests {
		if at.Before(w.Start) || at.After(w.End) {
			continue
		}
		any = append(any, w)
		if worker != 0 && w.PID == worker {
			mine = append(mine, w)
		}
	}
	pick := mine
	if len(pick) == 0 {
		pid := uint32(0)
		for i, w := range any {
			if i > 0 && w.PID != pid {
				return ""
			}
			pid = w.PID
		}
		pick = any
	}
	if len(pick) == 0 {
		return ""
	}
	sort.SliceStable(pick, func(i, j int) bool { return pick[i].Start.After(pick[j].Start) })
	return pick[0].Name
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
