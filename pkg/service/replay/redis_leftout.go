package replay

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/redisskip"
	"go.keploy.io/server/v3/pkg/models"
)

// leftOutRedis is the names of a set's mocks this build leaves out of the
// replay (redisskip.Recorded): of the mocks read (lists), and of the
// documents the store could not decode (skipped, their kinds by name), the
// Keploy Enterprise Redis mocks among them, which a build without a Redis
// parser cannot read. The agent never receives those, but a mapping may name
// them. nil in a build that links a Redis parser, or when none is left out.
func leftOutRedis(skipped map[string]models.Kind, lists ...[]*models.Mock) map[string]bool {
	r := redisskip.NewRecorded()
	for _, l := range lists {
		for _, m := range l {
			r.Add(m)
		}
	}
	for name, kind := range skipped {
		if kind == models.REDIS {
			r.Add(&models.Mock{Name: name, Kind: kind})
		}
	}
	return r.LeftOut()
}

// withoutNames is mocks less those left names: mocks itself when left is
// empty, a new slice otherwise.
func withoutNames(mocks []*models.Mock, left map[string]bool) []*models.Mock {
	if len(left) == 0 {
		return mocks
	}
	out := make([]*models.Mock, 0, len(mocks))
	for _, m := range mocks {
		if m == nil || !left[m.Name] {
			out = append(out, m)
		}
	}
	return out
}

// withoutLeftOutRedis is a set's per-test mappings and worker scope less
// every entry that names a mock this build leaves out of the replay (left):
// the set's Redis mocks, in a build without a Redis parser, whose proxy
// relays Redis to the real server (redisskip.LeftOutOf over the set's mocks,
// which the agent reads the same way as it stores them, and leaves out of the
// pool it serves). The call such a mock recorded is made for real, so no test
// owns it, no worker scope waits for it, and nothing is held to consuming
// it. It also returns how many entries it took out of the per-test mappings.
// Without one to leave out, the mappings and worker scope are returned as
// they are.
func withoutLeftOutRedis(mappings map[string][]models.MockEntry, worker []models.MockEntry, left map[string]bool) (map[string][]models.MockEntry, []models.MockEntry, int) {
	if len(left) == 0 {
		return mappings, worker, 0
	}
	taken := 0
	keep := func(entries []models.MockEntry) []models.MockEntry {
		if entries == nil {
			return nil
		}
		out := make([]models.MockEntry, 0, len(entries))
		for _, e := range entries {
			if !left[e.Name] {
				out = append(out, e)
			}
		}
		taken += len(entries) - len(out)
		return out
	}
	var kept map[string][]models.MockEntry
	if mappings != nil {
		kept = make(map[string][]models.MockEntry, len(mappings))
		for test, entries := range mappings {
			kept[test] = keep(entries)
		}
	}
	fromTests := taken
	return kept, keep(worker), fromTests
}

// withoutLeftOutConcurrency is the set's concurrency (concurrencyOf) less
// every grouped, marked or shared entry that names a mock this build leaves
// out of the replay (left), as withoutLeftOutRedis strips the mappings: such
// a mock's call is made for real, so it is no candidate any test may be
// served, no other test's miss may be its call (attribute), and an
// ambiguous mark keploy record's full round left on it
// (MockEntry.Unresolved, which a replay that does not share charges the test
// that lists it with: annotateConcurrent) charges no test. The groups stay:
// they say which tests were open together. conc itself when nothing is left
// out; nil, as concurrencyOf says a set that states none, when no group and
// no entry is left.
func withoutLeftOutConcurrency(conc *setConcurrency, left map[string]bool) *setConcurrency {
	if conc == nil || len(left) == 0 {
		return conc
	}
	out := &setConcurrency{groups: conc.groups, ofTest: conc.ofTest, entries: make(map[string]groupedEntry, len(conc.entries))}
	for name, e := range conc.entries {
		if !left[name] {
			out.entries[name] = e
		}
	}
	if len(out.groups) == 0 && len(out.entries) == 0 {
		return nil
	}
	return out
}

// keepLeftOutRedis adds to keep, the mocks --remove-unused-mocks retains
// (UpdateMocks), each mock this build left out of the replay (left) that keep
// does not hold. Such a mock goes unconsumed because this build relays its
// call to the real Redis, not because the application no longer makes it, so
// its going unconsumed says nothing about whether it is needed, as a
// connection failure's does not (mockdb's pruneKeeps). A Generic one is kept
// anyway (the Generic recorder writes every mock as a config one), but a
// Keploy Enterprise Redis mock this build decodes (from a gob mock file; a
// yaml or json one is copied as written) has no such type, and pruning it
// would delete a recording Keploy Enterprise replays.
func keepLeftOutRedis(keep map[string]models.MockState, left map[string]bool) {
	for name := range left {
		if _, ok := keep[name]; !ok {
			keep[name] = models.MockState{Name: name}
		}
	}
}
