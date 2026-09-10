package proxy

// treeDb is a simple wrapper around redblacktree to provide thread safety
// Here it is used to handle the mocks.

import (
	"sync"

	"github.com/emirpasic/gods/trees/redblacktree"
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
}

func NewTreeDb(comparator func(a, b interface{}) int) *TreeDb {
	return &TreeDb{
		rbt:     redblacktree.NewWith(comparator),
		idIndex: make(map[int]models.TestModeInfo),
	}
}

func (db *TreeDb) insert(key interface{}, obj interface{}) {
	db.mu.Lock()
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
	db.mu.Unlock()
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
