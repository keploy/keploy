package learn

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

// The set's budget is set from the agent's memory limit, not fixed: a
// quarter of the least of the Go runtime's limit, its cgroup's and each
// parent's, and the machine's memory, within its stated bounds. api-server's
// lane caps the agent at 12 GiB: before the agent's hold (holdHeap, half of
// that) a quarter of it is 3 GiB, where the old fixed tiers kept 224 MiB of
// a working set larger than that.
func TestTheSetsBudgetIsAQuarterOfTheAgentsMemoryLimit(t *testing.T) {
	const gib = int64(1) << 30
	dir := t.TempDir()
	write := func(name, text string) string {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	meminfo := write("meminfo", "MemTotal:       56623104 kB\nMemFree:  1000 kB\n")
	v2 := write("v2/cgroup", "0::/lane.slice/agent.scope\n")
	write("fs2/lane.slice/memory.max", fmt.Sprint(12*gib)+"\n")
	write("fs2/lane.slice/agent.scope/memory.max", "max\n")
	v1 := write("v1/cgroup", "12:cpu,cpuacct:/x\n11:memory:/docker/abc\n")
	write("fs1/memory/docker/abc/memory.limit_in_bytes", "9223372036854771712\n")
	write("fs1/memory/docker/memory.limit_in_bytes", fmt.Sprint(6*gib)+"\n")
	none := filepath.Join(dir, "missing")
	for _, c := range []struct {
		name             string
		goLimit          int64
		cgroup, root, mi string
		limit, budget    int64
	}{
		{"a cgroup v2 parent's limit, its own unset (api-server's lane)", math.MaxInt64, v2, filepath.Join(dir, "fs2"), meminfo, 12 * gib, 3 * gib},
		{"the Go runtime's limit, under the cgroup's", 2 * gib, v2, filepath.Join(dir, "fs2"), meminfo, 2 * gib, gib / 2},
		{"a cgroup v1 parent's limit, its own unlimited", math.MaxInt64, v1, filepath.Join(dir, "fs1"), meminfo, 6 * gib, 3 * gib / 2},
		{"the machine's memory, no cgroup limit", math.MaxInt64, none, none, meminfo, 56623104 << 10, maxSetBudget},
		{"no limit known", math.MaxInt64, none, none, none, 0, minSetBudget},
		{"a small limit", 128 << 20, none, none, none, 128 << 20, minSetBudget},
	} {
		got := memoryLimitAt(c.goLimit, c.cgroup, c.root, c.mi)
		if got != c.limit {
			t.Errorf("%s: the limit read is %d, want %d", c.name, got, c.limit)
		}
		if b := budgetOf(got); b != c.budget {
			t.Errorf("%s: the set's budget is %d, want %d", c.name, b, c.budget)
		}
	}
}

// budgetRow reads each payload of row's set for its words (both ways, as
// the word rule does) after the call: the parts of every tier a call reads.
func budgetRow(row heldRow, dc *DriftCache) string {
	got := fmt.Sprint(row.classify(dc))
	for _, x := range row.raw() {
		got += fmt.Sprint(len(dc.searchedTokens(x, false)), len(dc.searchedTokens(x, true)))
	}
	return got
}

// A set whose recordings' words are more than the old words tier (32 MiB,
// the recording read least recently let go first) keeps them all within
// the budget an agent limited to 12 GiB has: each payload is decoded, split
// into its words and read for its instants once for the set, however many
// tests read it. With the old tier, a call read them of every recording in
// the same order and kept none it read again: api-server's late calls
// re-decoded and re-split every payload they read, 99% of their learn.
func TestASetLargerThanTheOldTiersReadsEachPayloadOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("keeps more than 32 MiB of words")
	}
	defer func(b int64) { learnBudget = b }(learnBudget)
	learnBudget = budgetOf(12 << 30)
	const n, words, tests = 1500, 160, 3
	row := newHeldRow(n, words)
	decodes := countDecodes(t, row.raw(), nil)
	spans := countSpans(t)
	tokens := countTokenReads(t)
	tr := NewTracker()
	tr.NewSet()
	tr.Begin()
	first := budgetRow(row, tr.Drifts())
	tr.End()
	_, dec := decodesOf(decodes)
	sp, tok := spans.Load(), tokens.Load()
	if dec != n || tok == 0 || sp == 0 {
		t.Fatalf("the set's first test decoded its %d payloads %d times, split them %d times and read their instants %d times: want each payload decoded once, and split and read", n, dec, tok, sp)
	}
	for i := 0; i < tests; i++ {
		tr.Begin()
		got := budgetRow(row, tr.Drifts())
		tr.End()
		if got != first {
			t.Fatalf("test %d of the set read otherwise than its first", i+2)
		}
	}
	_, d := decodesOf(decodes)
	if d != dec || tokens.Load() != tok || spans.Load() != sp {
		t.Errorf("%d more tests of the set decoded its payloads %d more times, split them %d more times and read their instants %d more times: want none (once for the set, %d MiB kept of a %d MiB budget)", tests, d-dec, tokens.Load()-tok, spans.Load()-sp, tr.derived.usedBytes()>>20, learnBudget>>20)
	}
	if w := tr.derived.keptBytes(); w <= 32<<20 {
		t.Errorf("the set keeps %d MiB of words: want all of them, more than the old tier's 32 MiB", w>>20)
	}
}

