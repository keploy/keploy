package lru

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// state is where a TwoQueue holds its keys, each list oldest first.
type state struct{ recent, frequent, ghost []string }

func stateOf(q *TwoQueue[string, int]) state {
	return state{q.recent.keys(), q.frequent.keys(), q.ghost.keys()}
}

func keys(prefix string, from, to int) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf("%s%d", prefix, i))
	}
	return out
}

func newTwoQueue(t *testing.T, size int) *TwoQueue[string, int] {
	t.Helper()
	q, err := New2Q[string, int](size)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func wantState(t *testing.T, q *TwoQueue[string, int], step string, want state) {
	t.Helper()
	got := stateOf(q)
	if !slices.Equal(got.recent, want.recent) || !slices.Equal(got.frequent, want.frequent) || !slices.Equal(got.ghost, want.ghost) {
		t.Fatalf("after %s:\n got recent %v frequent %v ghost %v\nwant recent %v frequent %v ghost %v",
			step, got.recent, got.frequent, got.ghost, want.recent, want.frequent, want.ghost)
	}
	if n := q.Len(); n != len(want.recent)+len(want.frequent) {
		t.Fatalf("after %s: Len = %d; want %d", step, n, len(want.recent)+len(want.frequent))
	}
}

func TestNew2QRefusesSizesBelowOne(t *testing.T) {
	defer failOnPanic(t)
	for _, size := range []int{0, -1} {
		if q, err := New2Q[string, int](size); !errors.Is(err, ErrSize) || q != nil {
			t.Errorf("New2Q(%d) = %v, %v; want nil, ErrSize", size, q, err)
		}
	}
	if _, err := New2Q[string, int](1); err != nil {
		t.Errorf("New2Q(1) = %v; want a cache", err)
	}
}

// TestTwoQueueScanLeavesFrequentKeys pins the reason the cache is 2Q: a scan
// of keys each seen once, many times the size, evicts only seen-once keys.
func TestTwoQueueScanLeavesFrequentKeys(t *testing.T) {
	defer failOnPanic(t)
	q := newTwoQueue(t, 8) // seen-once share 2, ghost 4
	for _, k := range keys("f", 1, 4) {
		q.Add(k, 0)
		if _, ok := q.Get(k); !ok {
			t.Fatalf("Get(%s) right after Add missed", k)
		}
	}
	wantState(t, q, "promoting f1-f4", state{frequent: keys("f", 1, 4)})
	for _, k := range keys("s", 1, 4) {
		q.Add(k, 0)
	}
	wantState(t, q, "filling with s1-s4", state{recent: keys("s", 1, 4), frequent: keys("f", 1, 4)})
	q.Add("s5", 0)
	wantState(t, q, "s5 on a full cache", state{recent: keys("s", 2, 5), frequent: keys("f", 1, 4), ghost: []string{"s1"}})
	for _, k := range keys("s", 6, 40) {
		q.Add(k, 0)
	}
	wantState(t, q, "scanning s6-s40", state{recent: keys("s", 37, 40), frequent: keys("f", 1, 4), ghost: keys("s", 33, 36)})
}

// TestTwoQueueSeenOnceShare covers the list a full cache evicts from when the
// seen-once list is exactly at its share: a new key evicts a seen-once entry,
// a key coming back from the ghost list evicts a frequent one.
func TestTwoQueueSeenOnceShare(t *testing.T) {
	defer failOnPanic(t)
	q := newTwoQueue(t, 8)
	for _, k := range keys("a", 1, 6) {
		q.Add(k, 0)
		q.Get(k)
	}
	q.Add("r1", 0)
	q.Add("r2", 0)
	wantState(t, q, "a full cache at the seen-once share", state{recent: []string{"r1", "r2"}, frequent: keys("a", 1, 6)})
	q.Add("n1", 0)
	wantState(t, q, "a new key", state{recent: []string{"r2", "n1"}, frequent: keys("a", 1, 6), ghost: []string{"r1"}})
	q.Add("r1", 1)
	wantState(t, q, "a key back from the ghost list", state{recent: []string{"r2", "n1"}, frequent: append(keys("a", 2, 6), "r1")})
	if v, ok := q.Peek("r1"); !ok || v != 1 {
		t.Fatalf("Peek(r1) = %d, %v; want the value added back, 1", v, ok)
	}
}

// TestTwoQueueGhostFromBelowShare: a key back from the ghost list while the
// seen-once list is under its share evicts a frequent entry.
func TestTwoQueueGhostBelowShare(t *testing.T) {
	defer failOnPanic(t)
	q := newTwoQueue(t, 8)
	for _, k := range keys("s", 1, 9) {
		q.Add(k, 0)
	}
	wantState(t, q, "s1-s9", state{recent: keys("s", 2, 9), ghost: []string{"s1"}})
	q.Add("s1", 0)
	// The seen-once list is past its share, so s2 goes to the ghost list.
	wantState(t, q, "s1 back", state{recent: keys("s", 3, 9), frequent: []string{"s1"}, ghost: []string{"s2"}})
	for _, k := range keys("s", 3, 8) {
		q.Get(k)
	}
	wantState(t, q, "reading s3-s8", state{recent: []string{"s9"}, frequent: append([]string{"s1"}, keys("s", 3, 8)...), ghost: []string{"s2"}})
	q.Add("s2", 0)
	wantState(t, q, "s2 back under the share", state{recent: []string{"s9"}, frequent: append(keys("s", 3, 8), "s2"), ghost: []string{}})
}

// TestTwoQueueTinySizes: below size 4 the seen-once share is 0, and a full
// cache whose keys are all frequent must evict a frequent one.
func TestTwoQueueTinySizes(t *testing.T) {
	defer failOnPanic(t)
	q := newTwoQueue(t, 2)
	q.Add("a", 0)
	q.Get("a")
	q.Add("b", 0)
	q.Get("b")
	wantState(t, q, "two frequent keys", state{frequent: []string{"a", "b"}})
	noPanic(t, "adding to a full cache with no seen-once entry", func() { q.Add("c", 0) })
	wantState(t, q, "c", state{recent: []string{"c"}, frequent: []string{"b"}})
	q.Add("d", 0)
	wantState(t, q, "d", state{recent: []string{"d"}, frequent: []string{"b"}, ghost: []string{"c"}})

	one := newTwoQueue(t, 1)
	one.Add("a", 0)
	one.Add("b", 0)
	wantState(t, one, "size 1", state{recent: []string{"b"}})
}

func TestTwoQueueReadsAndUpdates(t *testing.T) {
	defer failOnPanic(t)
	q := newTwoQueue(t, 8)
	if _, ok := q.Get("x"); ok {
		t.Fatal("Get of a missing key hit")
	}
	if _, ok := q.Peek("x"); ok || q.Contains("x") {
		t.Fatal("Peek or Contains of a missing key hit")
	}
	q.Add("a", 1)
	q.Add("b", 2)
	if v, ok := q.Peek("a"); !ok || v != 1 || !q.Contains("a") {
		t.Fatalf("Peek(a) = %d, %v", v, ok)
	}
	wantState(t, q, "Peek and Contains", state{recent: []string{"a", "b"}})
	q.Add("a", 10) // a second Add promotes, with the new value
	wantState(t, q, "adding a again", state{recent: []string{"b"}, frequent: []string{"a"}})
	if v, ok := q.Peek("a"); !ok || v != 10 {
		t.Fatalf("Peek(a) = %d, %v; want 10", v, ok)
	}
	q.Get("b")
	wantState(t, q, "reading b", state{frequent: []string{"a", "b"}})
	if v, ok := q.Get("a"); !ok || v != 10 {
		t.Fatalf("Get(a) = %d, %v; want 10", v, ok)
	}
	wantState(t, q, "reading frequent a", state{frequent: []string{"b", "a"}})
	q.Add("b", 20)
	wantState(t, q, "adding frequent b", state{frequent: []string{"a", "b"}})
	if v, ok := q.Get("b"); !ok || v != 20 {
		t.Fatalf("Get(b) = %d, %v; want 20", v, ok)
	}
	if v, ok := q.Peek("b"); !ok || v != 20 || !q.Contains("b") {
		t.Fatalf("Peek(b) = %d, %v; want 20", v, ok)
	}
	q.Purge()
	wantState(t, q, "Purge", state{})
}

func TestTwoQueuePurgeForgetsGhosts(t *testing.T) {
	defer failOnPanic(t)
	q := newTwoQueue(t, 4)
	for _, k := range keys("s", 1, 5) {
		q.Add(k, 0)
	}
	q.Purge()
	q.Add("s1", 0)
	wantState(t, q, "s1 after Purge", state{recent: []string{"s1"}})
}
