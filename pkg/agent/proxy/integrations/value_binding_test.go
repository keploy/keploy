package integrations

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// A binding is made once: a recorded value has one live value and a live value
// stands for one recorded value, until Reset. A second value is refused: with
// several live values for a recorded one, an answer has to choose between
// them, and can choose another request's.
func TestBindingTable_BindsOnce(t *testing.T) {
	var tb BindingTable
	if _, ok := tb.Live("r1"); ok {
		t.Fatal("an empty table binds nothing")
	}
	if !tb.ClaimAndBind("", map[string]string{"r1": "l1"}) {
		t.Fatal("r1 -> l1 must bind")
	}
	if !tb.ClaimAndBind("", map[string]string{"r1": "l1"}) {
		t.Fatal("a pair the table already holds is no conflict")
	}
	for name, pairs := range map[string]map[string]string{
		"a second live value for a recorded one": {"r1": "l1b"},
		"a second recorded value for a live one": {"r3": "l1"},
		"a value bound to itself":                {"r4": "r4"},
		"an empty recorded value":                {"": "x"},
		"an empty live value":                    {"y": ""},
		"a recorded value that is a live one":    {"l1": "l5"},
		"a live value that is a recorded one":    {"r5": "r1"},
		"one live value for two recorded ones":   {"r6": "l6", "r7": "l6"},
		"a chain within one request":             {"r6": "r7", "r7": "l7"},
	} {
		if tb.ClaimAndBind("", pairs) {
			t.Errorf("%s must be refused: %v", name, pairs)
		}
	}
	if v, _ := tb.Live("r1"); v != "l1" {
		t.Fatalf("Live(r1) = %q, want l1: a refused pair changes nothing", v)
	}
	if r, _ := tb.Recorded("l1"); r != "r1" {
		t.Fatalf("Recorded(l1) = %q, want r1", r)
	}
	if _, ok := tb.Recorded("l1b"); ok {
		t.Fatal("a refused live value stands for nothing")
	}
	if tb.Len() != 1 {
		t.Fatalf("the table holds %d bindings, want the one admitted", tb.Len())
	}

	tb.ClaimAndBind("create-1", nil)
	if !tb.Claimed("create-1") || tb.Claimed("create-2") {
		t.Fatal("a claimed creator, and only it, reads as claimed")
	}
	tb.Reset()
	if tb.Len() != 0 || tb.Claimed("create-1") {
		t.Fatal("Reset must drop every binding and claim")
	}
	if !tb.ClaimAndBind("", map[string]string{"r1": "l1b"}) {
		t.Fatal("after a Reset the recorded value is free again")
	}
}

// One request's pairs are bound together or not at all: an answer must never
// name one operation's entities by ids from two runs.
func TestBindingTable_BindsARequestWholeOrNotAtAll(t *testing.T) {
	var tb BindingTable
	tb.ClaimAndBind("", map[string]string{"r1": "l1"})
	if tb.ClaimAndBind("", map[string]string{"r2": "l2", "r1": "other", "r3": "l3"}) {
		t.Fatal("a request with one pair that conflicts must be refused")
	}
	for _, rec := range []string{"r2", "r3"} {
		if v, ok := tb.Live(rec); ok {
			t.Fatalf("%s was bound to %q by a request that was refused", rec, v)
		}
	}
	if !tb.ClaimAndBind("", map[string]string{"r2": "l2", "r1": "l1", "r3": "l3"}) || tb.Len() != 3 {
		t.Fatalf("a request that agrees with the table binds what is new: %d bindings", tb.Len())
	}
}

// However many callers race to bind one recorded value, exactly one wins, and
// every loser is told so.
func TestBindingTable_ConcurrentBindKeepsOneValue(t *testing.T) {
	var tb BindingTable
	var wg sync.WaitGroup
	var won atomic.Int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if tb.ClaimAndBind("", map[string]string{"rec": fmt.Sprintf("live-%02d", i)}) {
				won.Add(1)
			}
			_, _ = tb.Live("rec")
		}(i)
	}
	wg.Wait()
	live, ok := tb.Live("rec")
	if !ok || won.Load() != 1 || tb.Len() != 1 {
		t.Fatalf("rec must be bound exactly once: bound=%v winners=%d bindings=%d", ok, won.Load(), tb.Len())
	}
	if r, _ := tb.Recorded(live); r != "rec" {
		t.Fatalf("the winning value %q must stand for rec", live)
	}
}

