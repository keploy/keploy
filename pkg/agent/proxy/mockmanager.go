package proxy

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type MockManager struct {
	filtered, unfiltered *TreeDb
	rev                  uint64
	revMu                sync.RWMutex
	revByKind            map[models.Kind]*uint64
	treesMu              sync.RWMutex
	filteredByKind       map[models.Kind]*TreeDb
	unfilteredByKind     map[models.Kind]*TreeDb
	lookupFiltered       map[models.Kind]map[string][]*models.Mock
	lookupUnfiltered     map[models.Kind]map[string][]*models.Mock
	logger               *zap.Logger
	consumedMocks        sync.Map
}

func NewMockManager(filtered, unfiltered *TreeDb, logger *zap.Logger) *MockManager {
	if filtered == nil { filtered = NewTreeDb(customComparator) }
	if unfiltered == nil { unfiltered = NewTreeDb(customComparator) }
	return &MockManager{
		filtered: filtered, unfiltered: unfiltered,
		filteredByKind: make(map[models.Kind]*TreeDb), unfilteredByKind: make(map[models.Kind]*TreeDb),
		lookupFiltered: make(map[models.Kind]map[string][]*models.Mock), lookupUnfiltered: make(map[models.Kind]map[string][]*models.Mock),
		revByKind: make(map[models.Kind]*uint64), logger: logger,
	}
}

func getLookupKey(mock *models.Mock) string {
	if mock == nil { return "" }
	switch mock.Kind {
	case models.DNS:
		if mock.Spec.DNSReq != nil { return strings.ToLower(dns.Fqdn(mock.Spec.DNSReq.Name)) }
	case models.HTTP:
		if mock.Spec.HTTPReq != nil { return string(mock.Spec.HTTPReq.Method) + " " + mock.Spec.HTTPReq.URL }
	}
	return mock.Name
}

func (m *MockManager) GetMocks(kind models.Kind, key string) (filtered, unfiltered []*models.Mock) {
	if kind == models.DNS { key = strings.ToLower(dns.Fqdn(key)) }
	m.treesMu.RLock()
	defer m.treesMu.RUnlock()
	if km, ok := m.lookupFiltered[kind]; ok { filtered = km[key] }
	if km, ok := m.lookupUnfiltered[kind]; ok { unfiltered = km[key] }
	return
}

func (m *MockManager) GetDNSMocks(name string) (filtered, unfiltered []*models.Mock) { return m.GetMocks(models.DNS, name) }

