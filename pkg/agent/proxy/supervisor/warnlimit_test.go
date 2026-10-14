package supervisor

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// A kind's first warning goes through; the rest within the interval are held
// back and counted, and the first past it says how many were: only those
// held back since the last one let through, not all since the start. Each
// kind keeps its own interval and count, and Reset forgets them all.
func TestWarnLimitersLetOneOfEachKindThroughPerInterval(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000, 0)
	ls := &WarnLimiters{every: time.Minute, now: func() time.Time { return clock }}
	allow := func(key string, wantOK bool, wantHeld uint64) {
		t.Helper()
		if ok, held := ls.Allow(key); ok != wantOK || held != wantHeld {
			t.Fatalf("Allow(%q) at %v = (%v, %d), want (%v, %d)", key, clock, ok, held, wantOK, wantHeld)
		}
	}

	allow("a", true, 0)
	allow("a", false, 0)
	allow("a", false, 0)
	allow("b", true, 0) // not behind a's
	clock = clock.Add(59 * time.Second)
	allow("a", false, 0)
	clock = clock.Add(time.Second)
	allow("a", true, 3)
	allow("b", true, 0) // b's interval ran out too, and nothing was held
	allow("a", false, 0)
	clock = clock.Add(time.Minute)
	allow("a", true, 1) // the one since the last let through, not 4

	ls.Reset()
	allow("a", true, 0)
	allow("b", true, 0)
}

// NewWarnLimiters goes by the wall clock.
func TestNewWarnLimitersUsesTheWallClock(t *testing.T) {
	t.Parallel()
	ls := NewWarnLimiters(time.Hour)
	if ok, _ := ls.Allow("k"); !ok {
		t.Fatal("the first warning was held back")
	}
	if ok, _ := ls.Allow("k"); ok {
		t.Fatal("a second warning within the hour went through")
	}
}

// AllowOr gives each of the first maxOpenKinds keys a kind of its own, and
// limits a key past them as its overflow kind: still let through once per
// interval, and counted there. A key kept before stays its own.
func TestWarnLimitersAllowOrKeepsAtMostMaxOpenKindsApart(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000, 0)
	ls := &WarnLimiters{every: time.Minute, now: func() time.Time { return clock }}
	allowOr := func(key string, wantOK bool, wantHeld uint64, wantKind string) {
		t.Helper()
		if ok, held, kind := ls.AllowOr(key, "overflow"); ok != wantOK || held != wantHeld || kind != wantKind {
			t.Fatalf("AllowOr(%q) = (%v, %d, %q), want (%v, %d, %q)", key, ok, held, kind, wantOK, wantHeld, wantKind)
		}
	}

	for i := 0; i < maxOpenKinds; i++ {
		key := fmt.Sprintf("key-%d", i)
		allowOr(key, true, 0, key)
	}
	allowOr("new", true, 0, "overflow")
	allowOr("newer", false, 0, "overflow")
	allowOr("key-0", false, 0, "key-0")
	clock = clock.Add(time.Minute)
	allowOr("key-0", true, 1, "key-0")
	allowOr("newest", true, 1, "overflow")

	kinds := 0
	ls.kinds.Range(func(_, _ any) bool { kinds++; return true })
	if kinds != maxOpenKinds+1 {
		t.Fatalf("%d kinds kept, want the %d keys and the overflow", kinds, maxOpenKinds)
	}
}

// Callers racing to add the same keys keep exactly maxOpenKinds of them
// apart: a key two callers add at once is counted once, and the bound is
// never passed.
func TestWarnLimitersAllowOrBoundHoldsUnderRacingCallers(t *testing.T) {
	t.Parallel()
	for round := 0; round < 200; round++ {
		ls := NewWarnLimiters(time.Hour)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < 2*maxOpenKinds; i++ {
					ls.AllowOr(fmt.Sprintf("key-%d", i), "overflow")
				}
			}()
		}
		close(start)
		wg.Wait()
		kinds := 0
		ls.kinds.Range(func(k, _ any) bool {
			if k != "overflow" {
				kinds++
			}
			return true
		})
		if kinds != maxOpenKinds || ls.open.Load() != maxOpenKinds {
			t.Fatalf("round %d: %d keys kept apart (%d counted), want %d", round, kinds, ls.open.Load(), maxOpenKinds)
		}
	}
}
