package proxy

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// ---------------- MockManager (kind-aware) ----------------

type MockManager struct {
	// legacy "all" trees (kept for compatibility with existing callers)
	filtered   *TreeDb
	unfiltered *TreeDb

	// global revision (legacy)
	rev uint64

	// NEW: per-kind revisions
	revMu     sync.RWMutex
	revByKind map[models.Kind]*uint64

	// NEW: per-kind trees (guarded by treesMu)
	treesMu          sync.RWMutex
	filteredByKind   map[models.Kind]*TreeDb
	unfilteredByKind map[models.Kind]*TreeDb

	logger *zap.Logger

	// consumedMu guards consumedList and consumedIndex.
	// consumedList records MockState entries in the order they were first
	// intercepted from the network (first call to flagMockAsUsed wins the
	// position; subsequent calls for the same name update the state in-place
	// without changing order).
	consumedMu    sync.Mutex
	consumedList  []models.MockState
	consumedIndex map[string]int

	// Optimized lookup maps
	statelessFiltered   map[models.Kind]map[string][]*models.Mock
	statelessUnfiltered map[models.Kind]map[string][]*models.Mock
}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	if filtered == nil {
		filtered = NewTreeDb(customComparator)
	}
	if unfiltered == nil {
		unfiltered = NewTreeDb(customComparator)
	}
	return &MockManager{
		filtered:            filtered,
		unfiltered:          unfiltered,
		filteredByKind:      make(map[models.Kind]*TreeDb),
		unfilteredByKind:    make(map[models.Kind]*TreeDb),
		statelessFiltered:   make(map[models.Kind]map[string][]*models.Mock),
		statelessUnfiltered: make(map[models.Kind]map[string][]*models.Mock),
		revByKind:           make(map[models.Kind]*uint64),
		consumedIndex:       make(map[string]int),
		logger:              logger,
	}
}

func (m *MockManager) GetStatelessMocks(kind models.Kind, key string) (filtered, unfiltered []*models.Mock) {
	if kind == models.DNS {
		key = strings.ToLower(dns.Fqdn(key))
	}
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	if km, ok := m.statelessFiltered[kind]; ok {
		if list := km[key]; len(list) > 0 {
			filtered = append([]*models.Mock(nil), list...)
		}
	}
	if km, ok := m.statelessUnfiltered[kind]; ok {
		if list := km[key]; len(list) > 0 {
			unfiltered = append([]*models.Mock(nil), list...)
		}
	}
	return
}

// ---------- revision helpers ----------

func (m *MockManager) Revision() uint64 {
	return atomic.LoadUint64(&m.rev)
}

func (m *MockManager) bumpRevisionAll() {
	atomic.AddUint64(&m.rev, 1)
}

func (m *MockManager) RevisionByKind(kind models.Kind) uint64 {
	m.revMu.RLock()
	ptr := m.revByKind[kind]
	m.revMu.RUnlock()
	if ptr == nil {
		return 0
	}
	return atomic.LoadUint64(ptr)
}

func (m *MockManager) bumpRevisionKind(kind models.Kind) {
	m.revMu.Lock()
	ptr := m.revByKind[kind]
	if ptr == nil {
		var v uint64
		// Store pointer in map; safe to use after unlocking as we mutate via atomics.
		ptr = &v
		m.revByKind[kind] = ptr
	}
	m.revMu.Unlock()
	atomic.AddUint64(ptr, 1)
}

// ensureKindTrees returns per-kind trees, creating them if missing.
// It is safe for concurrent use.
func (m *MockManager) ensureKindTrees(kind models.Kind) (f *TreeDb, u *TreeDb) {
	// Fast path: read lock
	m.treesMu.RLock()
	f = m.filteredByKind[kind]
	u = m.unfilteredByKind[kind]
	m.treesMu.RUnlock()
	if f != nil && u != nil {
		return f, u
	}

	// Slow path: upgrade to write lock and double-check
	m.treesMu.Lock()
	if f = m.filteredByKind[kind]; f == nil {
		f = NewTreeDb(customComparator)
		m.filteredByKind[kind] = f
	}
	if u = m.unfilteredByKind[kind]; u == nil {
		u = NewTreeDb(customComparator)
		m.unfilteredByKind[kind] = u
	}
	m.treesMu.Unlock()
	return f, u
}