// A set past its budget keeps no more than its budget, in all its tiers
// together, and the heap its learn step keeps grows with that, not with
// the set: it reads the rest once per test, and learns what a set within
// its budget learns. Its earlier set's data is let go when the next set
// starts (NewSet), so the agent holds one set's budget at a time.
func TestASetPastItsBudgetKeepsItsHeapWithinIt(t *testing.T) {
	if testing.Short() {
		t.Skip("reads a set of 32 MiB of words")
	}
	defer func(b, x int64) { learnBudget, textBudget = b, x }(learnBudget, textBudget)
	const n, words, tests = 800, 160, 3
	row := newHeldRow(n, words)
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	learnBudget = 1 << 40
	whole := NewTracker()
	whole.NewSet()
	whole.Begin()
	want := budgetRow(row, whole.Drifts())
	whole.End()
	ended := whole.derived
	need := ended.usedBytes()
	keptFirst := ended.held.used + ended.forms.used
	whole.NewSet()
	if u := ended.usedBytes(); u != 0 {
		t.Errorf("a set holds %d bytes of its derived data once the next set started: want none", u)
	}
	runtime.KeepAlive(whole)
	// A budget past the words' and another below what held and forms, which
	// let none go, need alone.
	for _, budget := range []int64{4 << 20, keptFirst / 2} {
		if need < 4*budget || budget == 0 {
			t.Fatalf("the set needs %d MiB, %d KiB of it held and forms: the row needs one four times past a budget of %d KiB", need>>20, keptFirst>>10, budget>>10)
		}
		learnBudget, textBudget = budget, 1<<20
		before := heap()
		tr := NewTracker()
		tr.NewSet()
		var peak int64
		for i := 0; i <= tests; i++ {
			tr.Begin()
			dc := tr.Drifts()
			got := budgetRow(row, dc)
			if h := heap() - before; h > peak {
				peak = h
			}
			runtime.KeepAlive(dc)
			tr.End()
			if got != want {
				t.Fatalf("test %d of a set past its budget of %d KiB read otherwise than one within it", i+1, budget>>10)
			}
			if u := tr.derived.usedBytes(); u > budget {
				t.Errorf("test %d: the set keeps %d bytes in its tiers, past its budget %d", i+1, u, budget)
			}
		}
		if bound := 2*budget + 2*textBudget + 8<<20; peak > bound {
			t.Errorf("a set needing %d MiB, within a budget of %d KiB, grew the heap %d MiB: past %d MiB (twice the budget and the test's texts' budget, and 8 MiB)", need>>20, budget>>10, peak>>20, bound>>20)
		}
		t.Logf("the set needs %d MiB (%d KiB held and forms); within a budget of %d KiB the heap grew at most %d MiB", need>>20, keptFirst>>10, budget>>10, peak>>20)
	}
}

