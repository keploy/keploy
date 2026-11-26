package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

var _ integrations.ReadThroughMatcher = (*MockManager)(nil)
var _ integrations.ReadThroughMatcher = (*heldScope)(nil)
var _ integrations.ReadThroughMatcher = (*scopedMockDb)(nil)

// readThroughStaged carries a validated read-through mock along with its
// precomputed canonical key so setMocksWithWindowLocked never recomputes
// ReadThroughKey under treesMu.Lock.
type readThroughStaged struct {
	mock *models.Mock
	key  string
}

// readThroughStore holds the deduplicated, bounded session read-through tier
// on MockManager. Guarded by MockManager.treesMu.
type readThroughStore struct {
	byKey      map[string]*models.Mock
	byName     map[string]*models.Mock
	respDigest map[string]string
	conflicted map[string]struct{}
	order      []*models.Mock
	warnedCap  bool
}

func (s *readThroughStore) clear() {
	s.byKey = nil
	s.byName = nil
	s.respDigest = nil
	s.conflicted = nil
	s.order = nil
	s.warnedCap = false
}

func (s *readThroughStore) upsertKey(mk *models.Mock, key string, capEntries int, logger *zap.Logger) {
	if mk == nil || key == "" {
		return
	}
	if s.byKey == nil {
		s.byKey = make(map[string]*models.Mock, 32)
		s.byName = make(map[string]*models.Mock, 32)
		s.respDigest = make(map[string]string, 32)
		s.conflicted = make(map[string]struct{})
	}
	if _, poisoned := s.conflicted[key]; poisoned {
		return
	}
	digest := models.ReadThroughResponseDigest(mk)
	if existing, exists := s.byKey[key]; exists {
		if s.respDigest[key] != digest {
			// Conflicting responses recorded for the same canonical key: fail
			// closed by evicting the key and blocking it from returning.
			delete(s.byKey, key)
			delete(s.respDigest, key)
			if existing != nil && existing.Name != "" {
				delete(s.byName, existing.Name)
			}
			s.conflicted[key] = struct{}{}
			if logger != nil {
				logger.Warn("mockmanager: conflicting read-through responses detected for the same canonical key; disabling key fail-closed",
					zap.String("kind", string(mk.Kind)),
					zap.String("existingMock", existing.Name),
					zap.String("conflictingMock", mk.Name),
					zap.String("next_step", "separate distinct servers onto distinct ports/databases or record dedicated per-test mocks for mutable responses"))
			}
		}
		return
	}
	if capEntries <= 0 {
		capEntries = models.DefaultReadThroughMaxEntries
	}
	if len(s.order) >= capEntries {
		if !s.warnedCap && logger != nil {
			s.warnedCap = true
			logger.Warn("mockmanager: session read-through tier reached entry cap; further unique read-through mocks will not be staged",
				zap.Int("entries", len(s.order)),
				zap.Int("max_entries", capEntries),
				zap.String("next_step", "reduce recording duration or split test sets if more than 4096 distinct cache-warming reads are needed per session"))
		}
		return
	}
	s.byKey[key] = mk
	s.respDigest[key] = digest
	if mk.Name != "" {
		s.byName[mk.Name] = mk
	}
	s.order = append(s.order, mk)
}

func hasReadThroughTag(mk *models.Mock) bool {
	return mk != nil && mk.Spec.Metadata != nil &&
		mk.Spec.Metadata[models.MetadataReadThroughKey] == models.MetadataReadThroughValue
}

// extractReadThrough partitions out read-through-tagged mocks from slice in,
// returning the remaining non-read-through mocks and appending valid
// read-through mocks (with precomputed canonical keys) to out. Any mock tagged
// readThrough="true" that fails ReadThroughKey (e.g. a hand-corrupted write
// mock) is dropped fail-closed rather than leaking into filtered/startup trees.
// Read-through mocks never enter per-test or session/config trees so per-test
// precedence and handshake pools stay intact.
func extractReadThrough(in []*models.Mock, out *[]readThroughStaged) []*models.Mock {
	if len(in) == 0 {
		return in
	}
	hasRT := false
	for _, mk := range in {
		if hasReadThroughTag(mk) {
			hasRT = true
			break
		}
	}
	if !hasRT {
		return in
	}
	rest := make([]*models.Mock, 0, len(in))
	for _, mk := range in {
		if hasReadThroughTag(mk) {
			if key, ok := models.ReadThroughKey(mk); ok {
				*out = append(*out, readThroughStaged{mock: mk, key: key})
			}
			continue
		}
		rest = append(rest, mk)
	}
	return rest
}

