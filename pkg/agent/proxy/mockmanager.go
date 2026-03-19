package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// MockManager manages mocks using red-black trees and specialized maps for lookups.
type MockManager struct {
	// legacy "all" trees (kept for compatibility)
	filtered   *TreeDb
	unfiltered *TreeDb

	// global revision (legacy)
	rev uint64

	// NEW: per-kind revisions
	revMu     sync.RWMutex
	revByKind map[models.Kind]*uint64

	// NEW: per-kind trees and maps (guarded by treesMu)
	treesMu          sync.RWMutex
	filteredByKind   map[models.Kind]*TreeDb
	unfilteredByKind map[models.Kind]*TreeDb

	// High-speed lookup by Kind + Key (guarded by treesMu)
	lookupFiltered   map[models.Kind]map[string][]*models.Mock
	lookupUnfiltered map[models.Kind]map[string][]*models.Mock

	logger        *zap.Logger
	consumedMocks sync.Map
}

// NewMockManager initializes a new MockManager with the provided trees.
// NewMockManager initializes a new MockManager with the provided trees and maps.
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
		lookupFiltered:   make(map[models.Kind]map[string][]*models.Mock),
		lookupUnfiltered: make(map[models.Kind]map[string][]*models.Mock),
		revByKind:        make(map[models.Kind]*uint64),
		logger:           logger,
	}
}

// getLookupKey generates a high-speed search key for a mock based on its kind.
func getLookupKey(mock *models.Mock) string {
	if mock == nil {
		return ""
	}
	switch mock.Kind {
	case models.DNS:
		if mock.Spec.DNSReq != nil {
			return strings.ToLower(dns.Fqdn(mock.Spec.DNSReq.Name))
		}
	case models.HTTP:
		if mock.Spec.HTTPReq != nil {
			return string(mock.Spec.HTTPReq.Method) + " " + mock.Spec.HTTPReq.URL
		}
	}
	return mock.Name
}

// GetMocks performs an O(1) hashmap lookup to find candidate mocks for replaying.
func (m *MockManager) GetMocks(kind models.Kind, key string) (filtered, unfiltered []*models.Mock) {
	if kind == models.DNS {
		key = strings.ToLower(dns.Fqdn(key))
	}
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()

	if km, ok := m.lookupFiltered[kind]; ok {
		filtered = km[key]
	}
	if km, ok := m.lookupUnfiltered[kind]; ok {
		unfiltered = km[key]
	}
	return
}

// ---------- revision helpers ----------

// Revision returns the global revision counter.
func (m *MockManager) Revision() uint64 {
	return atomic.LoadUint64(&m.rev)
}

func (m *MockManager) bumpRevisionAll() {
	atomic.AddUint64(&m.rev, 1)
}

// RevisionByKind returns the revision counter for a specific mock kind.
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
	defer m.treesMu.Unlock()
	if f = m.filteredByKind[kind]; f == nil {
		f = NewTreeDb(customComparator)
		m.filteredByKind[kind] = f
	}
	if u = m.unfilteredByKind[kind]; u == nil {
		u = NewTreeDb(customComparator)
		m.unfilteredByKind[kind] = u
	}
	return f, u
}

// ---------- getters ----------

// GetFilteredMocks returns all filtered mocks.
func (m *MockManager) GetFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	results := make([]*models.Mock, 0, 64)
	m.filtered.rangeValuesNoLock(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// GetUnFilteredMocks returns all unfiltered mocks.
func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	results := make([]*models.Mock, 0, 128)
	m.unfiltered.rangeValuesNoLock(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil {
			results = append(results, mock)
		}
		return true
	})
	return results, nil
}

// GetFilteredMocksByKind returns filtered mocks for a specific kind.
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

// GetUnFilteredMocksByKind returns unfiltered mocks for a specific kind.
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

// GetDNSMocks is maintained for backward compatibility with the DNS listener.
func (m *MockManager) GetDNSMocks(name string) (filtered, unfiltered []*models.Mock) {
	return m.GetMocks(models.DNS, name)
}

