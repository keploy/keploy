package lru

import (
	"slices"
	"sync"
	"testing"
	"time"
)

// clock is a hand-moved time source for an Expirable.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newExpirable(t *testing.T, size int, ttl time.Duration) (*Expirable[string, int], *clock, *recorder) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := &recorder{t: t}
	c := NewExpirable[string, int](size, r.note, ttl)
	c.now = clk.now
	r.mu = &c.mu
	return c, clk, r
}

func TestExpirableExpiresTTLAfterTheLastAdd(t *testing.T) {
	defer failOnPanic(t)
	const ttl = time.Minute
	c, clk, r := newExpirable(t, 8, ttl)
	c.Add("a", 1)
	clk.advance(ttl)
	// Reading does not extend the time, and the entry lives through its last
	// instant.
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) at exactly ttl = %d, %v; want a hit", v, ok)
	}
	clk.advance(time.Nanosecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("Get(a) hit past its ttl")
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"a", 1}}) {
		t.Fatalf("expiry told the callback %v; want a", got)
	}

	// Adding a held key again restarts its time and moves it last to expire.
	c.Add("b", 2)
	clk.advance(time.Second)
	c.Add("c", 3)
	clk.advance(time.Second)
	c.Add("b", 20)
	clk.advance(ttl - time.Second + time.Nanosecond) // c is past its ttl, b is not
	if got := c.Keys(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("Keys = %v; want [b]", got)
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"c", 3}}) {
		t.Fatalf("expiry told the callback %v; want c", got)
	}
	if v, ok := c.Peek("b"); !ok || v != 20 {
		t.Fatalf("Peek(b) = %d, %v; want 20", v, ok)
	}
}

// TestExpirableEveryCallDropsExpired: each method drops what has expired
// before it answers.
func TestExpirableEveryCallDropsExpired(t *testing.T) {
	defer failOnPanic(t)
	const ttl = time.Minute
	for name, call := range map[string]func(c *Expirable[string, int]) bool{
		"Get":      func(c *Expirable[string, int]) bool { _, ok := c.Get("a"); return ok },
		"Peek":     func(c *Expirable[string, int]) bool { _, ok := c.Peek("a"); return ok },
		"Contains": func(c *Expirable[string, int]) bool { return c.Contains("a") },
		"Remove":   func(c *Expirable[string, int]) bool { return c.Remove("a") },
		"Len":      func(c *Expirable[string, int]) bool { return c.Len() != 0 },
		"Keys":     func(c *Expirable[string, int]) bool { return len(c.Keys()) != 0 },
		"Add":      func(c *Expirable[string, int]) bool { c.Add("b", 0); return c.s.items["a"] != nil },
	} {
		t.Run(name, func(t *testing.T) {
			defer failOnPanic(t)
			c, clk, r := newExpirable(t, 8, ttl)
			c.Add("a", 1)
			if !call(c) {
				t.Fatalf("%s missed a live entry", name)
			}
			if name == "Remove" {
				c.Add("a", 1)
			}
			r.take()
			clk.advance(ttl + time.Nanosecond)
			if call(c) {
				t.Fatalf("%s saw an expired entry", name)
			}
			if got := r.take(); !slices.Contains(got, evicted{"a", 1}) {
				t.Fatalf("%s did not tell the callback a expired: %v", name, got)
			}
		})
	}
}

// TestExpirableSeveralExpireTogether: every entry whose time ran out since the
// last call goes at the next call, not one per call, and the sweep stops at
// the first entry still live.
func TestExpirableSeveralExpireTogether(t *testing.T) {
	defer failOnPanic(t)
	const ttl = time.Minute
	c, clk, r := newExpirable(t, 8, ttl)
	c.Add("a", 1)
	c.Add("b", 2)
	c.Add("c", 3)
	clk.advance(time.Second)
	c.Add("d", 4)
	clk.advance(ttl - time.Second + time.Nanosecond) // a, b and c are past their ttl, d is not
	if v, ok := c.Get("b"); ok {
		t.Fatalf("Get(b) = %d past its ttl; want a miss", v)
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"a", 1}, {"b", 2}, {"c", 3}}) {
		t.Fatalf("one call told the callback %v; want a, b, c", got)
	}
	if got := c.Keys(); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("Keys = %v; want [d]", got)
	}
}

// TestExpirableReadsTheTimeUnderTheLock: the expiry ring stays in expiry
// order only while every call reads the time with the lock held. A call that
// read it first and then waited for the lock would put a sooner expiry
// behind a later one.
func TestExpirableReadsTheTimeUnderTheLock(t *testing.T) {
	defer failOnPanic(t)
	c, clk, _ := newExpirable(t, 8, time.Minute)
	reads := 0
	c.now = func() time.Time {
		reads++
		if c.mu.TryLock() {
			c.mu.Unlock()
			t.Errorf("time read %d was taken without the lock", reads)
		}
		return clk.now()
	}
	c.Add("a", 1)
	c.Get("a")
	if reads != 2 {
		t.Fatalf("Add and Get read the time %d times; want 2", reads)
	}
}