func (m *MockManager) SetFilteredMocks(mocks []*models.Mock) {
	newAll, newByKind, newLookup := NewTreeDb(customComparator), make(map[models.Kind]*TreeDb), make(map[models.Kind]map[string][]*models.Mock)
	var maxSortOrder int64
	for i, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 { mock.TestModeInfo.SortOrder = int64(i) + 1}
		if mock.TestModeInfo.SortOrder > maxSortOrder { maxSortOrder = mock.TestModeInfo.SortOrder }
		mock.TestModeInfo.ID = i
		newAll.insert(mock.TestModeInfo, mock)
		k := mock.Kind
		if newByKind[k] == nil { newByKind[k] = NewTreeDb(customComparator) }
		newByKind[k].insert(mock.TestModeInfo, mock)
		if newLookup[k] == nil { newLookup[k] = make(map[string][]*models.Mock) }
		key := getLookupKey(mock)
		newLookup[k][key] = append(newLookup[k][key], mock)
	}
	for _, km := range newLookup { for _, slice := range km { sortDNSList(slice) } }
	if maxSortOrder > 0 { pkg.UpdateSortCounterIfHigher(maxSortOrder) }
	m.treesMu.Lock()
	m.filtered, m.filteredByKind, m.lookupFiltered = newAll, newByKind, newLookup
	m.treesMu.Unlock()
	m.bumpRevisionAll()
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	newAll, newByKind, newLookup := NewTreeDb(customComparator), make(map[models.Kind]*TreeDb), make(map[models.Kind]map[string][]*models.Mock)
	var maxSortOrder int64
	for i, mock := range mocks {
		if mock.TestModeInfo.SortOrder == 0 { mock.TestModeInfo.SortOrder = int64(i) + 1}
		if mock.TestModeInfo.SortOrder > maxSortOrder { maxSortOrder = mock.TestModeInfo.SortOrder }
		mock.TestModeInfo.ID = i
		newAll.insert(mock.TestModeInfo, mock)
		k := mock.Kind
		if newByKind[k] == nil { newByKind[k] = NewTreeDb(customComparator) }
		newByKind[k].insert(mock.TestModeInfo, mock)
		if newLookup[k] == nil { newLookup[k] = make(map[string][]*models.Mock) }
		key := getLookupKey(mock)
		newLookup[k][key] = append(newLookup[k][key], mock)
	}
	for _, km := range newLookup { for _, slice := range km { sortDNSList(slice) } }
	if maxSortOrder > 0 { pkg.UpdateSortCounterIfHigher(maxSortOrder) }
	m.treesMu.Lock()
	m.unfiltered, m.unfilteredByKind, m.lookupUnfiltered = newAll, newByKind, newLookup
	m.treesMu.Unlock()
	m.bumpRevisionAll()
}

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()
	if !m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new) { return false }
	if td, ok := m.unfilteredByKind[new.Kind]; ok { td.update(old.TestModeInfo, new.TestModeInfo, new) }
	m.syncLookup(old, false, true); m.syncLookup(new, false, false); return true
}

func (m *MockManager) DeleteFilteredMock(mock models.Mock) bool {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()
	if !m.filtered.delete(mock.TestModeInfo) { return false }
	if td, ok := m.filteredByKind[mock.Kind]; ok { td.delete(mock.TestModeInfo) }
	m.syncLookup(&mock, true, true); return true
}

func (m *MockManager) DeleteUnFilteredMock(mock models.Mock) bool {
	m.treesMu.Lock()
	defer m.treesMu.Unlock()
	if !m.unfiltered.delete(mock.TestModeInfo) { return false }
	if td, ok := m.unfilteredByKind[mock.Kind]; ok { td.delete(mock.TestModeInfo) }
	m.syncLookup(&mock, false, true); return true
}

func (m *MockManager) syncLookup(mock *models.Mock, isFiltered, remove bool) {
	key, k := getLookupKey(mock), mock.Kind
	target := m.lookupUnfiltered; if isFiltered { target = m.lookupFiltered }
	if target[k] == nil { target[k] = make(map[string][]*models.Mock) }
	list := target[k][key]
	if remove {
		for i, item := range list {
			if item.Name == mock.Name && item.TestModeInfo.ID == mock.TestModeInfo.ID {
				list = append(list[:i], list[i+1:]...); break
			}
		}
	} else { list = append(list, mock); sortDNSList(list) }
	if len(list) == 0 { delete(target[k], key) } else { target[k][key] = list }
}

func sortDNSList(list []*models.Mock) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].TestModeInfo.SortOrder != list[j].TestModeInfo.SortOrder { return list[i].TestModeInfo.SortOrder < list[j].TestModeInfo.SortOrder }
		return list[i].TestModeInfo.ID < list[j].TestModeInfo.ID
	})
}

func (m *MockManager) Revision() uint64 { return atomic.LoadUint64(&m.rev) }
func (m *MockManager) bumpRevisionAll() { atomic.AddUint64(&m.rev, 1) }
func (m *MockManager) RevisionByKind(kind models.Kind) uint64 {
	m.revMu.RLock(); ptr := m.revByKind[kind]; m.revMu.RUnlock()
	if ptr == nil { return 0 }; return atomic.LoadUint64(ptr)
}
func (m *MockManager) bumpRevisionKind(kind models.Kind) {
	m.revMu.Lock(); ptr := m.revByKind[kind]; if ptr == nil { var v uint64; ptr = &v; m.revByKind[kind] = ptr }
	m.revMu.Unlock(); atomic.AddUint64(ptr, 1)
}

