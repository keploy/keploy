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

	// Extract expected name from new object for identity validation.
	// This prevents accidentally removing a different mock when keys have
	// shifted after SetUnFilteredMocks re-populates the tree.
	var expectedName string
	if newMock, ok := newObj.(*models.Mock); ok && newMock != nil {
		expectedName = newMock.Name
	}

	// First try exact key match
	existingVal, found := db.rbt.Get(oldKey)
	if found && expectedName != "" {
		if existingMock, ok := existingVal.(*models.Mock); ok && existingMock != nil {
			if existingMock.Name != expectedName {
				found = false // Different mock at this key, skip
			}
		}
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
	if okOld {
		currentKey, exists := db.idIndex[oldInfo.ID]
		if exists {
			// Verify identity before removing
			if expectedName != "" {
				idVal, idFound := db.rbt.Get(currentKey)
				if idFound {
					if existingMock, ok := idVal.(*models.Mock); ok && existingMock != nil {
						if existingMock.Name != expectedName {
							exists = false // Different mock at this ID, skip
						}
					}
				}
			}
			if exists {
				db.rbt.Remove(currentKey)
				db.rbt.Put(newKey, newObj)
				delete(db.idIndex, oldInfo.ID)
				if okNew {
					db.idIndex[newInfo.ID] = newInfo
				}
				return true
			}
		}
	}

	// Fallback: linear scan by name to find the correct mock.
	// This handles the case where SetUnFilteredMocks re-populated the tree
	// with new ID/SortOrder assignments, making key/ID-based lookups stale.
	if expectedName != "" {
		var foundKey interface{}
		it := db.rbt.Iterator()
		for it.Next() {
			if existingMock, ok := it.Value().(*models.Mock); ok && existingMock != nil {
				if existingMock.Name == expectedName {
					foundKey = it.Key()
					break
				}
			}
		}
		if foundKey != nil {
			foundInfo, foundInfoOk := foundKey.(models.TestModeInfo)
			db.rbt.Remove(foundKey)
			// Use the found mock's ID in the new key to keep idIndex consistent
			adjustedNewKey := newInfo
			if foundInfoOk {
				adjustedNewKey.ID = foundInfo.ID
				delete(db.idIndex, foundInfo.ID)
			}
			db.rbt.Put(adjustedNewKey, newObj)
			if okNew {
				db.idIndex[adjustedNewKey.ID] = adjustedNewKey
			}
			return true
		}
	}

	return false
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