// MatchReadThrough serves a deduplicated read-only session read-through mock
// strictly by exact canonical request key match (models.ReadThroughRequestKey).
//
// Invariants:
//   - Writes / non-read-only calls NEVER match (ReadThroughRequestKey returns false).
//   - Strictly exact match: different sort, projection, filter, path, or query
//     produces a different canonical key and returns (nil, false).
//   - Reusable: never deletes/evicts the matched entry so subsequent tests or
//     workers on a cold replay pod can also warm their caches from it.
//   - Records usage with Usage=models.Updated and ReadThrough=true so
//     RemoveUnusedMocks keeps it while mappings.yaml and per-test dependency
//     assertions never claim ownership of it.
func (m *MockManager) MatchReadThrough(live *models.Mock) (*models.Mock, bool) {
	if m == nil || live == nil {
		return nil, false
	}
	key, ok := models.ReadThroughRequestKey(live)
	if !ok {
		return nil, false
	}
	m.treesMu.RLock()
	mk := m.readThrough.byKey[key]
	m.treesMu.RUnlock()
	if mk == nil || m.stalePooled(mk) {
		return nil, false
	}
	served := mk.DeepCopy()
	if err := served.HydrateResponse(); err != nil {
		if m.logger != nil {
			m.logger.Warn("read-through mock response hydration failed",
				zap.String("mockName", mk.Name), zap.Error(err))
		}
		return nil, false
	}
	m.recordReadThroughHit(mk, true)
	return served, true
}

// isReadThroughMock reports whether mk belongs to the staged read-through tier.
// Fast-paths on metadata tag so ordinary mocks take zero locks, then verifies
// membership in m.readThrough.byName so hand-tagged unstaged mocks are rejected.
func (m *MockManager) isReadThroughMock(mk *models.Mock) bool {
	if m == nil || mk == nil {
		return false
	}
	if !hasReadThroughTag(mk) {
		return false
	}
	return m.isReadThroughMockName(mk.Name)
}

func (m *MockManager) isReadThroughMockName(name string) bool {
	if m == nil || name == "" {
		return false
	}
	m.treesMu.RLock()
	_, ok := m.readThrough.byName[name]
	m.treesMu.RUnlock()
	return ok
}

// recordReadThroughHit flags a reusable hit on a read-through mock without
// evicting it from the tier. observe must be false when called inside a
// consume path (exactConsumeIn / deleteStartupTier) whose caller already
// invokes observeServed, preventing double observation.
func (m *MockManager) recordReadThroughHit(mk *models.Mock, observe bool) {
	if mk == nil || mk.Name == "" {
		return
	}
	_ = m.flagMockAsUsed(models.MockState{
		Name:             mk.Name,
		Kind:             mk.Kind,
		Usage:            models.Updated,
		IsFiltered:       false,
		SortOrder:        mk.TestModeInfo.SortOrder,
		Type:             mk.Spec.Metadata["type"],
		DataPlane:        dataPlaneTyped(mk),
		ReadThrough:      true,
		Lifetime:         mk.TestModeInfo.Lifetime,
		ReqTimestampMock: models.FormatMockTimestamp(mk.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mk.Spec.ResTimestampMock),
	}, mk.TestModeInfo.StagedSet)
	if observe {
		m.observeServed(mk, nil)
	}
}

func (h *heldScope) MatchReadThrough(live *models.Mock) (*models.Mock, bool) {
	if h.moved() {
		return nil, false
	}
	return h.MockManager.MatchReadThrough(live)
}