// ---------- getters ----------

func (m *MockManager) GetFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock()
	tree := m.filtered
	m.treesMu.RUnlock()
	results := make([]*models.Mock, 0, 64)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock()
	tree := m.unfiltered
	m.treesMu.RUnlock()
	results := make([]*models.Mock, 0, 128)
	tree.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// NEW: kind-scoped getters used by Redis matcher
func (m *MockManager) GetFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	// Fetch pointer safely; the tree itself is responsible for its own safety.
	m.treesMu.RLock()
	flt := m.filteredByKind[kind]
	m.treesMu.RUnlock()
	if flt == nil {
		flt, _ = m.ensureKindTrees(kind)
	}

	results := make([]*models.Mock, 0, 64)
	flt.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

func (m *MockManager) GetUnFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	m.treesMu.RLock()
	unf := m.unfilteredByKind[kind]
	m.treesMu.RUnlock()
	if unf == nil {
		_, unf = m.ensureKindTrees(kind)
	}

	results := make([]*models.Mock, 0, 128)
	unf.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// ---------- setters (populate both legacy + per-kind) ----------

func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	newFiltered := NewTreeDb(customComparator)
	newFilteredByKind := make(map[models.Kind]*TreeDb)
	newStateless := make(map[models.Kind]map[string][]*models.Mock)
	touched := map[models.Kind]struct{}{}
	var maxSortOrder int64
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		if mock.TestModeInfo.SortOrder > maxSortOrder {
			maxSortOrder = mock.TestModeInfo.SortOrder
		}
		mock.TestModeInfo.ID = index
		newFiltered.insert(mock.TestModeInfo, mock)
		k := mock.Kind
		td := newFilteredByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newFilteredByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
		if newStateless[k] == nil {
			newStateless[k] = make(map[string][]*models.Mock)
		}
		key := mock.Name
		if mock.Kind == models.DNS && mock.Spec.DNSReq != nil {
			key = strings.ToLower(dns.Fqdn(mock.Spec.DNSReq.Name))
		}
		newStateless[k][key] = append(newStateless[k][key], mock)
	}
	if maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(maxSortOrder)
	}
	m.treesMu.Lock()
	m.filtered, m.filteredByKind, m.statelessFiltered = newFiltered, newFilteredByKind, newStateless
	m.treesMu.Unlock()
	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	newUnFiltered := NewTreeDb(customComparator)
	newUnFilteredByKind := make(map[models.Kind]*TreeDb)
	newStateless := make(map[models.Kind]map[string][]*models.Mock)
	touched := map[models.Kind]struct{}{}
	var maxSortOrder int64
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		if mock.TestModeInfo.SortOrder > maxSortOrder {
			maxSortOrder = mock.TestModeInfo.SortOrder
		}
		mock.TestModeInfo.ID = index
		newUnFiltered.insert(mock.TestModeInfo, mock)
		k := mock.Kind
		td := newUnFilteredByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newUnFilteredByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
		if newStateless[k] == nil {
			newStateless[k] = make(map[string][]*models.Mock)
		}
		key := mock.Name
		if mock.Kind == models.DNS && mock.Spec.DNSReq != nil {
			key = strings.ToLower(dns.Fqdn(mock.Spec.DNSReq.Name))
		}
		newStateless[k][key] = append(newStateless[k][key], mock)
	}
	if maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(maxSortOrder)
	}
	m.treesMu.Lock()
	m.unfiltered, m.unfilteredByKind, m.statelessUnfiltered = newUnFiltered, newUnFilteredByKind, newStateless
	m.treesMu.Unlock()
	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