// SetFilteredMocks builds new trees and lookup maps for the filtered set.
func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	// 1. Build new structures locally to minimize lock time
	newAll := NewTreeDb(customComparator)
	newByKind := make(map[models.Kind]*TreeDb)
	newLookup := make(map[models.Kind]map[string][]*models.Mock)
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

		// Build legacy tree
		newAll.insert(mock.TestModeInfo, mock)

		// Build per-kind tree
		k := mock.Kind
		td := newByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}

		// Build lookup map
		if newLookup[k] == nil {
			newLookup[k] = make(map[string][]*models.Mock)
		}
		key := getLookupKey(mock)
		newLookup[k][key] = append(newLookup[k][key], mock)
	}

	// Sort lookup slices for determinism
	for _, km := range newLookup {
		for _, slice := range km {
			sortDNSList(slice)
		}
	}

	if maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(maxSortOrder)
	}

	// 2. Atomically swap current state
	m.treesMu.Lock()
	m.filtered = newAll
	m.filteredByKind = newByKind
	m.lookupFiltered = newLookup
	m.treesMu.Unlock()

	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

// SetUnFilteredMocks builds new trees and lookup maps for the unfiltered set.
func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	newAll := NewTreeDb(customComparator)
	newByKind := make(map[models.Kind]*TreeDb)
	newLookup := make(map[models.Kind]map[string][]*models.Mock)
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

		newAll.insert(mock.TestModeInfo, mock)

		k := mock.Kind
		td := newByKind[k]
		if td == nil {
			td = NewTreeDb(customComparator)
			newByKind[k] = td
		}
		td.insert(mock.TestModeInfo, mock)
		touched[k] = struct{}{}

		if newLookup[k] == nil {
			newLookup[k] = make(map[string][]*models.Mock)
		}
		key := getLookupKey(mock)
		newLookup[k][key] = append(newLookup[k][key], mock)
	}

	for _, km := range newLookup {
		for _, slice := range km {
			sortDNSList(slice)
		}
	}

	if maxSortOrder > 0 {
		pkg.UpdateSortCounterIfHigher(maxSortOrder)
	}

	m.treesMu.Lock()
	m.unfiltered = newAll
	m.unfilteredByKind = newByKind
	m.lookupUnfiltered = newLookup
	m.treesMu.Unlock()

	for k := range touched {
		m.bumpRevisionKind(k)
	}
	m.bumpRevisionAll()
}

// ---------- point updates / deletes (keep per-kind in sync) ----------

// UpdateUnFilteredMock updates an existing unfiltered mock or inserts it if not found.
func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()

	// Update legacy
	updatedGlobal := m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new)
	if !updatedGlobal {
		return false
	}

	// Update per-kind
	if td, ok := m.unfilteredByKind[new.Kind]; ok {
		td.update(old.TestModeInfo, new.TestModeInfo, new)
	}

	// Update lookup map
	m.removeFromLookupMap(old, false)
	m.addToLookupMap(new, false)

	return true
}

// DeleteFilteredMock removes a mock from the filtered trees and maps.
func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()

	deletedGlobal := m.filtered.delete(mock.TestModeInfo)
	if !deletedGlobal {
		return false
	}

	if td, ok := m.filteredByKind[mock.Kind]; ok {
		td.delete(mock.TestModeInfo)
	}

	m.removeFromLookupMap(&mock, true)
	return true
}

// DeleteUnFilteredMock removes a mock from the unfiltered trees and maps.
func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()

	deletedGlobal := m.unfiltered.delete(mock.TestModeInfo)
	if !deletedGlobal {
		return false
	}

	if td, ok := m.unfilteredByKind[mock.Kind]; ok {
		td.delete(mock.TestModeInfo)
	}

	m.removeFromLookupMap(&mock, false)
	return true
}

func (m *MockManager) addToLookupMap(mock *models.Mock, isFiltered bool) {
	key := getLookupKey(mock)
	k := mock.Kind
	var target map[models.Kind]map[string][]*models.Mock
	if isFiltered {
		target = m.lookupFiltered
	} else {
		target = m.lookupUnfiltered
	}

	if target[k] == nil {
		target[k] = make(map[string][]*models.Mock)
	}
	target[k][key] = append(target[k][key], mock)
	sortDNSList(target[k][key])
}

