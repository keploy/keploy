package proxy

// treeDb is a simple wrapper around redblacktree to provide thread safety
// Here it is used to handle the mocks.

import (
	"sort"
	"sync"
	"time"

	"github.com/emirpasic/gods/trees/redblacktree"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// customComparator is a custom comparator function for the tree db
var customComparator = func(a, b interface{}) int {
	aStruct := a.(models.TestModeInfo)
	bStruct := b.(models.TestModeInfo)
	if aStruct.SortOrder < bStruct.SortOrder {
		return -1
	} else if aStruct.SortOrder > bStruct.SortOrder {
		return 1
	}
	if aStruct.ID < bStruct.ID {
		return -1
	} else if aStruct.ID > bStruct.ID {
		return 1
	}
	return 0
}

type TreeDb struct {
	rbt     *redblacktree.Tree
	idIndex map[int]models.TestModeInfo // O(1) lookup by ID
	mu      sync.RWMutex                // RWMutex: many reads, few writes
	// win indexes the tree's entries by recorded request time once a
	// windowed lookup has asked for it. Built, read and dropped under mu; see
	// windowIndex.
	win *windowIndex
	// keyed holds a keyIndex per integrations.MockIndex a lookup has used.
	// Built, read and dropped under mu.
	keyed map[*integrations.MockIndex]*keyIndex
}

func NewTreeDb(comparator func(a, b interface{}) int) *TreeDb {
	return &TreeDb{
		rbt:     redblacktree.NewWith(comparator),
		idIndex: make(map[int]models.TestModeInfo),
	}
}

func (db *TreeDb) insert(key interface{}, obj interface{}) {
	db.mu.Lock()
	db.win = nil   // an entry the window index does not know
	db.keyed = nil // nor do the key indexes
	db.rbt.Put(key, obj)
	// Update ID index
	if info, ok := key.(models.TestModeInfo); ok {
		db.idIndex[info.ID] = info
	}
	db.mu.Unlock()
}

// sameMock reports whether the entry stored in a tree is the mock the caller
// means.
//
// Every tree is keyed by models.TestModeInfo, and that key is TIER-LOCAL:
// SortOrder and ID are stamped from zero as each tier builds its own tree, on
// the fresh copies the agent supplies per call, and customComparator orders on
// those two fields alone. So (SortOrder:1, ID:0) addresses "the first entry of
// whichever tree you asked", not one particular mock — and a mock taken from
// one tier can address a DIFFERENT mock in another. Callers do exactly that:
// mongo v2 tries the filtered door before the startup one, and HTTP and MySQL
// match against the startup-union pool and then consume through the filtered
// and unfiltered doors.
//
// Identity is Name plus Kind plus the recorded request timestamp rather than
// Name alone: names are not enforced unique by the manager, and a name-only
// check silently reverts to the collision when two mocks share one. When the
// caller carries no identity at all (an unnamed, kind-less, timestamp-less
// mock) the check abstains, preserving the historical delete-by-key behaviour
// rather than refusing a delete the caller may depend on.
func sameMock(stored interface{}, want models.Mock) bool {
	if want.Name == "" && want.Kind == "" && want.Spec.ReqTimestampMock.IsZero() {
		return true // nothing to compare against; abstain
	}
	mk, ok := stored.(*models.Mock)
	if !ok || mk == nil {
		return true // not a mock value; leave the old behaviour alone
	}
	if mk.Name != want.Name {
		return false
	}
	if mk.Kind != want.Kind {
		return false
	}
	return mk.Spec.ReqTimestampMock.Equal(want.Spec.ReqTimestampMock)
}

func (db *TreeDb) delete(key interface{}) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, found := db.rbt.Get(key)
	if !found {
		return false
	}
	db.rbt.Remove(key)
	// Remove from ID index
	if info, ok := key.(models.TestModeInfo); ok {
		delete(db.idIndex, info.ID)
	}
	return true
}

// deleteMock removes the entry at key only when it is the mock the caller
// means. See sameMock for why the key alone is not enough.
func (db *TreeDb) deleteMock(key interface{}, want models.Mock) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	v, found := db.rbt.Get(key)
	if !found {
		return false
	}
	if !sameMock(v, want) {
		return false
	}
	db.rbt.Remove(key)
	if info, ok := key.(models.TestModeInfo); ok {
		delete(db.idIndex, info.ID)
	}
	return true
}