// A view reads the table, and what a match commits through it is stored by the
// store's own function — which drops it once the epoch the view was taken in
// has ended — and reported back.
func TestBindings_View(t *testing.T) {
	var tb BindingTable
	epoch := uint64(7)
	view := func() *Bindings {
		return NewBindings(&tb, epoch, func(creator string, pairs map[string]string, e uint64) bool {
			return e == epoch && tb.ClaimAndBind(creator, pairs)
		})
	}
	b := view()
	if b.Active() {
		t.Fatal("an empty table is not active")
	}
	if !b.Commit("", nil) {
		t.Fatal("a match that established nothing has nothing to be refused")
	}
	if !b.Commit("create", map[string]string{"r1": "l1"}) {
		t.Fatal("the first value for r1 must be stored")
	}
	if !b.Active() || !b.Claimed("create") {
		t.Fatal("the view must read what was committed")
	}
	if v, _ := b.Live("r1"); v != "l1" {
		t.Fatalf("Live(r1) = %q, want l1", v)
	}
	if r, _ := b.Recorded("l1"); r != "r1" {
		t.Fatalf("Recorded(l1) = %q, want r1", r)
	}
	if b.Commit("retry", map[string]string{"r1": "l2"}) {
		t.Fatal("a second value for r1 must be reported as refused")
	}
	if b.Claimed("retry") {
		t.Fatal("a refused commit claims nothing")
	}

	stale := view()
	epoch++
	if stale.Commit("late", map[string]string{"r3": "l3"}) {
		t.Fatal("a commit from an ended epoch must be reported as dropped")
	}
	if _, ok := tb.Live("r3"); ok || tb.Claimed("late") {
		t.Fatal("a commit from an ended epoch is dropped")
	}
}

// A creator answers the one request it was recorded for. Claiming it and
// binding what that request made is one step, so of two requests that reach
// an unclaimed creator at once only the first is its own: the second can
// repeat what the table holds, and brings nothing new.
func TestBindingTable_AClaimedCreatorTakesNoNewID(t *testing.T) {
	var tb BindingTable
	if !tb.ClaimAndBind("create", map[string]string{"r1": "l1"}) {
		t.Fatal("the first request claims the creator and binds its id")
	}
	if !tb.ClaimAndBind("create", map[string]string{"r1": "l1"}) || !tb.ClaimAndBind("create", nil) {
		t.Fatal("the same request again holds")
	}
	if tb.ClaimAndBind("create", map[string]string{"r1": "l2"}) || tb.ClaimAndBind("create", map[string]string{"r2": "l2"}) {
		t.Fatal("a claimed creator must refuse an id the table does not hold")
	}
	if tb.Len() != 1 {
		t.Fatalf("a refused request bound something: %d bindings", tb.Len())
	}

	// A creator claimed by a request that made nothing anew (it sent the
	// recorded id) is spent all the same.
	if !tb.ClaimAndBind("fixture", nil) {
		t.Fatal("claim")
	}
	if tb.ClaimAndBind("fixture", map[string]string{"r9": "l9"}) {
		t.Fatal("a creator claimed as recorded must not bind a later request's id")
	}

	// Of many requests racing for one creator, each with its own id, one wins.
	var race BindingTable
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if race.ClaimAndBind("c", map[string]string{"rec": fmt.Sprintf("live-%02d", i)}) {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 || race.Len() != 1 {
		t.Fatalf("%d requests bound their id, %d bindings: want one", won.Load(), race.Len())
	}
}

// An id the app sent as recorded while nothing was bound to it is not made
// anew by this run, and is never bound. A binding that is disowned keeps
// answering the request that names its live value, and stops speaking for the
// rest. Reset forgets both.
func TestBindingTable_SentAsRecordedAndDisowned(t *testing.T) {
	var tb BindingTable
	tb.SentAsRecorded("fixture")
	if !tb.NeverBound("fixture") || tb.NeverBound("other") {
		t.Fatal("only the id that was sent as recorded is never bound")
	}
	if tb.ClaimAndBind("", map[string]string{"fixture": "l1"}) || tb.ClaimAndBind("", map[string]string{"x": "lx", "fixture": "l1"}) {
		t.Fatal("an id sent as recorded must not be bound, nor a request that would bind it")
	}

	if !tb.ClaimAndBind("", map[string]string{"r1": "l1"}) {
		t.Fatal("bind")
	}
	tb.SentAsRecorded("r1") // bound already: says nothing about it
	if tb.NeverBound("r1") {
		t.Fatal("an id that is bound is not marked by being sent as recorded")
	}
	if v, ok := tb.Default("r1"); !ok || v != "l1" {
		t.Fatalf("Default(r1) = %q, %v", v, ok)
	}
	if tb.Disown("nothing-bound") {
		t.Fatal("there is no binding to disown")
	}
	if !tb.Disown("r1") || tb.Disown("r1") {
		t.Fatal("a binding is disowned once")
	}
	if _, ok := tb.Default("r1"); ok {
		t.Fatal("a disowned binding does not speak by default")
	}
	if v, _ := tb.Live("r1"); v != "l1" {
		t.Fatal("it still answers the request that names its live value")
	}
	if r, _ := tb.Recorded("l1"); r != "r1" {
		t.Fatal("and still maps its live value back")
	}

	if !tb.FirstNote("k") || tb.FirstNote("k") || !tb.FirstNote("other") {
		t.Fatal("a note is first once")
	}
	tb.Reset()
	if tb.NeverBound("fixture") || !tb.FirstNote("k") {
		t.Fatal("Reset must forget what was sent as recorded and what was noted")
	}
	if !tb.ClaimAndBind("", map[string]string{"r1": "l1"}) {
		t.Fatal("bind after Reset")
	}
	if _, ok := tb.Default("r1"); !ok {
		t.Fatal("Reset must forget what was disowned")
	}
}
