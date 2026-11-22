package lru

import (
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// failOnPanic, deferred first in every test, reports a panic as a failure of
// that test instead of crashing the test binary.
func failOnPanic(t *testing.T) {
	t.Helper()
	if r := recover(); r != nil {
		t.Fatalf("panicked: %v", r)
	}
}

// noPanic runs fn and fails t, naming what panicked, when fn panics.
func noPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	fn()
}

type evicted struct {
	key   string
	value int
}

// recorder collects the callback's calls and checks that none runs while the
// cache holds its lock.
type recorder struct {
	t    *testing.T
	mu   *sync.Mutex
	seen []evicted
}

func (r *recorder) note(key string, value int) {
	if !r.mu.TryLock() {
		r.t.Errorf("callback for %q ran while the cache held its lock", key)
	} else {
		r.mu.Unlock()
	}
	r.seen = append(r.seen, evicted{key, value})
}

func (r *recorder) take() []evicted {
	seen := r.seen
	r.seen = nil
	return seen
}

func newRecordedCache(t *testing.T, size int) (*Cache[string, int], *recorder) {
	t.Helper()
	r := &recorder{t: t}
	c, err := NewWithEvict[string, int](size, r.note)
	if err != nil {
		t.Fatal(err)
	}
	r.mu = &c.mu
	return c, r
}

func TestNewRefusesSizesBelowOne(t *testing.T) {
	defer failOnPanic(t)
	for _, size := range []int{0, -1} {
		if c, err := New[string, int](size); !errors.Is(err, ErrSize) || c != nil {
			t.Errorf("New(%d) = %v, %v; want nil, ErrSize", size, c, err)
		}
		if c, err := NewWithEvict[string, int](size, func(string, int) {}); !errors.Is(err, ErrSize) || c != nil {
			t.Errorf("NewWithEvict(%d) = %v, %v; want nil, ErrSize", size, c, err)
		}
	}
	if _, err := New[string, int](1); err != nil {
		t.Errorf("New(1) = %v; want a cache", err)
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	defer failOnPanic(t)
	c, r := newRecordedCache(t, 3)
	for i, k := range []string{"a", "b", "c"} {
		if c.Add(k, i) {
			t.Fatalf("Add(%q) evicted with room left", k)
		}
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("Len = %d after filling a cache of 3", got)
	}
	// Get makes "a" the most recent, so "b" is the oldest now.
	if v, ok := c.Get("a"); !ok || v != 0 {
		t.Fatalf("Get(a) = %d, %v", v, ok)
	}
	if !c.Add("d", 3) {
		t.Fatal("Add(d) to a full cache did not report an eviction")
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"b", 1}}) {
		t.Fatalf("evicted %v; want b", got)
	}
	if got := c.Keys(); !slices.Equal(got, []string{"c", "a", "d"}) {
		t.Fatalf("Keys = %v; want oldest first [c a d]", got)
	}
	if _, ok := c.Get("b"); ok {
		t.Fatal("the evicted key is still held")
	}
	if got := c.Len(); got != 3 {
		t.Fatalf("Len = %d; want the size, 3", got)
	}
}

func TestCacheAddReplacesWithoutEvicting(t *testing.T) {
	defer failOnPanic(t)
	c, r := newRecordedCache(t, 2)
	c.Add("a", 1)
	c.Add("b", 2)
	if c.Add("a", 10) {
		t.Fatal("replacing a held key reported an eviction")
	}
	if got := r.take(); len(got) != 0 {
		t.Fatalf("replacing a held key told the callback %v", got)
	}
	if v, ok := c.Peek("a"); !ok || v != 10 {
		t.Fatalf("Peek(a) = %d, %v; want the new value 10", v, ok)
	}
	if got := c.Keys(); !slices.Equal(got, []string{"b", "a"}) {
		t.Fatalf("Keys = %v; replacing must make the key the most recent", got)
	}
	if got := c.Len(); got != 2 {
		t.Fatalf("Len = %d; want 2", got)
	}
}

func TestCachePeekAndContainsLeaveRecency(t *testing.T) {
	defer failOnPanic(t)
	c, _ := newRecordedCache(t, 2)
	c.Add("a", 1)
	c.Add("b", 2)
	if v, ok := c.Peek("a"); !ok || v != 1 {
		t.Fatalf("Peek(a) = %d, %v", v, ok)
	}
	if !c.Contains("a") {
		t.Fatal("Contains(a) = false for a held key")
	}
	if _, ok := c.Peek("zz"); ok {
		t.Fatal("Peek of a missing key reported a hit")
	}
	if c.Contains("zz") {
		t.Fatal("Contains of a missing key = true")
	}
	if _, ok := c.Get("zz"); ok {
		t.Fatal("Get of a missing key reported a hit")
	}
	c.Add("c", 3)
	if c.Contains("a") {
		t.Fatal("Peek or Contains made a the most recent: b was evicted instead")
	}
}