func TestExpirableEvictsLeastRecentlyUsed(t *testing.T) {
	defer failOnPanic(t)
	c, _, r := newExpirable(t, 2, time.Hour)
	c.Add("a", 1)
	if c.Add("b", 2) {
		t.Fatal("Add evicted with room left")
	}
	c.Get("a")
	if !c.Add("c", 3) {
		t.Fatal("Add to a full cache did not report an eviction")
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"b", 2}}) {
		t.Fatalf("evicted %v; want b", got)
	}
	if got := c.Keys(); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("Keys = %v; want [a c]", got)
	}
	if c.Add("a", 10) {
		t.Fatal("replacing a held key reported an eviction")
	}
	if got := c.Keys(); !slices.Equal(got, []string{"c", "a"}) || c.Len() != 2 {
		t.Fatalf("Keys = %v after replacing a; want [c a]", got)
	}
	if got := r.take(); len(got) != 0 {
		t.Fatalf("replacing a held key told the callback %v", got)
	}
	if _, ok := c.Peek("c"); !ok {
		t.Fatal("Peek(c) missed")
	}
	c.Add("d", 4) // Peek left c the least recently used
	if c.Contains("c") || !c.Contains("a") {
		t.Fatalf("Keys = %v; Peek must not change recency", c.Keys())
	}
}

func TestExpirableExpiryFreesRoom(t *testing.T) {
	defer failOnPanic(t)
	c, clk, r := newExpirable(t, 2, time.Minute)
	c.Add("a", 1)
	clk.advance(30 * time.Second)
	c.Add("b", 2)
	c.Get("a") // a is the most recent, but expires first
	clk.advance(31 * time.Second)
	if c.Add("c", 3) {
		t.Fatal("Add evicted though an expired entry made room")
	}
	if got := c.Keys(); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("Keys = %v; want [b c]", got)
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"a", 1}}) {
		t.Fatalf("callback told %v; want a", got)
	}
}

func TestExpirableUnboundedAndForever(t *testing.T) {
	defer failOnPanic(t)
	for _, size := range []int{0, -1} {
		c, clk, _ := newExpirable(t, size, 0)
		for i := range 100 {
			if c.Add(string(rune('a'+i%26))+string(rune('a'+i/26)), i) {
				t.Fatalf("size %d: Add %d evicted", size, i)
			}
		}
		clk.advance(100 * 365 * 24 * time.Hour)
		if n := c.Len(); n != 100 {
			t.Fatalf("size %d, ttl 0: Len = %d a century on; want all 100", size, n)
		}
	}
	c, clk, _ := newExpirable(t, 4, -time.Second)
	c.Add("a", 1)
	clk.advance(time.Hour)
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a negative ttl expired an entry")
	}
}

func TestExpirableRemoveAndPurge(t *testing.T) {
	defer failOnPanic(t)
	c, clk, r := newExpirable(t, 8, time.Minute)
	var removed bool
	noPanic(t, "calls on an empty cache", func() {
		c.Get("zz")
		removed = c.Remove("zz")
	})
	if removed {
		t.Fatal("Remove of a missing key reported it held")
	}
	c.Add("a", 1)
	c.Add("b", 2)
	c.Add("c", 3)
	if !c.Remove("b") {
		t.Fatal("Remove(b) reported it missing")
	}
	if got := r.take(); !slices.Equal(got, []evicted{{"b", 2}}) {
		t.Fatalf("Remove told the callback %v", got)
	}
	clk.advance(30 * time.Second)
	c.Add("d", 4)
	clk.advance(31 * time.Second) // a and c have expired, d has not
	c.Purge()
	if got := r.take(); !slices.Equal(got, []evicted{{"a", 1}, {"c", 3}, {"d", 4}}) {
		t.Fatalf("Purge told the callback %v; want a, c, d", got)
	}
	if c.Len() != 0 {
		t.Fatalf("Len = %d after Purge", c.Len())
	}
	c.Add("e", 5)
	clk.advance(time.Minute + time.Nanosecond)
	if c.Len() != 0 {
		t.Fatal("an entry added after Purge did not expire")
	}
}

func TestExpirableWithoutCallback(t *testing.T) {
	defer failOnPanic(t)
	c := NewExpirable[string, int](1, nil, time.Minute)
	clk := &clock{t: time.Unix(0, 0)}
	c.now = clk.now
	noPanic(t, "dropping entries with no callback", func() {
		c.Add("a", 1)
		c.Add("b", 2) // evicts a
		clk.advance(2 * time.Minute)
		c.Get("b") // expires b
		c.Add("c", 3)
		c.Remove("c")
		c.Add("d", 4)
		c.Purge()
	})
	if c.Len() != 0 {
		t.Fatalf("Len = %d", c.Len())
	}
}

// TestExpirableRealClock checks the default clock: an entry expires after its
// ttl of wall time.
func TestExpirableRealClock(t *testing.T) {
	defer failOnPanic(t)
	c := NewExpirable[string, int](4, nil, time.Millisecond)
	c.Add("a", 1)
	time.Sleep(5 * time.Millisecond)
	if _, ok := c.Get("a"); ok {
		t.Fatal("a 1ms entry was still held 5ms later")
	}
}

func TestExpirableConcurrentUse(t *testing.T) {
	defer failOnPanic(t)
	c := NewExpirable[int, int](32, func(int, int) {}, time.Millisecond)
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
				k := (g*5 + i) % 64
				c.Add(k, i)
				c.Get(k)
				if i%100 == 0 {
					c.Remove(k)
					c.Keys()
					c.Purge()
				}
			}
		}()
	}
	wg.Wait()
	if n := c.Len(); n > 32 {
		t.Fatalf("Len = %d past the size 32", n)
	}
}