// The agent's heap is held under half of its memory limit where nothing
// else limits it: it shares that limit with what it serves, and without a
// limit the Go heap doubles what the agent keeps, a set's budget and the
// rest of its own. A GOMEMLIMIT the user gave, or one the memory guard set,
// is theirs and stays; with no limit known it holds nothing.
func TestTheAgentsHeapIsHeldUnderHalfItsLimit(t *testing.T) {
	const gib = int64(1) << 30
	for _, c := range []struct {
		name       string
		env        string
		cur, limit int64
		hold       int64
	}{
		{"no limit set: half of the agent's (enterprise-ui's 12 GiB lane)", "", math.MaxInt64, 12 * gib, 6 * gib},
		{"the user's GOMEMLIMIT", "8GiB", 8 * gib, 12 * gib, 0},
		{"the user's GOMEMLIMIT=off", "off", math.MaxInt64, 12 * gib, 0},
		{"the memory guard's limit", "", 10 * gib, 12 * gib, 0},
		{"no limit known", "", math.MaxInt64, 0, 0},
	} {
		if got := heapHoldFor(c.env, c.cur, c.limit); got != c.hold {
			t.Errorf("%s: the hold is %d, want %d", c.name, got, c.hold)
		}
	}
}

// The hold is set once: a later set's start finds the runtime limited and
// holds nothing more. GOMEMLIMIT the user gave, or no limit known, holds
// nothing. The hold is the Go limit the set's budget reads: a quarter of it.
func TestTheHeapHoldIsSetOnceAndTheBudgetIsAQuarterOfIt(t *testing.T) {
	const gib = int64(1) << 30
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	debug.SetMemoryLimit(math.MaxInt64)
	holdHeapWith("8GiB", func() int64 { return 12 * gib })
	holdHeapWith("", func() int64 { return 0 })
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Fatalf("the user's GOMEMLIMIT, or no limit known, left the Go runtime's limit at %d, want none", got)
	}
	holdHeapWith("", func() int64 { return 12 * gib })
	if got := debug.SetMemoryLimit(-1); got != 6*gib {
		t.Fatalf("the Go runtime's limit is %d after the first set starts, want the hold %d", got, 6*gib)
	}
	holdHeapWith("", func() int64 { return 20 * gib })
	if got := debug.SetMemoryLimit(-1); got != 6*gib {
		t.Errorf("a second set moved the hold to %d, want it set once at %d", got, 6*gib)
	}
	dir := t.TempDir()
	cg := filepath.Join(dir, "cgroup")
	if err := os.WriteFile(cg, []byte("0::/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(fmt.Sprint(12*gib)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	limit := memoryLimitAt(debug.SetMemoryLimit(-1), cg, dir, filepath.Join(dir, "missing"))
	if limit != 6*gib || budgetOf(limit) != 3*gib/2 {
		t.Errorf("in a 12 GiB cgroup under the hold the limit read is %d and the budget %d, want the hold %d and a quarter of it %d", limit, budgetOf(limit), 6*gib, 3*gib/2)
	}
}

// Each set's start holds the agent's heap (setBudget), at half of the
// limit the agent reads, where the user gave no GOMEMLIMIT, and the set's
// budget is a quarter of the hold; GOMEMLIMIT the user gave holds nothing.
func TestASetsStartHoldsTheAgentsHeap(t *testing.T) {
	limit := memoryLimit()
	if limit == 0 {
		t.Skip("no memory limit readable here")
	}
	prevLimit, prevBudget := debug.SetMemoryLimit(-1), learnBudget
	t.Cleanup(func() {
		debug.SetMemoryLimit(prevLimit)
		learnBudget = prevBudget
	})
	learnBudget = 0
	debug.SetMemoryLimit(math.MaxInt64)
	limit = memoryLimit()
	t.Setenv("GOMEMLIMIT", "off")
	if b := setBudget(); b != budgetOf(limit) {
		t.Errorf("a set's budget under the user's GOMEMLIMIT=off is %d, want a quarter of the agent's limit %d", b, budgetOf(limit))
	}
	if got := debug.SetMemoryLimit(-1); got != math.MaxInt64 {
		t.Errorf("a set started under the user's GOMEMLIMIT=off set the Go runtime's limit to %d, want none", got)
	}
	t.Setenv("GOMEMLIMIT", "")
	if b := setBudget(); b != budgetOf(limit/2) {
		t.Errorf("a set's budget is %d under the hold, want a quarter of the hold %d", b, budgetOf(limit/2))
	}
	if got := debug.SetMemoryLimit(-1); got != limit/2 {
		t.Errorf("a set's start set the Go runtime's limit to %d, want half of the agent's %d", got, limit)
	}
}
