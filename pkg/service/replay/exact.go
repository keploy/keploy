package replay

import (
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Ownership and exact consumption for keploy test, under the proven switch
// (design v14 §5.2, S12): the replay hands the agent what each test case
// owns, and the agent serves each test case exactly its own mocks, each at
// most once and in recorded order, through the proxy's exact serving.
//
//   - A test case owns the mocks its mapping lists (the agent keeps the
//     data-plane ones). A test case the mapping omits, or lists with none,
//     owns none, and so does every test case of an all-empty mapping: a call
//     it makes is a miss. disableMapping does not apply.
//   - A set with no mappings.yaml (a recording made without the switch), or
//     one keploy test wrote from what a replay consumed rather than keploy
//     record (models.MappingSourceKeployTest), owns what its test windows
//     held: each test case the test mocks recorded inside its own window,
//     derived as keploy record derives mappings.yaml under the switch
//     (windowOwnership, models.DeriveWindowMappings), documents this keploy
//     cannot read included.
//   - A mapping that cannot be read refuses the set.
//   - Every mock no test case lists is the set's pool. It answers between
//     test cases, beside the protocol-phase mocks, with reuse or
//     cursor-saturate as recorded.
//   - The set is staged on the agent once; a test case's call opens it, and
//     the call after its consumption is read serves the pool again.
//   - The replay never writes mappings.yaml: only record does.

// exactPlan is the set's exact serving: what each test case owns, and the
// set's pool.
type exactPlan struct {
	owned map[string][]string
	pool  []string
	// ownsTestMocks: some test case owns a test mock (a data-plane mock the
	// agent holds).
	ownsTestMocks bool
}

// exactServing reports whether this replay serves each test case exactly
// its own mocks: under the proven switch, with an agent to serve them.
func (r *Replayer) exactServing() bool {
	return provenswitch.On() && r.instrument && !r.noAgent
}

// provenMappings reads what each test case of testSetID owns, under the
// proven switch, and an error for a mapping that cannot be read. It gives
// none, and why, for a set whose ownership its test windows then give
// (windowOwnership): one with no mapping store or no mappings.yaml, and one
// whose mappings.yaml keploy test wrote (models.MappingSourceKeployTest),
// which lists what a replay consumed, not what each test recorded. A
// mappings.yaml with no source is trusted as recorded: one keploy test wrote
// before it said so cannot be told from keploy record's.
func (r *Replayer) provenMappings(ctx context.Context, testSetID string) (map[string][]models.MockEntry, string, error) {
	const absent = "the test set has no mappings.yaml"
	if r.mappingDB == nil {
		return map[string][]models.MockEntry{}, absent, nil
	}
	if mr, ok := r.mappingDB.(mappingReader); ok {
		m, err := mr.GetMapping(ctx, testSetID)
		if err != nil {
			return nil, "", fmt.Errorf("keploy cannot judge each test case of %s: its mappings.yaml cannot be read: %w; record the set again", testSetID, err)
		}
		if m != nil && m.Source == models.MappingSourceKeployTest {
			return map[string][]models.MockEntry{}, "the test set's mappings.yaml was written by keploy test, from what a replay consumed, not by keploy record", nil
		}
		mappings := map[string][]models.MockEntry{}
		if m != nil {
			for _, tc := range m.TestCases {
				mappings[tc.ID] = tc.Mocks
			}
		}
		return mappings, absent, nil
	}
	mappings, _, err := r.mappingDB.Get(ctx, testSetID)
	if err != nil {
		return nil, "", fmt.Errorf("keploy cannot judge each test case of %s: its mappings.yaml cannot be read: %w; record the set again", testSetID, err)
	}
	if mappings == nil {
		mappings = map[string][]models.MockEntry{}
	}
	return mappings, absent, nil
}

// mappingReader is the part of a mapping store that reads a set's whole
// mappings.yaml, which says which keploy command wrote its test entries
// (models.Mapping.Source): nil for no file.
type mappingReader interface {
	GetMapping(ctx context.Context, testSetID string) (*models.Mapping, error)
}

// windowOwnership is what each test case of testSetID owns when no
// mappings.yaml keploy record wrote says so (why, from provenMappings): the
// test mocks recorded inside its own window, the earliest-opened one where
// two were open (models.DeriveWindowMappings), as keploy record derives a
// recording's mappings.yaml under the switch (S3). A recording made without
// the switch has none; replayed under it, a test case owns what its window
// held, never nothing, and never another test's call. cases are every test
// case of the set, run or not, so a mock of one not selected is no other's.
// stored is every mock read, both pools; skipped the documents the decoders
// could not read (models.TestSetMocks.SkippedWindowed): a test case owns a
// recorded call this keploy cannot serve, and is judged for it. The replay
// never writes what it derives.
func (r *Replayer) windowOwnership(testSetID string, cases []*models.TestCase, why string, skipped []models.WindowedMock, stored ...[]*models.Mock) map[string][]models.MockEntry {
	windows := make([]models.TestWindow, 0, len(cases))
	for _, tc := range cases {
		windows = append(windows, models.RecordedWindow(tc))
	}
	var mocks []models.WindowedMock
	seen := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk == nil || mk.Name == "" || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			mocks = append(mocks, models.NewWindowedMock(mk))
		}
	}
	for _, w := range skipped {
		if w.Name == "" || seen[w.Name] {
			continue
		}
		seen[w.Name] = true
		mocks = append(mocks, w)
	}
	derived := models.DeriveWindowMappings(windows, mocks)
	out := make(map[string][]models.MockEntry, len(derived.Tests))
	mapped := 0
	for _, tc := range derived.Tests {
		out[tc.ID] = tc.Mocks
		mapped += len(tc.Mocks)
	}
	r.logger.Info("under KEPLOY_PROVEN each test case of this test set owns the calls recorded inside its own window, as keploy record maps a recording made under the switch",
		zap.String("testSetID", testSetID),
		zap.String("reason", why),
		zap.Int("numTests", len(out)),
		zap.Int("ownedMocks", mapped),
		zap.String("next_step", "record the test set again under KEPLOY_PROVEN for a mappings.yaml keploy record writes"))
	if n := len(derived.Concurrent); n > 0 {
		r.logger.Warn("some tests of this test set were recorded while another test's request was still open: a call recorded while two tests were open is owned by the test whose request came first, which may not be the test that made it",
			zap.String("testSetID", testSetID),
			zap.Int("count", n),
			zap.Strings("tests", derived.Concurrent[:min(n, 20)]),
			zap.String("next_step", "if one of these tests fails with a call it did not make or a call no recorded mock answered, record the test set again under KEPLOY_PROVEN, one request at a time"))
	}
	return out
}

