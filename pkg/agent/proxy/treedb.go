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

// reset replaces the internal tree and index with new ones.
func (db *TreeDb) reset(rbt *redblacktree.Tree, idIndex map[int]models.TestModeInfo) {
	db.mu.Lock()
	db.rbt = rbt
	db.idIndex = idIndex
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