// ---------- point updates / deletes (keep per-kind in sync) ----------

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	// Snapshot the legacy tree pointer safely
	m.treesMu.RLock()
	globalTree := m.unfiltered
	m.treesMu.RUnlock()
	// Update legacy/global tree first
	updatedGlobal := globalTree.update(old.TestModeInfo, new.TestModeInfo, new)

	oldK, newK := old.Kind, new.Kind
	var updatedOldKind, updatedNewKind bool

	if oldK == newK {
		// Same kind: update the per-kind tree under lock
		_, unf := m.ensureKindTrees(newK)
		m.treesMu.Lock()
		updatedNewKind = unf.update(old.TestModeInfo, new.TestModeInfo, new)

		// Self-heal if global updated but per-kind missed (e.g., not present yet)
		if updatedGlobal && !updatedNewKind {
			if m.logger != nil {
				m.logger.Warn("self-healing per-kind tree: global update succeeded but per-kind missed",
					zap.String("kind", string(newK)),
					zap.String("mockName", new.Name),
					zap.Any("testModeInfo", new.TestModeInfo),
				)
			}
			unf.insert(new.TestModeInfo, new)
			updatedNewKind = true
		}
		m.treesMu.Unlock()
	} else {
		// Kind changed: remove from old kind tree, insert/update in new kind tree under one lock
		_, oldUnf := m.ensureKindTrees(oldK)
		_, newUnf := m.ensureKindTrees(newK)
		m.treesMu.Lock()
		updatedOldKind = oldUnf.delete(old.TestModeInfo)
		updatedNewKind = newUnf.update(old.TestModeInfo, new.TestModeInfo, new)
		if !updatedNewKind {
			newUnf.insert(new.TestModeInfo, new)
			updatedNewKind = true
		}
		m.treesMu.Unlock()
		if m.logger != nil {
			m.logger.Info("moved mock across kinds",
				zap.String("mockName", new.Name),
				zap.String("fromKind", string(oldK)),
				zap.String("toKind", string(newK)),
			)
		}
	}

	// Mark usage if global changed (legacy behavior)
	if updatedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             new.Name,
			Kind:             new.Kind,
			Usage:            models.Updated,
			IsFiltered:       new.TestModeInfo.IsFiltered,
			SortOrder:        new.TestModeInfo.SortOrder,
			Type:             new.Spec.Metadata["type"],
			ReqTimestampMock: models.FormatMockTimestamp(new.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(new.Spec.ResTimestampMock),
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}

	// Bump revisions accurately:
	// - global only if the global tree changed
	// - per-kind only for kinds whose per-kind tree changed
	if oldK != newK {
		if updatedOldKind {
			m.bumpRevisionKind(oldK)
		}
		if updatedNewKind {
			m.bumpRevisionKind(newK)
		}
	} else if updatedNewKind {
		m.bumpRevisionKind(newK)
	}
	if updatedGlobal {
		m.bumpRevisionAll()
	}
	return updatedGlobal
}

func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	m.treesMu.RLock()
	globalTree := m.filtered
	m.treesMu.RUnlock()
	deletedGlobal := globalTree.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	flt, _ := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := flt.delete(mock.TestModeInfo)
	m.treesMu.Unlock()

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             mock.Name,
			Kind:             mock.Kind,
			Usage:            models.Deleted,
			IsFiltered:       mock.TestModeInfo.IsFiltered,
			SortOrder:        mock.TestModeInfo.SortOrder,
			Type:             mock.Spec.Metadata["type"],
			ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}

	// Bump per-kind only if that tree changed; global only if global changed
	if deletedKind {
		m.bumpRevisionKind(k)
	}
	if deletedGlobal {
		m.bumpRevisionAll()
	}
	return deletedGlobal
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	m.treesMu.RLock()
	globalTree := m.unfiltered
	m.treesMu.RUnlock()
	deletedGlobal := globalTree.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	_, unf := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := unf.delete(mock.TestModeInfo)
	m.treesMu.Unlock()

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:             mock.Name,
			Kind:             mock.Kind,
			Usage:            models.Deleted,
			IsFiltered:       mock.TestModeInfo.IsFiltered,
			SortOrder:        mock.TestModeInfo.SortOrder,
			Type:             mock.Spec.Metadata["type"],
			ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
			ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}

	// Bump per-kind only if that tree changed; global only if global changed
	if deletedKind {
		m.bumpRevisionKind(k)
	}
	if deletedGlobal {
		m.bumpRevisionAll()
	}
	return deletedGlobal
}

