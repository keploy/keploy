package models

import (
	"fmt"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// shapedRunsByEveryRun is shapedRuns read the plain way: every run of every
// path built and shaped, then the longest at each place. It is the rule
// shapedRuns keeps, and costs about the cube of a path's depth.
func shapedRunsByEveryRun(text string, paths []string) string {
	runs := map[string]string{}
	add := func(run string) {
		if shape := FieldShape(run); shape != run && !countAlone(run) {
			runs[run] = shape
		}
	}
	for _, p := range paths {
		fields := strings.Split(p, ".")
		for i := range fields {
			for j := i + 1; j <= len(fields); j++ {
				run := strings.Join(fields[i:j], ".")
				add(run)
				if k := strings.IndexByte(fields[j-1], '['); k > 0 {
					add(run[:len(run)-len(fields[j-1])+k])
				}
			}
		}
	}
	longest := 0
	for run := range runs {
		longest = max(longest, len(run))
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		end := -1
		if i == 0 || text[i-1] == '.' || wordEnd(text[i-1]) {
			for j := i + 1; j <= min(len(text), i+longest); j++ {
				if j < len(text) && text[j] != '.' && text[j] != '[' && !wordEnd(text[j]) {
					continue
				}
				if _, ok := runs[text[i:j]]; ok {
					end = j
				}
			}
		}
		if end < 0 {
			b.WriteByte(text[i])
			i++
			continue
		}
		b.WriteString(runs[text[i:end]])
		i = end
	}
	return b.String()
}

// A text's runs are shaped as building every run of every path shapes them,
// for any paths and any text: fields that repeat, ids, counts, an id-long
// number, indexes, a header's name, SQL places, keys with a space or a mark,
// an empty field, and texts that name runs, parts of runs and runs of two
// paths side by side.
func TestShapedRunsShapesAsEveryRunWould(t *testing.T) {
	u := "00000001-0000-4000-8000-000000000001"
	fields := []string{"body", "node", "a", "b", "sessions", u, "token", "1", "7", "1700000000", "tags[0]", "tags", "[0]",
		"header", "X-Trace2", "bind", "$1", "literal", "3", "John Smith", "jane@example", "com", "", "line1", "id(x", "k=v", "{}",
		u + "[0]", "1700000000[1]", "x y", "x", "a:b", "{", "{[0]"}
	marks := []string{".", ".", ".", " ", ", ", "[", "[]", "(", ")", ": ", "'", "=", "{", "}", "s"}
	// Few fields, so paths repeat runs of fields (the automaton's shared
	// states), with an SQL place and a header's name to read as one.
	few := []string{"a", "b", u, "7", "literal", "3", "header", "a:b", "x y", u + "[0]", "{}", "{"}
	rng := rand.New(rand.NewSource(5143))
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	for c := 0; c < 60000; c++ {
		alphabet := fields
		if c%2 == 1 {
			alphabet = few
		}
		var paths []string
		var runs [][]string
		for range 1 + rng.Intn(3) {
			n := 1 + rng.Intn(9)
			fs := make([]string, n)
			for k := range fs {
				fs[k] = pick(alphabet)
			}
			p := strings.Join(fs, ".")
			if FieldShape(p) != p {
				paths = append(paths, p)
				if q := strings.TrimPrefix(p, "body."); q != p && FieldShape(q) != q {
					paths = append(paths, q)
				}
			}
			runs = append(runs, fs)
		}
		if len(paths) == 0 {
			continue
		}
		var b strings.Builder
		for range 1 + rng.Intn(10) {
			switch fs := runs[rng.Intn(len(runs))]; rng.Intn(4) {
			case 0:
				b.WriteString(pick(alphabet))
			default:
				// A run of a path: whole, its last field without its
				// indexes, or one field swapped.
				i := rng.Intn(len(fs))
				j := i + 1 + rng.Intn(len(fs)-i)
				run := append([]string(nil), fs[i:j]...)
				switch rng.Intn(4) {
				case 0:
					if k := strings.IndexByte(run[len(run)-1], '['); k > 0 {
						run[len(run)-1] = run[len(run)-1][:k]
					}
				case 1:
					run[rng.Intn(len(run))] = pick(alphabet)
				}
				b.WriteString(strings.Join(run, "."))
			}
			b.WriteString(pick(marks))
		}
		text := b.String()
		if got, want := shapedRuns(text, paths), shapedRunsByEveryRun(text, paths); got != want {
			t.Fatalf("paths %q, text %q:\nshapedRuns    %q\nevery run     %q", paths, text, got, want)
		}
	}
}

// A key "{}" is kept whole, but its start "{" is shaped where a path holds
// "{" as a field, so a run "a.b.{" that a text names inside "a.b.{}" is
// shaped though the longer run "a.b.{}.c" around it is none to shape.
func TestTheNextStepShapesAShapedStartOfAKeptField(t *testing.T) {
	u := "00000001-0000-4000-8000-000000000001"
	v := "00000002-0000-4000-8000-000000000002"
	paths := []string{"b.{}.c." + u, "a.b.{", "a.b.{}." + u}
	text := "a.b.{}.c." + u
	if got, want := shapedRuns(text, paths), shapedRunsByEveryRun(text, paths); got != want || got != "a.b.{}}.c.{}" {
		t.Errorf("shapedRuns %q; every run %q, want a.b.{}}.c.{}", got, want)
	}
	m := UnmatchedCall{Protocol: "HTTP", NextSteps: "entries for a.b.{}.c." + v + " differ", FieldDiffs: []MockFieldDiff{
		{Path: "body.x.a.b.{}." + u, Kind: DiffKindValueChanged},
		{Path: "body.y.a.b.{", Kind: DiffKindValueChanged},
		{Path: "body.z.b.{}.c." + v, Kind: DiffKindValueChanged},
	}}
	if got, want := m.SanitizedNextStep(), "entries for a.b.{}}.c.{} differ"; got != want {
		t.Errorf("next step %q; want %q", got, want)
	}
	// Deeper in a run of kept fields, the start "{" ends the run at the
	// nearest "{}" whose start follows it (the farther one's follows no
	// path). The run of another path that begins inside it ("b.c.{}...7")
	// is then not read from there: the first text's count 7 stays, and the
	// second's is shaped by the run "d.{}.7" after it.
	for _, c := range []struct {
		paths      []string
		text, want string
	}{
		{[]string{"a.b.c.{}", "a.b.c.{", "b.c.{}.7"}, "a.b.c.{}.7", "a.b.c.{}}.7"},
		{[]string{"a.b.c.{}.d.{}", "a.b.c.{", "b.c.{}.d.{}.7"}, "a.b.c.{}.d.{}.7", "a.b.c.{}}.d.{}.{}"},
	} {
		if got, want := shapedRuns(c.text, c.paths), shapedRunsByEveryRun(c.text, c.paths); got != want || got != c.want {
			t.Errorf("paths %q, text %q: shapedRuns %q; every run %q, want %q", c.paths, c.text, got, want, c.want)
		}
	}
}

// A number as long as an id that a path holds is shaped where the text names
// it alone; a shorter one is a count, kept.
func TestTheNextStepShapesAnIDLongNumberAlone(t *testing.T) {
	m := UnmatchedCall{Protocol: "HTTP", NextSteps: `entries for 1700000000 ("^s"), in 3 of 170000000 runs`,
		FieldDiffs: []MockFieldDiff{{Path: "body.series.1700000000.value", Kind: DiffKindValueChanged}, {Path: "body.counts.170000000.n", Kind: DiffKindValueChanged}}}
	if got, want := m.SanitizedNextStep(), `entries for {} ("{}"), in 3 of 170000000 runs`; got != want {
		t.Errorf("next step %q; want %q", got, want)
	}
}

// Shaping a miss's next step grows with its paths' depth, not with its
// square, in three shapes, each at a depth and at about 8 times it: 25
// field diffs with an id mid-path, the text naming each whole and by runs
// around the id (120 and 1000 fields deep); and one diff of keys "" and
// "{}", or "y" and "x:y", nested 500 and 4000 deep and named whole, where
// each place after a mark ("}", ":") begins a chain of kept fields. The
// bytes a call allocates of its own are bounded (ownBytes: they move
// neither with the runner's load nor with -race), and so are two ratios at
// the deeper depth to the shallower: of those bytes, and of the CPU time a
// call runs for by its thread's own clock (threadCPU: time the thread
// waited for a CPU, or that a CPU quota held it back, is no part of it).
// Each is about 8 to 10 when the cost grows with depth, about 64 or more
// with its square, so each must stay under 25. A cost that grows with the
// square while its bytes do not (a chain read again at each place) shows
// only in the CPU time. No wall-clock timing is asserted: a runner's load
// and its CPU quota move it, and a quota can throttle a longer timing and
// spare a shorter one. Where no thread clock is read (Windows: threadCPU),
// the CPU ratio is logged as not measured and the bytes alone are asserted.
// (Building every run of every path took 1.3 s and 650 MB a call at 120
// fields.)
func TestShapingANextStepGrowsWithItsPathsDepthNotItsSquare(t *testing.T) {
	u := "00000001-0000-4000-8000-000000000001"
	diffs25 := func(depth int) UnmatchedCall {
		segs := make([]string, depth)
		for i := range segs {
			segs[i] = "node"
		}
		segs[depth/2] = u
		var diffs []MockFieldDiff
		var named []string
		for n := 0; n < 25; n++ {
			p := "body." + strings.Join(segs, ".") + fmt.Sprintf(".leaf%d", n)
			diffs = append(diffs, MockFieldDiff{Path: p, Kind: DiffKindValueChanged})
			named = append(named, p, strings.Join(segs[depth/2-3:depth/2+3], "."), strings.Join(segs[2:depth-2], "."))
		}
		return UnmatchedCall{Protocol: "HTTP", NextSteps: "Only values drifted (" + strings.Join(named, ", ") + "). Re-record.", FieldDiffs: diffs}
	}
	chain := func(first, key string) func(int) UnmatchedCall {
		return func(depth int) UnmatchedCall {
			p := "body.q." + first + "." + strings.Repeat(key+".", depth) + u
			return UnmatchedCall{Protocol: "HTTP", NextSteps: "Only values drifted (" + p + "). Re-record.", FieldDiffs: []MockFieldDiff{{Path: p, Kind: DiffKindValueChanged}}}
		}
	}
	// cpu is the fastest of 5 readings of the CPU time its thread runs reps
	// calls of m for, a call's share, with the goroutine locked to its
	// thread, or false where no thread clock is read (threadCPU).
	cpu := func(m UnmatchedCall, reps int) (time.Duration, bool) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		fastest := time.Duration(1<<63 - 1)
		for range 5 {
			start, ok := threadCPU()
			if !ok {
				return 0, false
			}
			for range reps {
				m.SanitizedNextStep()
			}
			end, _ := threadCPU()
			fastest = min(fastest, (end-start)/time.Duration(reps))
		}
		return fastest, true
	}
	for _, c := range []struct {
		name            string
		call            func(int) UnmatchedCall
		low, high       int
		lowMax, highMax uint64 // bytes a call
	}{
		{"25 diffs, an id mid-path", diffs25, 120, 1000, 6 << 20, 48 << 20},
		{`keys "" and "{}"`, chain("", "{}"), 500, 4000, 2 << 20, 12 << 20},
		{`keys "y" and "x:y"`, chain("y", "x:y"), 500, 4000, 2 << 20, 12 << 20},
	} {
		low, high := c.call(c.low), c.call(c.high)
		for _, m := range []UnmatchedCall{low, high} {
			if out := m.SanitizedNextStep(); strings.Contains(out, u) {
				t.Fatalf("%s: the id is in the next step: %q", c.name, out)
			}
		}
		lowBytes, lowState := ownBytes(t, func() { low.SanitizedNextStep() })
		highBytes, highState := ownBytes(t, func() { high.SanitizedNextStep() })
		ratio := float64(highBytes) / float64(max(lowBytes, 1))
		t.Logf("%s: depth %d %d KB a call (and %d KB of matcher state), depth %d %d KB (and %d KB), bytes x%.1f",
			c.name, c.low, lowBytes>>10, lowState>>10, c.high, highBytes>>10, highState>>10, ratio)
		if lowBytes > c.lowMax || highBytes > c.highMax {
			t.Errorf("%s: %d KB at depth %d and %d KB at %d a call; want at most %d KB and %d KB",
				c.name, lowBytes>>10, c.low, highBytes>>10, c.high, c.lowMax>>10, c.highMax>>10)
		}
		if ratio >= 25 {
			t.Errorf("%s: depth %d allocates %.1f times what depth %d does (%d KB, %d KB); want under 25, as cost that grows with depth",
				c.name, c.high, ratio, c.low, highBytes>>10, lowBytes>>10)
		}
		// Enough calls that the shallower reading is a few milliseconds.
		reps := 1
		if d, ok := cpu(low, 1); ok && d < 2*time.Millisecond {
			reps = int(2*time.Millisecond/max(d, time.Microsecond)) + 1
		}
		lowCPU, ok := cpu(low, reps)
		if !ok {
			t.Logf("%s: no thread clock is read here; the CPU time's ratio is not measured", c.name)
			continue
		}
		highCPU, _ := cpu(high, reps)
		cpuRatio := float64(highCPU) / float64(max(lowCPU, 1))
		t.Logf("%s: a call's CPU time %v at depth %d, %v at %d, x%.1f", c.name, lowCPU, c.low, highCPU, c.high, cpuRatio)
		if cpuRatio >= 25 {
			t.Errorf("%s: a call at depth %d runs %.1f times the CPU time of one at depth %d (%v, %v); want under 25, as cost that grows with depth",
				c.name, c.high, cpuRatio, c.low, highCPU, lowCPU)
		}
	}
}

