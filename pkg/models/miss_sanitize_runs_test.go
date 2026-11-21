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
		u + "[0]", "1700000000[1]", "x y", "x", "a:b"}
	marks := []string{".", ".", ".", " ", ", ", "[", "[]", "(", ")", ": ", "'", "=", "{", "}", "s"}
	rng := rand.New(rand.NewSource(5143))
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	for c := 0; c < 30000; c++ {
		var paths []string
		var named []string
		for range 1 + rng.Intn(3) {
			n := 1 + rng.Intn(7)
			fs := make([]string, n)
			for k := range fs {
				fs[k] = pick(fields)
			}
			p := strings.Join(fs, ".")
			if FieldShape(p) != p {
				paths = append(paths, p)
				if q := strings.TrimPrefix(p, "body."); q != p && FieldShape(q) != q {
					paths = append(paths, q)
				}
			}
			named = append(named, fs...)
		}
		if len(paths) == 0 {
			continue
		}
		var b strings.Builder
		for range 1 + rng.Intn(14) {
			if rng.Intn(3) > 0 {
				b.WriteString(pick(named))
			} else {
				b.WriteString(pick(fields))
			}
			b.WriteString(pick(marks))
		}
		text := b.String()
		if got, want := shapedRuns(text, paths), shapedRunsByEveryRun(text, paths); got != want {
			t.Fatalf("paths %q, text %q:\nshapedRuns    %q\nevery run     %q", paths, text, got, want)
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

// Shaping a miss's next step costs about what its text and paths are long,
// however deep its paths are: 25 field diffs 120, then 1000, fields deep
// with an id mid-path, the text naming each whole and by runs around the id.
// (Building every run of every path took 1.3 s and 650 MB at 120 fields.)
func TestTheNextStepShapesDeepPathsInLinearCost(t *testing.T) {
	u := "00000001-0000-4000-8000-000000000001"
	for _, c := range []struct {
		depth    int
		maxBytes uint64
		maxTime  time.Duration
	}{
		{120, 4 << 20, 250 * time.Millisecond},
		{1000, 32 << 20, 2 * time.Second},
	} {
		segs := make([]string, c.depth)
		for i := range segs {
			segs[i] = "node"
		}
		segs[c.depth/2] = u
		var diffs []MockFieldDiff
		var named []string
		for n := 0; n < 25; n++ {
			p := "body." + strings.Join(segs, ".") + fmt.Sprintf(".leaf%d", n)
			diffs = append(diffs, MockFieldDiff{Path: p, Kind: DiffKindValueChanged})
			named = append(named, p, strings.Join(segs[c.depth/2-3:c.depth/2+3], "."), strings.Join(segs[2:c.depth-2], "."))
		}
		m := UnmatchedCall{Protocol: "HTTP", NextSteps: "Only values drifted (" + strings.Join(named, ", ") + "). Re-record.", FieldDiffs: diffs}
		out := m.SanitizedNextStep() // warm
		if strings.Contains(out, u) {
			t.Fatalf("depth %d: the id is in the next step: %q", c.depth, out)
		}
		const calls = 3
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		start := time.Now()
		for range calls {
			m.SanitizedNextStep()
		}
		took := time.Since(start) / calls
		runtime.ReadMemStats(&after)
		bytes := (after.TotalAlloc - before.TotalAlloc) / calls
		t.Logf("depth %d: %v and %d KB a call", c.depth, took, bytes>>10)
		if bytes > c.maxBytes || took > c.maxTime {
			t.Fatalf("depth %d: a next step took %v and %d KB; want at most %v and %d KB", c.depth, took, bytes>>10, c.maxTime, c.maxBytes>>10)
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