func (m *MockManager) ensureKindTrees(kind models.Kind) (f *TreeDb, u *TreeDb) {
	m.treesMu.RLock(); f, u = m.filteredByKind[kind], m.unfilteredByKind[kind]; m.treesMu.RUnlock()
	if f != nil && u != nil { return f, u }
	m.treesMu.Lock(); defer m.treesMu.Unlock()
	if f = m.filteredByKind[kind]; f == nil { f = NewTreeDb(customComparator); m.filteredByKind[kind] = f }
	if u = m.unfilteredByKind[kind]; u == nil { u = NewTreeDb(customComparator); m.unfilteredByKind[kind] = u }
	return f, u
}

func (m *MockManager) GetFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock(); defer m.treesMu.RUnlock(); results := make([]*models.Mock, 0, 64)
	m.filtered.rangeValuesNoLock(func(v interface{}) bool { if mock, ok := v.(*models.Mock); ok && mock != nil { results = append(results, mock) }; return true })
	return results, nil
}
func (m *MockManager) GetUnFilteredMocks() ([]*models.Mock, error) {
	m.treesMu.RLock(); defer m.treesMu.RUnlock(); results := make([]*models.Mock, 0, 128)
	m.unfiltered.rangeValuesNoLock(func(v interface{}) bool { if mock, ok := v.(*models.Mock); ok && mock != nil { results = append(results, mock) }; return true })
	return results, nil
}
func (m *MockManager) GetFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	m.treesMu.RLock(); flt := m.filteredByKind[kind]; m.treesMu.RUnlock()
	if flt == nil { flt, _ = m.ensureKindTrees(kind) }
	results := make([]*models.Mock, 0, 64)
	flt.rangeValues(func(v interface{}) bool { if mock, ok := v.(*models.Mock); ok && mock != nil { results = append(results, mock) }; return true })
	return results, nil
}
func (m *MockManager) GetUnFilteredMocksByKind(kind models.Kind) ([]*models.Mock, error) {
	m.treesMu.RLock(); unf := m.unfilteredByKind[kind]; m.treesMu.RUnlock()
	if unf == nil { _, unf = m.ensureKindTrees(kind) }
	results := make([]*models.Mock, 0, 128)
	unf.rangeValues(func(v interface{}) bool { if mock, ok := v.(*models.Mock); ok && mock != nil { results = append(results, mock) }; return true })
	return results, nil
}

func (m *MockManager) MarkMockAsUsed(mock models.Mock) bool {
	m.flagMockAsUsed(models.MockState{Name: mock.Name, Kind: mock.Kind, Usage: models.Updated, IsFiltered: mock.TestModeInfo.IsFiltered, SortOrder: mock.TestModeInfo.SortOrder, Type: mock.Spec.Metadata["type"], Timestamp: mock.Spec.ReqTimestampMock.Unix()})
	return true
}
func (m *MockManager) flagMockAsUsed(mock models.MockState) error {
	if mock.Name == "" { return fmt.Errorf("mock is empty") }
	m.consumedMocks.Store(mock.Name, mock); return nil
}
func (m *MockManager) GetConsumedMocks() []models.MockState {
	var out []models.MockState
	m.consumedMocks.Range(func(key, val interface{}) bool { if st, ok := val.(models.MockState); ok { out = append(out, st) }; return true })
	for _, st := range out { m.consumedMocks.Delete(st.Name) }
	return out
}

func (m *MockManager) GetMySQLCounts() (total, config, data int) {
	m.treesMu.RLock(); defer m.treesMu.RUnlock(); m.unfiltered.rangeValuesNoLock(func(v interface{}) bool {
		if mock, ok := v.(*models.Mock); ok && mock != nil && mock.Kind == models.MySQL {
			total++; if mock.Spec.Metadata["type"] == "config" { config++ } else { data++ }
		}
		return true
	})
	return
}