func (db *TreeDb) update(oldKey interface{}, newKey interface{}, newObj interface{}, want models.Mock) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	oldInfo, okOld := oldKey.(models.TestModeInfo)
	newInfo, okNew := newKey.(models.TestModeInfo)

	// First try exact match
	cur, found := db.rbt.Get(oldKey)
	if found && !sameMock(cur, want) {
		// The key resolves, but to a different tier's mock. Fall through to
		// the ID index rather than rewriting it; that path is guarded too, so
		// a genuine cross-tier call ends up a no-op instead of a corruption.
		found = false
	}
	if found {
		db.dropWindowUnlessFiled(oldInfo.ID, newInfo.ID, newObj)
		db.followKeyedLocked(oldKey, newKey, cur, newObj)
		db.rbt.Remove(oldKey)
		db.rbt.Put(newKey, newObj)
		// Update ID index
		if okOld {
			delete(db.idIndex, oldInfo.ID)
		}
		if okNew {
			db.idIndex[newInfo.ID] = newInfo
		}
		return true
	}

	// If exact match fails, use ID index for O(1) lookup
	if !okOld {
		return false
	}

	currentKey, exists := db.idIndex[oldInfo.ID]
	if !exists {
		return false
	}

	// The ID index is keyed on ID ALONE, and ID is stamped from zero per tier,
	// so idIndex[0] exists in every tree. Without an identity check this
	// fallback fires on any exact-match miss and rewrites whatever sits at that
	// ID in THIS tree — which for a mock that belongs to another tier is a
	// session mock reused by every test in the set, replaced by a foreign mock
	// at a fresh key where it is then served for the rest of the run. That is
	// how an exact-match miss turns into silent cross-tier corruption.
	// Refuse unless the entry the index points at is demonstrably the caller's
	// mock. A missing entry means a dangling index, and falling through would
	// Remove a no-op and then blindly INSERT the caller's mock into this tree —
	// injecting a foreign tier's mock rather than merely rewriting one.
	cur, curFound := db.rbt.Get(currentKey)
	if !curFound || !sameMock(cur, want) {
		return false
	}

	// Found by ID, update it
	db.dropWindowUnlessFiled(currentKey.ID, newInfo.ID, newObj)
	db.followKeyedLocked(currentKey, newKey, cur, newObj)
	db.rbt.Remove(currentKey)
	db.rbt.Put(newKey, newObj)
	delete(db.idIndex, oldInfo.ID)
	if okNew {
		db.idIndex[newInfo.ID] = newInfo
	}
	return true
}

func (db *TreeDb) deleteAll() {
	db.mu.Lock()
	db.rbt.Clear()
	db.idIndex = make(map[int]models.TestModeInfo) // Reset ID index
	db.win = nil
	db.keyed = nil
	db.mu.Unlock()
}

// size returns the number of entries the tree holds.
func (db *TreeDb) size() int {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.rbt.Size()
}

// rangeValues iterates without allocating a []interface{} snapshot.
func (db *TreeDb) rangeValues(fn func(v interface{}) bool) {
	db.mu.RLock()
	it := db.rbt.Iterator()
	for it.Next() {
		if !fn(it.Value()) {
			break
		}
	}
	db.mu.RUnlock()
}

// dropWindowUnlessFiled drops the window index unless it stays right once
// newObj is stored under newID in place of the entry under oldID: the ID does
// not change, and the index files it under newObj's request time. db.mu must
// be held for writing.
func (db *TreeDb) dropWindowUnlessFiled(oldID, newID int, newObj interface{}) {
	if db.win == nil || db.win.walk {
		return // a walk-only index holds nothing an update can make wrong
	}
	n, ok := newObj.(*models.Mock)
	if oldID != newID || !ok || n == nil || !db.win.files(newID, n.Spec.ReqTimestampMock) {
		db.win = nil
	}
}

// valuesInWindow returns, in tree order, the mocks the tree holds recorded in
// [start, end] or undated, found through the window index, which the first
// lookup after a staging or a change builds. It holds the tree's lock from
// reading the index to resolving its IDs, so no change can land in between.
func (db *TreeDb) valuesInWindow(start, end time.Time) []*models.Mock {
	db.mu.RLock()
	if db.win == nil {
		db.mu.RUnlock()
		db.mu.Lock()
		if db.win == nil {
			db.win = db.buildWindowIndexLocked()
		}
		hits := db.windowHitsLocked(start, end)
		db.mu.Unlock()
		return db.inTreeOrder(hits)
	}
	hits := db.windowHitsLocked(start, end)
	db.mu.RUnlock()
	return db.inTreeOrder(hits)
}

type windowHit struct {
	key interface{}
	mk  *models.Mock
}

// windowHitsLocked resolves the index's candidates for [start, end] to the
// mocks the tree holds under them and keeps those recorded in the window.
// db.mu must be held, and db.win set.
func (db *TreeDb) windowHitsLocked(start, end time.Time) []windowHit {
	if db.win.walk {
		var hits []windowHit
		it := db.rbt.Iterator()
		for it.Next() {
			if mk, isMock := it.Value().(*models.Mock); isMock && mk != nil && recordedIn(mk, start, end) {
				hits = append(hits, windowHit{it.Key(), mk})
			}
		}
		return hits
	}
	ids := db.win.idsIn(start, end)
	hits := make([]windowHit, 0, len(ids))
	for _, id := range ids {
		key, ok := db.idIndex[id]
		if !ok {
			continue
		}
		v, found := db.rbt.Get(key)
		if !found {
			continue
		}
		if mk, isMock := v.(*models.Mock); isMock && mk != nil && recordedIn(mk, start, end) {
			hits = append(hits, windowHit{key, mk})
		}
	}
	return hits
}

// inTreeOrder sorts hits by the tree's comparator, which never changes, so it
// needs no lock.
func (db *TreeDb) inTreeOrder(hits []windowHit) []*models.Mock {
	cmp := db.rbt.Comparator
	sort.Slice(hits, func(i, j int) bool { return cmp(hits[i].key, hits[j].key) < 0 })
	out := make([]*models.Mock, len(hits))
	for i, h := range hits {
		out[i] = h.mk
	}
	return out
}
