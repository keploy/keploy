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
	revMu sync.RWMutex

	// NEW: per-kind trees
	filteredByKind   map[models.Kind]*TreeDb
	unfilteredByKind map[models.Kind]*TreeDb
	revByKind        map[models.Kind]*uint64

	logger        *zap.Logger
	consumedMocks sync.Map
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
		logger:           logger,
		revByKind:        make(map[models.Kind]*uint64),
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
		ptr = &v
		m.revByKind[kind] = ptr
	}
	m.revMu.Unlock()
	atomic.AddUint64(ptr, 1)
}
func (m *MockManager) ensureKindTrees(kind models.Kind) (f *TreeDb, u *TreeDb) {
	if t := m.filteredByKind[kind]; t == nil {
		m.filteredByKind[kind] = NewTreeDb(customComparator)
	}
	if t := m.unfilteredByKind[kind]; t == nil {
		m.unfilteredByKind[kind] = NewTreeDb(customComparator)
	}
	return m.filteredByKind[kind], m.unfilteredByKind[kind]
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
	flt, _ := m.ensureKindTrees(kind)
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
	_, unf := m.ensureKindTrees(kind)
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
	m.filtered.deleteAll()
	// we will track which kinds were touched to bump only their revisions
	touched := map[models.Kind]struct{}{}
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		mock.TestModeInfo.ID = index
		m.filtered.insert(mock.TestModeInfo, mock)

		// per-kind
		k := mock.Kind
		flt, _ := m.ensureKindTrees(k)
		flt.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
	}
	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	m.unfiltered.deleteAll()
	touched := map[models.Kind]struct{}{}
	for index, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 {
			mock.TestModeInfo.SortOrder = int64(index) + 1
		}
		mock.TestModeInfo.ID = index
		m.unfiltered.insert(mock.TestModeInfo, mock)

		// per-kind
		k := mock.Kind
		_, unf := m.ensureKindTrees(k)
		unf.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}
	}
	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

// ---------- point updates / deletes (keep per-kind in sync) ----------

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	updated := m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new)
	// per-kind
	k := new.Kind
	_, unf := m.ensureKindTrees(k)
	_ = unf.update(old.TestModeInfo, new.TestModeInfo, new)

	if updated {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       (*new).Name,
			Usage:      models.Updated,
			IsFiltered: (*new).TestModeInfo.IsFiltered,
			SortOrder:  (*new).TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
	m.bumpRevisionKind(k)
	m.bumpRevisionAll()
	return updated
}

func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	isDeleted := m.filtered.delete(mock.TestModeInfo)
	// per-kind
	k := mock.Kind
	flt, _ := m.ensureKindTrees(k)
	_ = flt.delete(mock.TestModeInfo)

	if isDeleted {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
	m.bumpRevisionKind(k)
	m.bumpRevisionAll()
	return isDeleted
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	isDeleted := m.unfiltered.delete(mock.TestModeInfo)
	// per-kind
	k := mock.Kind
	_, unf := m.ensureKindTrees(k)
	_ = unf.delete(mock.TestModeInfo)

	if isDeleted {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
		}); err != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
	}
	m.bumpRevisionKind(k)
	m.bumpRevisionAll()
	return isDeleted
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
	var keys []models.MockState
	m.consumedMocks.Range(func(key, val interface{}) bool {
		if _, ok := key.(string); ok {
			keys = append(keys, val.(models.MockState))
			m.consumedMocks.Delete(key)
		}
		return true
	})
	sort.Slice(keys, func(i, j int) bool {
		numI, _ := strconv.Atoi(strings.Split(keys[i].Name, "-")[1])
		numJ, _ := strconv.Atoi(strings.Split(keys[j].Name, "-")[1])
		return numI < numJ
	})
	for key := range keys {
		m.consumedMocks.Delete(key)
	}
	return keys
}

// GetMySQLCounts computes counts of MySQL mocks.
// Uses the per-kind unfiltered tree if available, otherwise falls back
// to scanning the legacy unfiltered tree.
func (m *MockManager) GetMySQLCounts() (total, config, data int) {
	// Fast path: per-kind tree present
	if t := m.unfilteredByKind[models.MySQL]; t != nil {
		t.rangeValues(func(v interface{}) bool {
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