func TestCacheOldest(t *testing.T) {
	defer failOnPanic(t)
	c, r := newRecordedCache(t, 3)
	if k, v, ok := c.GetOldest(); ok || k != "" || v != 0 {
		t.Fatalf("GetOldest on an empty cache = %q, %d, %v", k, v, ok)
	}
	var k string
	var v int
	var ok bool
	noPanic(t, "RemoveOldest on an empty cache", func() { k, v, ok = c.RemoveOldest() })
	if ok || k != "" || v != 0 {
		t.Fatalf("RemoveOldest on an empty cache = %q, %d, %v", k, v, ok)
	}
	c.Add("a", 1)
	c.Add("b", 2)
	if k, v, ok := c.GetOldest(); !ok || k != "a" || v != 1 {
		t.Fatalf("GetOldest = %q, %d, %v; want a", k, v, ok)
	}
	if k, v, ok := c.RemoveOldest(); !ok || k != "a" || v != 1 {
		t.Fatalf("RemoveOldest = %q, %d, %v; want a", k, v, ok)
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"a", 1}}) {
		t.Fatalf("RemoveOldest told the callback %v; want a", got)
	}
	if got := c.Keys(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("Keys = %v after RemoveOldest", got)
	}
}

func TestCacheRemoveAndPurge(t *testing.T) {
	defer failOnPanic(t)
	c, r := newRecordedCache(t, 4)
	c.Add("a", 1)
	c.Add("b", 2)
	c.Add("c", 3)
	var removed bool
	noPanic(t, "Remove of a missing key", func() { removed = c.Remove("zz") })
	if removed {
		t.Fatal("Remove of a missing key reported it held")
	}
	if !c.Remove("b") {
		t.Fatal("Remove(b) reported it missing")
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"b", 2}}) {
		t.Fatalf("Remove told the callback %v; want b", got)
	}
	if c.Contains("b") || c.Len() != 2 {
		t.Fatalf("b still held after Remove (Len %d)", c.Len())
	}
	c.Purge()
	if got := r.take(); !slices.Equal(got, []evicted{{"a", 1}, {"c", 3}}) {
		t.Fatalf("Purge told the callback %v; want a then c", got)
	}
	if c.Len() != 0 || len(c.Keys()) != 0 {
		t.Fatalf("Purge left %v", c.Keys())
	}
	// The cache works after a purge.
	c.Add("d", 4)
	if v, ok := c.Get("d"); !ok || v != 4 {
		t.Fatalf("Get(d) after Purge = %d, %v", v, ok)
	}
}

// TestCacheWithoutCallback drops entries every way there is with no callback:
// none may call the missing one.
func TestCacheWithoutCallback(t *testing.T) {
	defer failOnPanic(t)
	c, err := New[string, int](1)
	if err != nil {
		t.Fatal(err)
	}
	noPanic(t, "dropping entries with no callback", func() {
		c.Add("a", 1)
		c.Add("b", 2)
		c.Remove("b")
		c.Add("c", 3)
		c.RemoveOldest()
		c.Add("d", 4)
		c.Purge()
	})
	if c.Len() != 0 {
		t.Fatalf("Len = %d after Purge", c.Len())
	}
}

// TestCacheCallbackMayUseTheCache: the callback runs outside the lock, so it
// can call into the cache that dropped the entry.
func TestCacheCallbackMayUseTheCache(t *testing.T) {
	defer failOnPanic(t)
	var c *Cache[string, int]
	var lens []int
	c, err := NewWithEvict[string, int](1, func(string, int) { lens = append(lens, c.Len()) })
	if err != nil {
		t.Fatal(err)
	}
	c.Add("a", 1)
	c.Add("b", 2)
	c.Remove("b")
	if !slices.Equal(lens, []int{1, 0}) {
		t.Fatalf("Len seen from the callback = %v; want [1 0]", lens)
	}
}

func TestCacheConcurrentUse(t *testing.T) {
	defer failOnPanic(t)
	c, err := New[int, int](64)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("goroutine %d panicked: %v", g, r)
				}
			}()
			for i := 0; i < 2000; i++ {
				k := (g*7 + i) % 100
				c.Add(k, i)
				c.Get(k)
				c.Peek(k + 1)
				if i%50 == 0 {
					c.Remove(k)
					c.Keys()
				}
			}
		}()
	}
	wg.Wait()
	if n := c.Len(); n > 64 {
		t.Fatalf("Len = %d past the size 64", n)
	}
	if n, keys := c.Len(), c.Keys(); n != len(keys) {
		t.Fatalf("Len %d and Keys %d disagree", n, len(keys))
	}
}

// TestAPanicReleasesTheLock: a key that panics inside a cache (an unhashable
// value in an interface key) must not leave that cache locked for every
// later caller.
func TestAPanicReleasesTheLock(t *testing.T) {
	defer failOnPanic(t)
	c, _ := NewWithEvict[any, int](2, func(any, int) {})
	q, _ := New2Q[any, int](2)
	e := NewExpirable[any, int](2, func(any, int) {}, time.Minute)
	for _, tc := range []struct {
		name string
		add  func(key any)
		len  func() int
		mu   *sync.Mutex
	}{
		{"Cache", func(k any) { c.Add(k, 1) }, c.Len, &c.mu},
		{"TwoQueue", func(k any) { q.Add(k, 1) }, q.Len, &q.mu},
		{"Expirable", func(k any) { e.Add(k, 1) }, e.Len, &e.mu},
	} {
		func() {
			defer func() { _ = recover() }()
			tc.add([]int{1})
			t.Errorf("%s: adding an unhashable key did not panic", tc.name)
		}()
		if !tc.mu.TryLock() {
			t.Fatalf("%s: the panic left the cache locked", tc.name)
		}
		tc.mu.Unlock()
		tc.add("a")
		if n := tc.len(); n != 1 {
			t.Fatalf("%s: Len = %d after a panic and one Add; want 1", tc.name, n)
		}
	}
}
