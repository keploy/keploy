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

// deleteMock removes the entry at mock's key ONLY when the stored value is that
// same mock, and reports whether it removed anything.
//
// This exists because customComparator keys the tree on (SortOrder, ID) alone —
// not Name, not Kind — while SetFilteredMocks and SetUnFilteredMocks each stamp
// those fields from their OWN 0-based slice index. The same key therefore exists
// in more than one tree, so a delete aimed at the wrong tree does not miss: it
// removes whichever unrelated mock happens to occupy that coordinate.
//
// Callers that hold the matched *models.Mock should prefer this over delete() so
// a wrong-tree delete degrades to a clean miss instead of silently destroying a
// bystander. delete() is kept for callers that only have a key.
func (db *TreeDb) deleteMock(mock *models.Mock) bool {
	if mock == nil {
		return false
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	v, found := db.rbt.Get(mock.TestModeInfo)
	if !found {
		return false
	}
	// Identity check. Name+Kind is what distinguishes two mocks that collide on
	// (SortOrder, ID); a mismatch means this key belongs to a different tree's
	// numbering and must not be touched here.
	if stored, ok := v.(*models.Mock); !ok || stored == nil ||
		stored.Name != mock.Name || stored.Kind != mock.Kind {
		return false
	}
	db.rbt.Remove(mock.TestModeInfo)
	delete(db.idIndex, mock.TestModeInfo.ID)
	return true
}

func (db *TreeDb) update(oldKey interface{}, newKey interface{}, newObj interface{}) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	oldInfo, okOld := oldKey.(models.TestModeInfo)
	newInfo, okNew := newKey.(models.TestModeInfo)

	// First try exact match
	_, found := db.rbt.Get(oldKey)
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