// matcherState are the functions in which package regexp builds the state a
// match runs on: a backtracker's, an NFA machine's, a one-pass machine's.
// It keeps that state in a sync.Pool between matches, so a match that finds
// it there allocates none of it. Under -race a sync.Pool drops one put in
// four at random, so a call rebuilds it for about a quarter of its matches:
// by a memory profile, 4.5 to 5.5 MB more a call of keys "y" and "x:y" 500
// deep, all of it in these functions, while the bytes the rest of the call
// allocates stay within 2% of a build without -race.
var matcherState = map[string]bool{
	"regexp.newBitState":       true,
	"regexp.(*bitState).reset": true,
	"regexp.(*bitState).push":  true,
	"regexp.(*Regexp).get":     true,
	"regexp.(*machine).alloc":  true,
	"regexp.(*machine).add":    true,
	"regexp.(*machine).step":   true,
	"regexp.(*machine).clear":  true,
	"regexp.newOnePassMachine": true,
}

// ownBytes is what f allocates, as a memory profile at rate 1 (every
// allocation) counts it while f runs, split by the function that made each
// allocation: own is every byte but regexp's matcher state (matcherState),
// state is that state. The profile must hold at least the bytes the runtime
// counts allocated while f runs (TotalAlloc), or the row fails: a profile
// that misses allocations would make own too small.
func ownBytes(t *testing.T, f func()) (own, state uint64) {
	t.Helper()
	n, _ := runtime.MemProfile(nil, true)
	before := make([]runtime.MemProfileRecord, n+4096)
	after := make([]runtime.MemProfileRecord, n+4096)
	rate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	defer func() { runtime.MemProfileRate = rate }()
	// A profile may be up to two collections old; runtime.GC publishes
	// what was allocated before it, and a second one covers a collection
	// that overlapped the first.
	runtime.GC()
	runtime.GC()
	nb, okBefore := runtime.MemProfile(before, true)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	f()
	runtime.ReadMemStats(&m1)
	runtime.GC()
	runtime.GC()
	na, okAfter := runtime.MemProfile(after, true)
	runtime.MemProfileRate = rate
	if !okBefore || !okAfter {
		t.Fatalf("the memory profile holds more than %d records", len(before))
	}
	bytesByStack := func(recs []runtime.MemProfileRecord) map[[32]uintptr]int64 {
		m := make(map[[32]uintptr]int64, len(recs))
		for _, r := range recs {
			m[r.Stack0] += r.AllocBytes
		}
		return m
	}
	was := bytesByStack(before[:nb])
	for stk, b := range bytesByStack(after[:na]) {
		d := uint64(b - was[stk])
		if matcherState[allocator(stk[:])] {
			state += d
		} else {
			own += d
		}
	}
	if counted := m1.TotalAlloc - m0.TotalAlloc; own+state < counted {
		t.Fatalf("the memory profile holds %d bytes allocated while the call ran; the runtime counts %d", own+state, counted)
	}
	return own, state
}

// allocator is the function that made an allocation whose stack is stk
// (a record's Stack0: it ends at its first 0): its first frame outside the
// runtime.
func allocator(stk []uintptr) string {
	if i := slices.Index(stk, 0); i >= 0 {
		stk = stk[:i]
	}
	frames := runtime.CallersFrames(stk)
	for {
		f, more := frames.Next()
		if !more || !strings.HasPrefix(f.Function, "runtime.") {
			return f.Function
		}
	}
}

// A log line's next step is shaped when the line is written, so a Debug line
// that is off shapes nothing: what it says is the report's next step then.
func TestALogLinesNextStepIsShapedWhenWritten(t *testing.T) {
	r := &MockMismatchReport{NextSteps: "before"}
	s := LazyMissLogNextStep(r)
	r.NextSteps = "check 00000001-0000-4000-8000-000000000001.token"
	r.FieldDiffs = []MockFieldDiff{{Path: "body.sessions.00000001-0000-4000-8000-000000000001.token", Kind: DiffKindValueChanged}}
	if got, want := s.String(), MissLogNextStep(r); got != want || got != "check {}.token" {
		t.Errorf("the line says %q; want %q, the report's next step when written", got, want)
	}
}
