package models

import (
	"fmt"
	"math/rand"
	"runtime"
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
// each place after a mark ("}", ":") begins a chain of kept fields. Bytes
// are bounded (they do not move with the runner's load), and so is the
// ratio of the fastest of 5 timings at the deeper depth to the shallower:
// about 8 to 10 when the cost grows with depth, about 64 or more with its
// square, so it must stay under 25. (Building every run of every path took
// 1.3 s and 650 MB a call at 120 fields.)
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
	// cost is the bytes a call allocates and the fastest of 5 timings of
	// reps calls, a call's share.
	cost := func(m UnmatchedCall, reps int) (uint64, time.Duration) {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		m.SanitizedNextStep()
		runtime.ReadMemStats(&after)
		fastest := time.Duration(1<<63 - 1)
		for range 5 {
			start := time.Now()
			for range reps {
				m.SanitizedNextStep()
			}
			fastest = min(fastest, time.Since(start)/time.Duration(reps))
		}
		return after.TotalAlloc - before.TotalAlloc, fastest
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
		// Enough calls that the shallower timing is a few milliseconds.
		reps := 1
		if _, d := cost(low, 1); d < 2*time.Millisecond {
			reps = int(2*time.Millisecond/max(d, time.Microsecond)) + 1
		}
		lowBytes, lowTime := cost(low, reps)
		highBytes, highTime := cost(high, reps)
		ratio := float64(highTime) / float64(max(lowTime, 1))
		t.Logf("%s: depth %d %v and %d KB a call, depth %d %v and %d KB, time x%.1f",
			c.name, c.low, lowTime, lowBytes>>10, c.high, highTime, highBytes>>10, ratio)
		if lowBytes > c.lowMax || highBytes > c.highMax {
			t.Errorf("%s: %d KB at depth %d and %d KB at %d a call; want at most %d KB and %d KB",
				c.name, lowBytes>>10, c.low, highBytes>>10, c.high, c.lowMax>>10, c.highMax>>10)
		}
		if ratio >= 25 {
			t.Errorf("%s: depth %d takes %.1f times what depth %d does (%v, %v); want under 25, as cost that grows with depth",
				c.name, c.high, ratio, c.low, highTime, lowTime)
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
