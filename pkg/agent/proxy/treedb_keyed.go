package proxy

import (
	"slices"
	"sort"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// keyIndex files a tree's entries under the keys an integrations.MockIndex
// gives their mocks, each key's entries in tree order, so a lookup visits the
// entries filed under one key instead of walking the whole tree. It belongs to
// one TreeDb and lives under its lock (TreeDb.keyed); the first lookup with a
// MockIndex builds it.
//
// An entry records the tree key it was filed under, and counts only while the
// tree holds that very mock under that key; lookups skip any other. Every list
// is in strict tree-key order. An update moves the mock's entry to its new key
// in each of its lists (see follow). A change the lists cannot follow (an
// insert, a mock stored again under a key a list files, or an update that
// changes the mock's keys) drops the index, and the next lookup builds it
// again.
type keyIndex struct {
	lists map[string][]keyedEntry
}

// keyedEntry is one mock filed under one key. key is the tree key it was filed
// under, boxed once here rather than on every lookup.
type keyedEntry struct {
	key interface{}
	mk  *models.Mock
}

// distinctKeys returns ix's keys for mk, each once.
func distinctKeys(ix *integrations.MockIndex, mk *models.Mock) []string {
	ks := ix.Keys(mk)
	for i := 1; i < len(ks); i++ {
		if slices.Contains(ks[:i], ks[i]) {
			out := slices.Clone(ks[:i])
			for _, k := range ks[i+1:] {
				if !slices.Contains(out, k) {
					out = append(out, k)
				}
			}
			return out
		}
	}
	return ks
}

// buildKeyIndexLocked files db's entries for ix. db.mu must be held for
// writing.
func (db *TreeDb) buildKeyIndexLocked(ix *integrations.MockIndex) *keyIndex {
	ki := &keyIndex{lists: map[string][]keyedEntry{}}
	it := db.rbt.Iterator()
	for it.Next() {
		key, ok := it.Key().(models.TestModeInfo)
		if !ok {
			continue
		}
		mk, ok := it.Value().(*models.Mock)
		if !ok || mk == nil {
			continue
		}
		var boxed interface{} = key
		for _, k := range distinctKeys(ix, mk) {
			ki.lists[k] = append(ki.lists[k], keyedEntry{boxed, mk})
		}
	}
	return ki
}

// liveLocked reports whether the tree still holds e's mock under e's key.
// db.mu must be held.
func (db *TreeDb) liveLocked(e keyedEntry) bool {
	v, found := db.rbt.Get(e.key)
	return found && v == e.mk
}

// followKeyedLocked keeps every key index right across an update that stores
// newObj under newKey in place of cur, stored under curKey, or drops the ones
// it cannot keep right. It runs before the tree changes. db.mu must be held
// for writing.
func (db *TreeDb) followKeyedLocked(curKey, newKey interface{}, cur, newObj interface{}) {
	if len(db.keyed) == 0 {
		return
	}
	_, ok1 := curKey.(models.TestModeInfo)
	_, ok2 := newKey.(models.TestModeInfo)
	c, ok3 := cur.(*models.Mock)
	n, ok4 := newObj.(*models.Mock)
	for ix, ki := range db.keyed {
		if !(ok1 && ok2 && ok3 && ok4 && c != nil && n != nil && ki.follow(db, ix, curKey, newKey, c, n)) {
			delete(db.keyed, ix)
		}
	}
}

// follow moves cur's entry in each of its lists to n under newKey, at newKey's
// place in tree order. It reports false, filing nothing, when it cannot: the
// keys change, or a list already files an entry under newKey (a mock stored
// again under the key it had).
//
// cur's entry is found by binary search on curKey and removed by shifting the
// shorter side of the list, so no dead entry is left behind for lookups to
// step over, whatever sits in front of it. A re-stamp lands at or near the end
// of each list; one that took its sort number before another re-stamp landed
// lands just in front of it.
func (ki *keyIndex) follow(db *TreeDb, ix *integrations.MockIndex, curKey, newKey interface{}, cur, n *models.Mock) bool {
	cmp := db.rbt.Comparator
	ks := distinctKeys(ix, n)
	old := distinctKeys(ix, cur)
	if len(ks) != len(old) {
		return false
	}
	for _, k := range ks {
		if !slices.Contains(old, k) {
			return false
		}
	}
	if len(ks) == 0 {
		return true
	}
	for _, k := range ks {
		l := ki.lists[k]
		j := sort.Search(len(l), func(j int) bool { return cmp(l[j].key, newKey) >= 0 })
		if j < len(l) && cmp(l[j].key, newKey) == 0 {
			return false
		}
	}
	for _, k := range ks {
		l := ki.lists[k]
		i := sort.Search(len(l), func(j int) bool { return cmp(l[j].key, curKey) >= 0 })
		if i < len(l) && l[i].mk == cur && cmp(l[i].key, curKey) == 0 {
			if i < len(l)-1-i {
				copy(l[1:i+1], l[:i])
				l[0] = keyedEntry{}
				l = l[1:]
			} else {
				copy(l[i:], l[i+1:])
				l[len(l)-1] = keyedEntry{}
				l = l[:len(l)-1]
			}
		}
		// Entries of deleted mocks at the front.
		for len(l) > 0 && !db.liveLocked(l[0]) {
			l[0] = keyedEntry{}
			l = l[1:]
		}
		j := sort.Search(len(l), func(j int) bool { return cmp(l[j].key, newKey) > 0 })
		l = append(l, keyedEntry{})
		copy(l[j+1:], l[j:])
		l[j] = keyedEntry{newKey, n}
		ki.lists[k] = l
	}
	return true
}

// swapInPlace replaces cur's entry under key with n, in every list that files
// it, when n files under exactly cur's keys — the key does not change, so each
// entry keeps its place in tree order. It reports false when n's keys differ,
// or when a list that should file cur does not (the index no longer describes
// the tree); the caller drops the index then, and the next lookup rebuilds it.
// db.mu must be held for writing.
func (ki *keyIndex) swapInPlace(db *TreeDb, ix *integrations.MockIndex, key models.TestModeInfo, cur, n *models.Mock) bool {
	ks := distinctKeys(ix, n)
	old := distinctKeys(ix, cur)
	if len(ks) != len(old) {
		return false
	}
	for _, k := range ks {
		if !slices.Contains(old, k) {
			return false
		}
	}
	cmp := db.rbt.Comparator
	for _, k := range ks {
		l := ki.lists[k]
		i := sort.Search(len(l), func(j int) bool { return cmp(l[j].key, key) >= 0 })
		if i >= len(l) || l[i].mk != cur || cmp(l[i].key, key) != 0 {
			return false
		}
		l[i].mk = n
	}
	return true
}

// rangeKeyed calls fn with the mocks the tree holds that ix files under key, in
// tree order, until fn returns false.
//
// fn runs without db.mu held: the walk copies a few live entries at a time
// under the read lock, so fn may call back into the manager. Between two
// batches the walk resumes after the tree key of the last entry it examined,
// not at a list position: a list is in tree-key order, and follow removes and
// inserts entries while fn runs, which shifts every position. The same holds
// across a rebuild of the index. Like successive reads, a walk passes a mock
// re-stamped behind it to fn again, as the copy the re-stamp stored.
func (db *TreeDb) rangeKeyed(ix *integrations.MockIndex, key string, fn func(*models.Mock) bool) {
	const batch = 16
	cmp := db.rbt.Comparator
	var (
		after interface{} // tree key of the last entry examined; nil before the first
		buf   = make([]*models.Mock, 0, batch)
	)
	for {
		buf = buf[:0]
		db.mu.RLock()
		cur := db.keyed[ix]
		if cur == nil {
			db.mu.RUnlock()
			db.mu.Lock()
			if db.keyed[ix] == nil {
				if db.keyed == nil {
					db.keyed = map[*integrations.MockIndex]*keyIndex{}
				}
				db.keyed[ix] = db.buildKeyIndexLocked(ix)
			}
			db.mu.Unlock()
			continue
		}
		list := cur.lists[key]
		pos := 0
		if after != nil {
			pos = sort.Search(len(list), func(j int) bool { return cmp(list[j].key, after) > 0 })
		}
		for pos < len(list) && len(buf) < batch {
			e := list[pos]
			pos++
			after = e.key
			if db.liveLocked(e) {
				buf = append(buf, e.mk)
			}
		}
		more := pos < len(list)
		db.mu.RUnlock()
		for _, mk := range buf {
			if !fn(mk) {
				return
			}
		}
		if !more {
			return
		}
	}
}
