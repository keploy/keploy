package supervisor

import (
	"fmt"
	"sync"
	"sync/atomic"
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
		if kinds != maxOpenKinds || ls.open != maxOpenKinds {
			t.Fatalf("round %d: %d keys kept apart (%d counted), want %d", round, kinds, ls.open, maxOpenKinds)
		}
	}
}

// A key one caller is still adding takes no room from another key: with room
// left for two more kinds, a key two callers add at once takes one of them,
// and a third caller's new key takes the other. And a caller that finds the
// last room taken by a key it is adding too is limited as that key, not as
// overflow. The first caller is held as it goes to add its key, while the
// others call.
func TestWarnLimitersAllowOrAKeyBeingAddedTakesItsRoomOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		room int    // kinds left to add before the bound
		next string // the third caller's new key, after the other two
		want string // the kind it is limited as
	}{
		{name: "two rooms: the next key gets the other", room: 2, next: "m", want: "m"},
		{name: "one room: the next key is overflow", room: 1, next: "m", want: "overflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := time.Unix(1_000, 0)
			ls := &WarnLimiters{every: time.Minute, now: func() time.Time { return clock }}
			for i := 0; i < maxOpenKinds-tc.room; i++ {
				ls.AllowOr(fmt.Sprintf("key-%d", i), "overflow")
			}
			held, release := make(chan struct{}), make(chan struct{})
			releaseFirst := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseFirst) // a failed row does not leave the first caller held
			var first atomic.Bool
			ls.adding = func(key string) {
				if key == "k" && first.CompareAndSwap(false, true) {
					close(held)
					<-release
				}
			}
			type result struct {
				ok   bool
				kind string
			}
			firstDone := make(chan result, 1)
			go func() {
				ok, _, kind := ls.AllowOr("k", "overflow")
				firstDone <- result{ok, kind}
			}()
			<-held

			if ok, _, kind := ls.AllowOr("k", "overflow"); !ok || kind != "k" {
				t.Fatalf("the second caller of k, beside the first: AllowOr = (%v, %q), want (true, %q)", ok, kind, "k")
			}
			if tc.room > 1 {
				if _, _, kind := ls.AllowOr(tc.next, "overflow"); kind != tc.want {
					t.Fatalf("a new key %q while k is being added: limited as %q, want %q", tc.next, kind, tc.want)
				}
			}
			releaseFirst()
			if r := <-firstDone; r.ok || r.kind != "k" {
				t.Fatalf("the first caller of k: AllowOr = (%v, %q), want (false, %q): held back behind the second's", r.ok, r.kind, "k")
			}
			if tc.room == 1 {
				if _, _, kind := ls.AllowOr(tc.next, "overflow"); kind != tc.want {
					t.Fatalf("a new key %q once k took the last room: limited as %q, want %q", tc.next, kind, tc.want)
				}
			}

			clock = clock.Add(time.Minute)
			if ok, heldBack, kind := ls.AllowOr("k", "overflow"); !ok || heldBack != 1 || kind != "k" {
				t.Fatalf("k past the interval: AllowOr = (%v, %d, %q), want (true, 1, %q): the first caller's is counted under k", ok, heldBack, kind, "k")
			}
			kinds := 0
			ls.kinds.Range(func(k, _ any) bool {
				if k != "overflow" {
					kinds++
				}
				return true
			})
			if kinds != maxOpenKinds {
				t.Fatalf("%d keys kept apart, want %d", kinds, maxOpenKinds)
			}
		})
	}
}
