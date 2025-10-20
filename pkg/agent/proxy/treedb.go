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
	rbt *redblacktree.Tree
	mu  sync.RWMutex // RWMutex: many reads, few writes
}

func NewTreeDb(comparator func(a, b interface{}) int) *TreeDb {
	return &TreeDb{
		rbt: redblacktree.NewWith(comparator),
	}
}

func (db *TreeDb) insert(key interface{}, obj interface{}) {
	db.mu.Lock()
	db.rbt.Put(key, obj)
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
	return true
}

func (db *TreeDb) update(oldKey interface{}, newKey interface{}, newObj interface{}) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, found := db.rbt.Get(oldKey)
	if !found {
		return false
	}
	db.rbt.Remove(oldKey)
	db.rbt.Put(newKey, newObj)
	return true
}

func (db *TreeDb) deleteAll() {
	db.mu.Lock()
	db.rbt.Clear()
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
