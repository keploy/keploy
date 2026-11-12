package http

import (
	"testing"
)

// restagingDb is genDb that tells its parsers when it lets its recordings go
// (integrations.Restaged): restage moves its generation, then runs what they
// asked to run, as the manager does at a staging.
type restagingDb struct {
	*genDb
	asked map[any]func()
}

func (d *restagingDb) WhenRestaged(slot any, release func()) {
	if d.asked == nil {
		d.asked = map[any]func(){}
	}
	d.asked[slot] = release
}

func (d *restagingDb) restage() {
	d.gen++
	fns := d.asked
	d.asked = nil
	for _, f := range fns {
		f()
	}
}

// kept is whether x keeps an index, and whether anything it keeps names a
// store.
func kept[T any](x *genIndexes[T]) (index, store bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.has, x.has || x.key.store != nil || x.seen.store != nil
}

// An index of a store's reads, and the key of the parser's last call, go when
// the store lets its recordings go, not at the parser's next call: that may
// come only in a later set, or never, and until then the index would keep
// the last set's recordings beside the next set's.
func TestAnIndexGoesWhenItsStoreLetsItsRecordingsGo(t *testing.T) {
	db := &restagingDb{genDb: newGenDb(nil, nil)}
	var x genIndexes[*int]
	read := func() error { return nil }
	build := func(learnIndexKey) *int { return new(int) }
	if _, ok := x.of(db, db, "", read, build); ok {
		t.Fatal("the generation's first call built an index")
	}
	db.restage()
	if _, store := kept(&x); store {
		t.Fatal("the key of the last call is kept after its store let its recordings go")
	}
	x.of(db, db, "", read, build)
	if _, ok := x.of(db, db, "", read, build); !ok {
		t.Fatal("the generation's second call built no index")
	}
	if index, _ := kept(&x); !index {
		t.Fatal("no index kept")
	}
	db.restage()
	if index, store := kept(&x); index || store {
		t.Fatalf("after its store let its recordings go the parser keeps an index %v, or names the store %v", index, store)
	}
}

// An index built from what the store let go of while it was built goes too:
// the store ran what the parser asked to run before the index was kept.
func TestAnIndexBuiltAsItsStoreLetsItsRecordingsGoIsNotKept(t *testing.T) {
	db := &restagingDb{genDb: newGenDb(nil, nil)}
	var x genIndexes[*int]
	read := func() error { return nil }
	x.of(db, db, "", read, func(learnIndexKey) *int { return new(int) })
	if _, ok := x.of(db, db, "", read, func(learnIndexKey) *int { db.restage(); return new(int) }); !ok {
		t.Fatal("the generation's second call built no index")
	}
	if index, store := kept(&x); index || store {
		t.Fatalf("an index of recordings its store let go of while it was built is kept %v, or names the store %v", index, store)
	}
}