// newExactPlan is the plan for a set whose test cases own mappings and whose
// stored mocks are stored (what the agent holds): every stored mock no
// mapping lists is the pool's. A test case owns the stored mocks its mapping
// lists. One it lists that the agent does not hold (a connection failure,
// which keploy never hands it, or one the store pruned) answers nothing: the
// agent is not asked for it, and the test case is judged for it all the
// same, as a recorded call it did not make (unconsumedTestMocks reads the
// whole mapping).
func newExactPlan(mappings map[string][]models.MockEntry, stored ...[]*models.Mock) *exactPlan {
	p := &exactPlan{owned: make(map[string][]string, len(mappings))}
	// held: the stored mocks by name, true for a test mock (data-plane).
	held := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk != nil && mk.Name != "" {
				held[mk.Name] = !mk.IsProtocolPhase()
			}
		}
	}
	mapped := map[string]bool{}
	for test, entries := range mappings {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.Name == "" {
				continue
			}
			mapped[e.Name] = true
			dataPlane, ok := held[e.Name]
			if !ok {
				continue
			}
			names = append(names, e.Name)
			p.ownsTestMocks = p.ownsTestMocks || dataPlane
		}
		p.owned[test] = names
	}
	seen := map[string]bool{}
	for _, list := range stored {
		for _, mk := range list {
			if mk == nil || mk.Name == "" || mapped[mk.Name] || seen[mk.Name] {
				continue
			}
			seen[mk.Name] = true
			p.pool = append(p.pool, mk.Name)
		}
	}
	return p
}