func (m *MockManager) removeFromLookupMap(mock *models.Mock, isFiltered bool) {
	key := getLookupKey(mock)
	k := mock.Kind
	var target map[models.Kind]map[string][]*models.Mock
	if isFiltered {
		target = m.lookupFiltered
	} else {
		target = m.lookupUnfiltered
	}

	km := target[k]
	if km == nil {
		return
	}
	list := km[key]
	for i, item := range list {
		if item.Name == mock.Name && item.TestModeInfo.ID == mock.TestModeInfo.ID {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(km, key)
	} else {
		km[key] = list
	}
}

func sortDNSList(list []*models.Mock) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].TestModeInfo.SortOrder != list[j].TestModeInfo.SortOrder {
			return list[i].TestModeInfo.SortOrder < list[j].TestModeInfo.SortOrder
		}
		return list[i].TestModeInfo.ID < list[j].TestModeInfo.ID
	})
}

// MarkMockAsUsed flags the mock.
func (m *MockManager) MarkMockAsUsed(mock models.Mock) bool {
	if mock.Name == "" {
		return false
	}
	m.flagMockAsUsed(models.MockState{
		Name:       mock.Name,
		Kind:       mock.Kind,
		Usage:      models.Updated,
		IsFiltered: mock.TestModeInfo.IsFiltered,
		SortOrder:  mock.TestModeInfo.SortOrder,
		Type:       mock.Spec.Metadata["type"],
		Timestamp:  mock.Spec.ReqTimestampMock.Unix(),
	})
	return true
}

// ---------- bookkeeping ----------
func (m *MockManager) flagMockAsUsed(mock models.MockState) error {
	if mock.Name == "" {
		return fmt.Errorf("mock is empty")
	}
	m.consumedMocks.Store(mock.Name, mock)
	return nil
}

// GetConsumedMocks returns the consumed mocks and clears the internal storage.
func (m *MockManager) GetConsumedMocks() []models.MockState {
	var out []models.MockState

	// Snapshot & collect first (no deletes during Range). We intentionally drain only what existed at snapshot time.
	m.consumedMocks.Range(func(key, val interface{}) bool {
		k, ok := key.(string)
		if !ok {
			if m.logger != nil {
				m.logger.Warn("unexpected key type in consumedMocks; skipping",
					zap.Any("keyType", fmt.Sprintf("%T", key)))
			}
			return true // skip this entry
		}
		if st, ok := val.(models.MockState); ok {
			out = append(out, st)
		} else if m.logger != nil {
			m.logger.Warn("unexpected value type in consumedMocks; skipping",
				zap.String("key", k),
				zap.Any("valueType", fmt.Sprintf("%T", val)))
		}
		return true
	})

	// Sort: prefer numeric suffix after the last '-' (e.g., name-123); else lexicographic
	type withSuffix struct {
		st   models.MockState
		name string
		num  int
		has  bool
	}
	numericSuffix := func(name string) (int, bool) {
		i := strings.LastIndexByte(name, '-')
		if i < 0 || i+1 >= len(name) {
			return 0, false
		}
		n, err := strconv.Atoi(name[i+1:])
		if err != nil {
			return 0, false
		}
		return n, true
	}

	ws := make([]withSuffix, len(out))
	for i, st := range out {
		n, ok := numericSuffix(st.Name)
		ws[i] = withSuffix{st: st, name: st.Name, num: n, has: ok}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		a, b := ws[i], ws[j]
		if a.has && b.has {
			if a.num != b.num {
				return a.num < b.num
			}
			// tie-break numerics by name for determinism
			return a.name < b.name
		}
		return a.name < b.name
	})
	for i := range out {
		out[i] = ws[i].st
	}

	// Now clear those entries from the map we just drained
	for _, st := range out {
		m.consumedMocks.Delete(st.Name)
	}
	return out
}

// GetMySQLCounts computes counts of MySQL mocks.
// Uses the per-kind unfiltered tree if available, otherwise falls back
// to scanning the legacy unfiltered tree.
func (m *MockManager) GetMySQLCounts() (total, config, data int) {
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	m.unfiltered.rangeValuesNoLock(func(v interface{}) bool {
		mock, ok := v.(*models.Mock)
		if ok && mock != nil && mock.Kind == models.MySQL {
			total++
			if mock.Spec.Metadata["type"] == "config" {
				config++
			} else {
				data++
			}
		}
		return true
	})
	return
}
