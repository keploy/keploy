package replay

import (
	"context"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// dedup_drop.go — the tests dynamic dedup drops (k8s-proxy auto-replay, through
// DuplicateNamer) leave nothing behind: in the prune of the same run, the mocks
// only they consumed are deleted from the set's mock file and their lines from
// its mapping file, before either is uploaded.
//
// The duplicates pass, so the prune keeps their mocks (they are in the passing
// tests' consumption) and the mapping write writes their entries; the hook's
// caller deletes their test files after the run. Without this the mocks stayed
// for good: mapdb.Insert merges and never deletes an entry, and the entry
// re-protects its mocks in every later run.
//
// A duplicate's line goes only with its mocks: only when every mock it names
// was deleted or is still named by a kept test's line. A mock no line names is
// served to a smart test set as a config mock, so a line that goes alone would
// leave its mocks to be served to every test.

// duplicateMockDropper and duplicateTestDropper are the stores' writes
// (mockdb.MockYaml.DropMocks, mapdb.MappingDb.DropTests); mappingDocReader
// reads the whole mapping file (mapdb.MappingDb.GetMapping). A store without
// them drops nothing.
type duplicateMockDropper interface {
	DropMocks(ctx context.Context, testSetID string, names []string) ([]string, error)
}

type duplicateTestDropper interface {
	DropTests(ctx context.Context, testSetID string, ids []string) error
}

type mappingDocReader interface {
	GetMapping(ctx context.Context, testSetID string) (*models.Mapping, error)
}

// duplicateDropIn is what a run knows when it decides what of its duplicates
// to drop.
type duplicateDropIn struct {
	// dups are the hook's duplicates.
	dups []string
	// consumers are the tests that consumed each mock this run, by name.
	consumers map[string][]string
	// consumed are the passing tests' consumed mock states, by name: their
	// tier (Lifetime), type tag and kind.
	consumed map[string]models.MockState
	// setup names what the set's setup drain consumed (boot traffic, a
	// readiness gate's call).
	setup map[string]bool
	// run is this run's mapping (its entries and sections), disk the file's.
	run, disk *models.Mapping
}

// duplicatePlan is what to drop: mocks to delete, and the duplicates whose
// lines may go with them (linesToDrop says which do, once the mocks are gone).
type duplicatePlan struct {
	mocks []string
	tests []string
	// lines are the mocks each duplicate's line names (this run's entry and
	// the file's); keptNamed the mocks a kept test's line names.
	lines     map[string]map[string]bool
	keptNamed map[string]bool
}

// planDuplicateDrops decides, with no I/O, which mocks to delete: each one
// this run consumed only by duplicates whose lines may go, that is per-test
// (not reusable, not a config mock, not a connection failure), not named by a
// kept test's line, by a set section (startup, runner, worker, shared, boots)
// or by the line of a duplicate that keeps it, and not consumed by the set's
// setup drain. A duplicate's line may go when every mock it names is deleted
// or is a kept test's; one named in a concurrency group never goes. The two
// depend on each other: a duplicate that keeps its line keeps its mocks, which
// may keep another's line, until nothing changes.
func planDuplicateDrops(in duplicateDropIn) duplicatePlan {
	p := duplicatePlan{lines: map[string]map[string]bool{}, keptNamed: map[string]bool{}}
	dup := map[string]bool{}
	for _, d := range in.dups {
		if d != "" {
			dup[d] = true
		}
	}
	if len(dup) == 0 {
		return p
	}
	protected := map[string]bool{}
	for m := range in.setup {
		protected[m] = true
	}
	grouped := map[string]bool{}
	for _, doc := range []*models.Mapping{in.run, in.disk} {
		if doc == nil {
			continue
		}
		for _, section := range [][]models.MockEntry{doc.Startup, doc.Runner, doc.Worker, doc.Shared} {
			for _, e := range section {
				protected[e.Name] = true
			}
		}
		for _, b := range doc.Boots {
			for _, e := range b.Mocks {
				protected[e.Name] = true
			}
		}
		for _, g := range doc.Groups {
			for _, t := range g.Tests {
				grouped[t] = true
			}
		}
		for _, tc := range doc.TestCases {
			names := p.keptNamed
			if dup[tc.ID] {
				if p.lines[tc.ID] == nil {
					p.lines[tc.ID] = map[string]bool{}
				}
				names = p.lines[tc.ID]
			}
			for _, e := range tc.Mocks {
				if e.Name != "" {
					names[e.Name] = true
				}
			}
			for _, ms := range tc.CaseMocks {
				for _, m := range ms {
					if m != "" {
						names[m] = true
					}
				}
			}
		}
	}
	goes := map[string]bool{}
	for d := range dup {
		if !grouped[d] {
			goes[d] = true
		}
	}
	for {
		drop := map[string]bool{}
		for m, by := range in.consumers {
			if len(by) == 0 || protected[m] || p.keptNamed[m] {
				continue
			}
			only := true
			for _, t := range by {
				if !goes[t] {
					only = false
					break
				}
			}
			st, ok := in.consumed[m]
			if !only || !ok || st.Lifetime != models.LifetimePerTest || st.Type == "config" || st.Kind == models.ConnectionFailure {
				continue
			}
			drop[m] = true
		}
		for d, line := range p.lines {
			if !goes[d] {
				for m := range line {
					delete(drop, m)
				}
			}
		}
		next := map[string]bool{}
		for d := range goes {
			if allIn(p.lines[d], drop, p.keptNamed) {
				next[d] = true
			}
		}
		if len(next) == len(goes) {
			p.mocks, p.tests = sortedMockNames(drop), sortedMockNames(goes)
			return p
		}
		goes = next
	}
}

// linesToDrop are the planned duplicates whose every named mock was deleted
// (deleted: what DropMocks reports it deleted) or is still a kept test's.
func (p duplicatePlan) linesToDrop(deleted []string) []string {
	gone := make(map[string]bool, len(deleted))
	for _, m := range deleted {
		gone[m] = true
	}
	var out []string
	for _, d := range p.tests {
		if allIn(p.lines[d], gone, p.keptNamed) {
			out = append(out, d)
		}
	}
	return out
}

// allIn reports whether every name of set is in a or b.
func allIn(set, a, b map[string]bool) bool {
	for m := range set {
		if !a[m] && !b[m] {
			return false
		}
	}
	return true
}

func sortedMockNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dropDuplicates deletes, in a prune that ran, the mocks only the hook's
// duplicates consumed and the lines of the duplicates whose every named mock
// is gone or still a kept test's, and returns those duplicates: the mapping
// write holds them out of the passing tests, so their entries are not written
// back. nil, and nothing written, when the hooks name no duplicate, a store
// cannot drop, or a read or write fails.
func (r *Replayer) dropDuplicates(ctx context.Context, testSetID string, in duplicateDropIn) map[string]struct{} {
	namer, ok := r.hookImpl.(DuplicateNamer)
	if !ok {
		return nil
	}
	if in.dups = namer.GetDuplicateTestCaseNames(testSetID); len(in.dups) == 0 {
		return nil
	}
	mocks, okMocks := r.mockDB.(duplicateMockDropper)
	tests, okTests := r.mappingDB.(duplicateTestDropper)
	reader, okReader := r.mappingDB.(mappingDocReader)
	if !okMocks || !okTests || !okReader {
		r.logger.Debug("kept the mocks and mapping lines of dynamic dedup's duplicates: the mock or mapping store cannot drop them",
			zap.String("testSetID", testSetID), zap.Int("duplicates", len(in.dups)))
		return nil
	}
	disk, err := reader.GetMapping(ctx, testSetID)
	if err != nil {
		r.logger.Warn("kept the mocks and mapping lines of dynamic dedup's duplicates: the mapping file could not be read",
			zap.String("testSetID", testSetID), zap.Error(err))
		return nil
	}
	in.disk = disk
	plan := planDuplicateDrops(in)
	deleted, err := mocks.DropMocks(ctx, testSetID, plan.mocks)
	if err != nil {
		r.logger.Warn("kept the mocks and mapping lines of dynamic dedup's duplicates: deleting their mocks failed",
			zap.String("testSetID", testSetID), zap.Error(err))
		return nil
	}
	ids := plan.linesToDrop(deleted)
	if err := tests.DropTests(ctx, testSetID, ids); err != nil {
		r.logger.Warn("deleted the mocks only dynamic dedup's duplicates consumed, but not their mapping lines",
			zap.String("testSetID", testSetID), zap.Strings("duplicates", ids[:min(len(ids), 20)]), zap.Error(err))
	}
	r.logger.Info("deleted the mocks and mapping lines of the tests dynamic dedup dropped",
		zap.String("testSetID", testSetID),
		zap.Int("duplicates", len(in.dups)),
		zap.Int("linesDeleted", len(ids)),
		zap.Strings("tests", ids[:min(len(ids), 20)]),
		zap.Int("mocksDeleted", len(deleted)),
		zap.Int("mocksPlanned", len(plan.mocks)))
	held := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		held[id] = struct{}{}
	}
	return held
}

// heldIDs lists held's tests.
func heldIDs(held map[string]struct{}) []string {
	out := make([]string, 0, len(held))
	for id := range held {
		out = append(out, id)
	}
	return out
}
