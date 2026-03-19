package proxy

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"

	"github.com/emirpasic/gods/trees/redblacktree"
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
	// Build global and kind-based structures in local maps (HashMap Batching)
	globalRBT := redblacktree.NewWith(customComparator)
	globalIdx := make(map[int]models.TestModeInfo)
	
	type kindData struct {
		rbt *redblacktree.Tree
		idx map[int]models.TestModeInfo
	}
	kinds := make(map[models.Kind]*kindData)
	touched := map[models.Kind]struct{}{}

	var maxSort int64
	for i, mk := range mocks {
		if mk == nil { continue }
		if mk.TestModeInfo.SortOrder == 0 { mk.TestModeInfo.SortOrder = int64(i) + 1 }
		if mk.TestModeInfo.SortOrder > maxSort { maxSort = mk.TestModeInfo.SortOrder }
		mk.TestModeInfo.ID = i

		globalRBT.Put(mk.TestModeInfo, mk)
		globalIdx[mk.TestModeInfo.ID] = mk.TestModeInfo

		kd := kinds[mk.Kind]
		if kd == nil {
			kd = &kindData{rbt: redblacktree.NewWith(customComparator), idx: make(map[int]models.TestModeInfo)}
			kinds[mk.Kind] = kd
		}
		kd.rbt.Put(mk.TestModeInfo, mk)
		kd.idx[mk.TestModeInfo.ID] = mk.TestModeInfo
		touched[mk.Kind] = struct{}{}
	}

	if maxSort > 0 { pkg.UpdateSortCounterIfHigher(maxSort) }

	// Atomic Swap (Fast Switch)
	m.filtered.reset(globalRBT, globalIdx)
	newKinds := make(map[models.Kind]*TreeDb, len(kinds))
	for k, kd := range kinds {
		td := NewTreeDb(customComparator)
		td.rbt, td.idIndex = kd.rbt, kd.idx
		newKinds[k] = td
	}
	m.treesMu.Lock()
	m.filteredByKind = newKinds
	m.treesMu.Unlock()

	for k := range touched { m.bumpRevisionKind(k) }
	m.bumpRevisionAll()
}

func (m *MockManager) SetUnFilteredMocks(mocks []*models.Mock) {
	// Build global and kind-based structures in local maps (HashMap Batching)
	globalRBT := redblacktree.NewWith(customComparator)
	globalIdx := make(map[int]models.TestModeInfo)
	
	type kindData struct {
		rbt *redblacktree.Tree
		idx map[int]models.TestModeInfo
	}
	kinds := make(map[models.Kind]*kindData)
	touched := map[models.Kind]struct{}{}

	var maxSort int64
	for i, mk := range mocks {
		if mk == nil { continue }
		if mk.TestModeInfo.SortOrder == 0 { mk.TestModeInfo.SortOrder = int64(i) + 1 }
		if mk.TestModeInfo.SortOrder > maxSort { maxSort = mk.TestModeInfo.SortOrder }
		mk.TestModeInfo.ID = i

		globalRBT.Put(mk.TestModeInfo, mk)
		globalIdx[mk.TestModeInfo.ID] = mk.TestModeInfo

		kd := kinds[mk.Kind]
		if kd == nil {
			kd = &kindData{rbt: redblacktree.NewWith(customComparator), idx: make(map[int]models.TestModeInfo)}
			kinds[mk.Kind] = kd
		}
		kd.rbt.Put(mk.TestModeInfo, mk)
		kd.idx[mk.TestModeInfo.ID] = mk.TestModeInfo
		touched[mk.Kind] = struct{}{}
	}

	if maxSort > 0 { pkg.UpdateSortCounterIfHigher(maxSort) }

	// Atomic Swap (Fast Switch)
	m.unfiltered.reset(globalRBT, globalIdx)
	newKinds := make(map[models.Kind]*TreeDb, len(kinds))
	for k, kd := range kinds {
		td := NewTreeDb(customComparator)
		td.rbt, td.idIndex = kd.rbt, kd.idx
		newKinds[k] = td
	}
	m.treesMu.Lock()
	m.unfilteredByKind = newKinds
	m.treesMu.Unlock()

	for k := range touched { m.bumpRevisionKind(k) }
	m.bumpRevisionAll()
}

// ---------- point updates / deletes (keep per-kind in sync) ----------

func (m *MockManager) UpdateUnFilteredMock(old *models.Mock, new *models.Mock) bool {
	// Update legacy/global tree first
	updatedGlobal := m.unfiltered.update(old.TestModeInfo, new.TestModeInfo, new)

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
			Name:       new.Name,
			Kind:       new.Kind,
			Usage:      models.Updated,
			IsFiltered: new.TestModeInfo.IsFiltered,
			SortOrder:  new.TestModeInfo.SortOrder,
			Type:       new.Spec.Metadata["type"],
			Timestamp:  new.Spec.ReqTimestampMock.Unix(),
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
	deletedGlobal := m.filtered.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	flt, _ := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := flt.delete(mock.TestModeInfo)
	m.treesMu.Unlock()

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Kind:       mock.Kind,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
			Type:       mock.Spec.Metadata["type"],
			Timestamp:  mock.Spec.ReqTimestampMock.Unix(),
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
	deletedGlobal := m.unfiltered.delete(mock.TestModeInfo)

	// per-kind
	k := mock.Kind
	_, unf := m.ensureKindTrees(k)
	m.treesMu.Lock()
	deletedKind := unf.delete(mock.TestModeInfo)
	m.treesMu.Unlock()

	if deletedGlobal {
		if err := m.flagMockAsUsed(models.MockState{
			Name:       mock.Name,
			Kind:       mock.Kind,
			Usage:      models.Deleted,
			IsFiltered: mock.TestModeInfo.IsFiltered,
			SortOrder:  mock.TestModeInfo.SortOrder,
			Type:       mock.Spec.Metadata["type"],
			Timestamp:  mock.Spec.ReqTimestampMock.Unix(),
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
		Name:       mock.Name,
		Kind:       mock.Kind,
		Usage:      models.Updated,
		IsFiltered: mock.TestModeInfo.IsFiltered,
		SortOrder:  mock.TestModeInfo.SortOrder,
		Type:       mock.Spec.Metadata["type"],
		Timestamp:  mock.Spec.ReqTimestampMock.Unix(),
	}); err != nil {
		if m.logger != nil {
			m.logger.Error("failed to flag mock as used", zap.Error(err))
		}
		return false
	}
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
