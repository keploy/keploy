package hooks

import (
	"sync"

	"github.com/emirpasic/gods/trees/redblacktree"
)

type treeDb struct {
	rbt   *redblacktree.Tree
	mutex *sync.Mutex
}

func NewTreeDb(comparator func(a, b interface{}) int) *treeDb {
	return &treeDb{
		rbt:   redblacktree.NewWith(comparator),
		mutex: &sync.Mutex{},
	}
}

func (db *treeDb) insert(key interface{}, obj interface{}) {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	db.rbt.Put(key, obj)
}

func (db *treeDb) delete(key interface{}) bool {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	_, found := db.rbt.Get(key)
	if !found {
		return false
	}
	db.rbt.Remove(key)
	return true
}

func (db *treeDb) update(oldKey interface{}, newKey interface{}, newObj interface{}) bool {
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

func (db *treeDb) deleteAll() {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	db.rbt.Clear()
}

func (db *treeDb) getAll() []interface{} {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	return db.rbt.Values()
}
