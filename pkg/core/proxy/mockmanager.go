//go:build linux

package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v2/pkg/models"
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

	logger        *zap.Logger
	consumedMocks sync.Map // zero value is ready-to-use; no explicit init required
}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	if filtered == nil {
		filtered = NewTreeDb(customComparator)
	}
	if unfiltered == nil {
		unfiltered = NewTreeDb(customComparator)
	}
	return &MockManager{
		filtered:         filtered,
		unfiltered:       unfiltered,
		filteredByKind:   make(map[models.Kind]*TreeDb),
		unfilteredByKind: make(map[models.Kind]*TreeDb),
		revByKind:        make(map[models.Kind]*uint64),
		logger:           logger,
	}
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
		ptr = &v // escapes; safe after unlock
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
	results := make([]*models.Mock, 0, 64)
	m.filtered.rangeValues(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	results := make([]*models.Mock, 0, 128)
	m.unfiltered.rangeValues(func(v interface{}) bool {
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
	// legacy rebuild
	m.filtered.deleteAll()

	// rebuild per-kind filtered maps from scratch to avoid stale entries
	newFilteredByKind := make(map[models.Kind]*TreeDb, len(m.filteredByKind))
	touched := map[models.Kind]struct{}{}

	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		mock.TestModeInfo.ID = index
		m.filtered.insert(mock.TestModeInfo, mock)

		k := mock.Kind
		td := newFilteredByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newFilteredByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
	}

	// atomically swap the per-kind map
	m.treesMu.Lock()
	m.filteredByKind = newFilteredByKind
	m.treesMu.Unlock()

	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	// legacy rebuild
	m.unfiltered.deleteAll()

	// rebuild per-kind unfiltered maps from scratch to avoid stale entries
	newUnfilteredByKind := make(map[models.Kind]*TreeDb, len(m.unfilteredByKind))
	touched := map[models.Kind]struct{}{}

	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		mock.TestModeInfo.ID = index
		m.unfiltered.insert(mock.TestModeInfo, mock)

		k := mock.Kind
		td := newUnfilteredByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newUnfilteredByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
	}

	// atomically swap the per-kind map
	m.treesMu.Lock()
	m.unfilteredByKind = newUnfilteredByKind
	m.treesMu.Unlock()

	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

// ---------- point updates / deletes (keep per-kind in sync) ----------

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	// Update legacy/global tree
	updatedGlobal := m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new)

	// Update per-kind tree
	k := new.Kind
	_, unf := m.ensureKindTrees(k)
	updatedKind := unf.update(old.TestModeInfo, new.TestModeInfo, new)

	// If global updated but per-kind missed (e.g., not present), try to self-heal
	if updatedGlobal && !updatedKind {
		unf.insert(new.TestModeInfo, new)
		updatedKind = true
	}

	// Mark usage if global changed (legacy behavior)
	if updatedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       new.Name,
			Usage:      models.Updated,
			IsFiltered: new.TestModeInfo.IsFiltered,
			SortOrder:  new.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}

	// Only bump if *something* actually changed
	if updatedGlobal || updatedKind {
		m.bumpRevisionKind(k)
		m.bumpRevisionAll()
	}
	return updatedGlobal
}

func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	deletedGlobal := m.filtered.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	flt, _ := m.ensureKindTrees(k)
	deletedKind := flt.delete(mock.TestModeInfo)

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}

	if deletedGlobal || deletedKind {
		m.bumpRevisionKind(k)
		m.bumpRevisionAll()
	}
	return deletedGlobal
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	deletedGlobal := m.unfiltered.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	_, unf := m.ensureKindTrees(k)
	deletedKind := unf.delete(mock.TestModeInfo)

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}

	if deletedGlobal || deletedKind {
		m.bumpRevisionKind(k)
		m.bumpRevisionAll()
	}
	return deletedGlobal
}

// ---------- bookkeeping ----------

func (m *MockManager) flagMockAsUsed(mock models.MockState) error {
	if mock.Name == "" {
		return fmt.Errorf("mock is empty")
	}
	m.consumedMocks.Store(mock.Name, mock)
	return nil
}

func (m *MockManager) GetConsumedMocks() []models.MockState {
	var out []models.MockState
	// Collect first (no deletes during Range)
	m.consumedMocks.Range(func(key, val interface{}) bool {
		if _, ok := key.(string); !ok {
			return true
		}
		if st, ok := val.(models.MockState); ok {
			out = append(out, st)
		}
		return true
	})

	// Sort: prefer numeric suffix after '-', else lexicographic
	sort.SliceStable(out, func(i, j int) bool {
		partsI := strings.Split(out[i].Name, "-")
		partsJ := strings.Split(out[j].Name, "-")
		if len(partsI) >= 2 && len(partsJ) >= 2 {
			if numI, errI := strconv.Atoi(partsI[1]); errI == nil {
				if numJ, errJ := strconv.Atoi(partsJ[1]); errJ == nil {
					return numI < numJ
				}
			}
		}
		return out[i].Name < out[j].Name
	})

	// Now clear those entries
	for _, st := range out {
		m.consumedMocks.Delete(st.Name)
	}
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
	m.unfiltered.rangeValues(func(v interface{}) bool {
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