// MarkMockAsUsed marks the given mock as used (consumed) without modifying
// its sort order or removing it from any tree. This is intended for parsers
// (e.g. mongo v2) that need to record mock usage without changing mock ordering.
func (m *MockManager) MarkMockAsUsed(mock models.Mock) bool {
	if mock.Name == "" {
		return false
	}
	if err := m.flagMockAsUsed(models.MockState{
		Name:             mock.Name,
		Kind:             mock.Kind,
		Usage:            models.Updated,
		IsFiltered:       mock.TestModeInfo.IsFiltered,
		SortOrder:        mock.TestModeInfo.SortOrder,
		Type:             mock.Spec.Metadata["type"],
		ReqTimestampMock: models.FormatMockTimestamp(mock.Spec.ReqTimestampMock),
		ResTimestampMock: models.FormatMockTimestamp(mock.Spec.ResTimestampMock),
	}); err != nil {
		if m.logger != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
		return false
	}
	return true
}

// ---------- bookkeeping ----------

// flagMockAsUsed records that a mock was consumed from the network.
// The first call for a given name establishes its position in the consumption
// order; subsequent calls for the same name update the stored state in-place
// without changing its position. This preserves true network call order in
// GetConsumedMocks.
func (m *MockManager) flagMockAsUsed(mock models.MockState) error {
	if mock.Name == "" {
		return fmt.Errorf("mock is empty")
	}
	m.consumedMu.Lock()
	if idx, exists := m.consumedIndex[mock.Name]; exists {
		m.consumedList[idx] = mock // update state, preserve position
	} else {
		m.consumedIndex[mock.Name] = len(m.consumedList)
		m.consumedList = append(m.consumedList, mock)
	}
	m.consumedMu.Unlock()
	return nil
}

// GetConsumedMocks returns and drains the list of mocks that were consumed
// since the last call, in the order they were first intercepted from the
// network.
func (m *MockManager) GetConsumedMocks() []models.MockState {
	m.consumedMu.Lock()
	out := append([]models.MockState(nil), m.consumedList...)
	m.consumedList = m.consumedList[:0]
	m.consumedIndex = make(map[string]int)
	m.consumedMu.Unlock()
	return out
}

// GetMySQLCounts computes counts of MySQL mocks.
// Uses the per-kind unfiltered tree if available, otherwise falls back
// to scanning the legacy unfiltered tree.
func (m *MockManager) GetMySQLCounts() (total, config, data int) {
	// Fast path: snapshot the per-kind tree pointer under lock
	m.treesMu.RLock()
	tree := m.unfilteredByKind[models.MySQL]
	m.treesMu.RUnlock()

	if tree != nil {
		tree.rangeValues(func(v interface{}) bool {
			mock, ok := v.(*models.Mock)
			if !ok || mock == nil {
				return true
			}
			total++
			if mock.Spec.Metadata["type"] == "config" {
				config++
			} else {
				data++
			}
			return true
		})
		return
	}

	// Fallback: legacy scan of the combined tree
	m.treesMu.RLock()
	legacyTree := m.unfiltered
	m.treesMu.RUnlock()
	legacyTree.rangeValues(func(v interface{}) bool {
		mock, ok := v.(*models.Mock)
		if !ok || mock == nil || mock.Kind != models.MySQL {
			return true
		}
		total++
		if mock.Spec.Metadata["type"] == "config" {
			config++
		} else {
			data++
		}
		return true
	})
	return
}
