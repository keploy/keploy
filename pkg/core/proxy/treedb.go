package proxy

// treeDb is a simple wrapper around redblacktree to provide thread safety
// Here it is used to handle the mocks.

import (
	"sync"

	"github.com/emirpasic/gods/trees/redblacktree"
	"go.keploy.io/server/v2/pkg/models"
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
	rbt   *redblacktree.Tree
	mutex *sync.Mutex
}

func NewTreeDb(comparator func(a, b interface{}) int) *TreeDb {
	return &TreeDb{
		rbt:   redblacktree.NewWith(comparator),
		mutex: &sync.Mutex{},
	}
}

func (db *TreeDb) insert(key interface{}, obj interface{}) {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	db.rbt.Put(key, obj)
}

func (db *TreeDb) delete(key interface{}) bool {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	_, found := db.rbt.Get(key)
	if !found {
		return false
	}
	db.rbt.Remove(key)
	return true
}

func (db *TreeDb) update(oldKey interface{}, newKey interface{}, newObj interface{}) bool {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	_, found := db.rbt.Get(oldKey)
	if !found {
		return false
	}
	db.rbt.Remove(oldKey)
	db.rbt.Put(newKey, newObj)
	return true
}

func (db *TreeDb) deleteAll() {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	db.rbt.Clear()
}

func (db *TreeDb) getAll() []interface{} {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	return db.rbt.Values()
}