// serveExactTest opens test case name on the agent, served exactly its own
// mocks; vars are the test variables its request carries.
func (r *Replayer) serveExactTest(ctx context.Context, plan *exactPlan, name string, vars ...models.VariableBinding) error {
	err := r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{
		Variables: vars,
		Exact:     &models.ExactServing{Test: name, Owned: append([]string(nil), plan.owned[name]...)},
	})
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test case its own mocks", zap.String("testCase", name))
	}
	return err
}

// serveExactPool serves the set's pool again, once a test case's
// consumption is read: a call the application makes between test cases is
// the pool's.
func (r *Replayer) serveExactPool(ctx context.Context, plan *exactPlan) error {
	if plan == nil {
		return nil
	}
	err := r.instrumentation.UpdateMockParams(ctx, models.MockFilterParams{Exact: &models.ExactServing{}})
	if err != nil {
		utils.LogError(r.logger, err, "failed to serve the test set's pool between test cases")
	}
	return err
}

// noteMappingAlwaysUsed says once per run that disableMapping does not apply
// under the proven switch.
func (r *Replayer) noteMappingAlwaysUsed() {
	r.noteIgnored("disableMapping (--disable-mapping)", "each test case is served exactly the mocks its mapping lists")
}

// The settings exact serving deletes (design v14 §10, U18 rows of S12):
// under the proven switch each is ignored, said once per run, and never
// weakens a verdict. The flip deletes them.

// statefulMocksDisabled is test.disableStatefulMocks, ignored under the
// switch: a test's own mocks answer once each in recorded order, and the
// pool keeps its cursors.
func (r *Replayer) statefulMocksDisabled() bool {
	if !r.config.Test.DisableStatefulMocks {
		return false
	}
	if provenswitch.On() {
		r.noteIgnored("disableStatefulMocks", "a test's own mocks answer once each, in recorded order")
		return false
	}
	return true
}

// removeUnusedMocks is test.removeUnusedMocks, ignored under the switch: a
// passing test leaves no test mock unconsumed, so pruning would only delete
// pool mocks another run may need (preserveFailedMocks goes with it).
func (r *Replayer) removeUnusedMocks() bool {
	if !r.config.Test.RemoveUnusedMocks {
		return false
	}
	if provenswitch.On() {
		r.noteIgnored("removeUnusedMocks (--remove-unused-mocks)", "a passing test leaves none of its mocks unconsumed, and the pool's are other runs' too")
		return false
	}
	return true
}

// noteIgnored says once per run that setting does not apply under the switch.
func (r *Replayer) noteIgnored(setting, why string) {
	if _, said := r.ignoredSaid.LoadOrStore(setting, true); said {
		return
	}
	r.logger.Warn(setting+" is ignored under KEPLOY_PROVEN: "+why,
		zap.String("next_step", "remove it from the configuration or the command line"))
}

// noteOwnsNothing says once per set, under the switch, that no test case of
// testSetID owns a test mock: every call a test case makes to a dependency
// misses.
func (r *Replayer) noteOwnsNothing(testSetID string) {
	if r.exact == nil || r.exact.ownsTestMocks {
		return
	}
	r.logger.Warn("under KEPLOY_PROVEN no test case of this test set owns a recorded call: its mappings.yaml, or with none its test windows, list no data-plane mock the agent holds, so every call a test case makes to a dependency misses",
		zap.String("testSetID", testSetID),
		zap.String("next_step", "record the test set again with this keploy (keploy record writes mappings.yaml)"))
}
