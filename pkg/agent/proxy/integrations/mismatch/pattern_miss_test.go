package mismatch

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// ValueShapes holds each distinct shape once, the first three with how many
// values have each, and counts the values past them, merged or added one by
// one; its zero value names none.
func TestValueShapes(t *testing.T) {
	merged := ShapesOf("a", "b", "a")
	merged.merge(ShapesOf("c", "d", "b", "d"))
	saturated := ShapesOf("a", "b", "c", "d", "e")
	saturated.merge(ShapesOf("a", "f"))
	// A shape both name: its values are counted, each.
	shared := ShapesOf("a")
	shared.merge(ShapesOf("a", "a"))
	// Values past the shapes the merged one names are counted too.
	counted := ShapesOf("a")
	counted.merge(ShapesOf("b", "c", "d", "e"))
	for _, c := range []struct {
		name   string
		s      ValueShapes
		shapes []string
		len    int
		text   string
	}{
		{"none", ValueShapes{}, []string{}, 0, ""},
		{"one", ShapesOf("a"), []string{"a"}, 1, "a"},
		{"one shape twice", ShapesOf("a", "a"), []string{"a"}, 2, "a"},
		{"three shapes", ShapesOf("a", "b", "c"), []string{"a", "b", "c"}, 3, "a, b, c"},
		{"past three shapes", ShapesOf("a", "b", "c", "d", "a", "e", "d"), []string{"a", "b", "c"}, 7, "a, b, c and 3 more"},
		{"merged", merged, []string{"a", "b", "c"}, 7, "a, b, c and 2 more"},
		{"merged past three shapes", saturated, []string{"a", "b", "c"}, 7, "a, b, c and 3 more"},
		{"merged, a shape both name", shared, []string{"a"}, 3, "a"},
		{"merged, values past the shapes the merged one names", counted, []string{"a", "b", "c"}, 5, "a, b, c and 2 more"},
	} {
		if got := c.s.Shapes(); !slices.Equal(got, c.shapes) {
			t.Errorf("%s: shapes %q; want %q", c.name, got, c.shapes)
		}
		if got := c.s.Len(); got != c.len {
			t.Errorf("%s: %d values; want %d", c.name, got, c.len)
		}
		if got := c.s.String(); got != c.text {
			t.Errorf("%s: named %q; want %q", c.name, got, c.text)
		}
	}
	// A copy is its own: adding to it leaves the original as it was.
	orig := ShapesOf("a")
	cp := orig
	cp.Add("b")
	cp.merge(ShapesOf("a"))
	if orig.Len() != 1 || orig.String() != "a" {
		t.Errorf("adding to a copy changed the original: %q, %d values", orig.String(), orig.Len())
	}
}

// However many values of a field miss its patterns, named in one miss or
// in many (an array's elements share a path), the hint and the facts the
// builder holds stay as small as with a few: a hint names each distinct
// shape of a side once, the first three, and counts the values past them.
func TestTheHintStaysSmallHoweverManyValuesMiss(t *testing.T) {
	// maxHint bounds the hint of one field: its text without the values is
	// under 300 bytes, and it names at most three shapes a side and a count.
	const maxHint = 600
	shapes := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("a string of %d bytes", i+2)
		}
		return out
	}
	drift := models.MockFieldDiff{Path: "body.ids[]", Kind: models.DiffKindValueChanged, Expected: "1", Actual: "22"}
	for _, n := range []int{1, 4, 10, 3000} {
		many := make([]PatternMiss, n)
		for i, s := range shapes(n) {
			many[i] = PatternMiss{Path: "body.ids[]", Entry: "ids", Patterns: []string{`^\d$`}, Live: ShapesOf(s), Recorded: ShapesOf(s)}
		}
		for _, c := range []struct {
			name   string
			misses []PatternMiss
		}{
			{"named once", []PatternMiss{{Path: "body.ids[]", Entry: "ids", Patterns: []string{`^\d$`}, Live: ShapesOf(shapes(n)...), Recorded: ShapesOf(shapes(n)...)}}},
			{"named once per value", many},
		} {
			b := NewReport(ProtocolHTTP, "POST /x").WithClosest("mock-1", []models.MockFieldDiff{drift}).WithPatternMisses(c.misses...)
			if len(b.noise.guarded) != 1 {
				t.Errorf("%d values, %s: the builder holds %d facts of one field; want 1", n, c.name, len(b.noise.guarded))
			}
			r := b.Build()
			if len(r.NextSteps) > maxHint {
				t.Errorf("%d values, %s: the hint is %d bytes; want at most %d: %q", n, c.name, len(r.NextSteps), maxHint, r.NextSteps)
			}
			for _, side := range []string{"live", "recorded"} {
				want := fmt.Sprintf("the %s value (a string of 2 bytes)", side)
				switch {
				case n > 3:
					want = fmt.Sprintf("the %s values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and %d more)", side, n-3)
				case n > 1:
					want = fmt.Sprintf("the %s values (%s)", side, strings.Join(shapes(n), ", "))
				}
				if !strings.Contains(r.NextSteps, want) {
					t.Errorf("%d values, %s: hint %q; want it to name %q", n, c.name, r.NextSteps, want)
				}
			}
		}
	}
}

// A pattern miss's reason names its patterns and each side's values outside
// them by their few shapes, a value or many, and stays small however many
// namings of the field are folded into it; folding keeps the field's own
// entry and patterns, and a container on either side.
func TestAPatternMissWhyNamesEachSide(t *testing.T) {
	const lets = "keploy.yml's noise lets body.ids[] vary only within \"^\\\\d$\"; "
	pats := []string{`^\d$`}
	for _, c := range []struct {
		name           string
		live, recorded ValueShapes
		want           string
	}{
		{"one live value", ShapesOf("a string of 2 bytes"), ValueShapes{}, lets + "the live value (a string of 2 bytes) is outside it, so it is compared"},
		{"one recorded value", ValueShapes{}, ShapesOf("a number of 3 characters"), lets + "the recorded value (a number of 3 characters) is outside it, so it is compared"},
		{"two live values of one shape", ShapesOf("a string of 2 bytes", "a string of 2 bytes"), ValueShapes{}, lets + "the live values (a string of 2 bytes) are outside it, so it is compared"},
		{"a value of each side", ShapesOf("a string of 2 bytes"), ShapesOf("a string of 3 bytes"), lets + "the live value (a string of 2 bytes) and the recorded value (a string of 3 bytes) are outside it, so it is compared"},
		{"many of each side", ShapesOf("a string of 2 bytes", "a string of 3 bytes", "a string of 4 bytes", "a string of 5 bytes"), ShapesOf("a string of 6 bytes", "a string of 7 bytes"), lets + "the live values (a string of 2 bytes, a string of 3 bytes, a string of 4 bytes and 1 more) and the recorded values (a string of 6 bytes, a string of 7 bytes) are outside it, so it is compared"},
		{"none", ValueShapes{}, ValueShapes{}, lets + "a value is outside it, so it is compared"},
	} {
		if got := (PatternMiss{Path: "body.ids[]", Patterns: pats, Live: c.live, Recorded: c.recorded}).Why("body.ids[]"); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
	m := PatternMiss{Path: "body.ids[]", Entry: "ids", Patterns: pats, Live: ShapesOf("a string of 2 bytes")}
	for i := 0; i < 3000; i++ {
		m.Fold(PatternMiss{Path: "body.ids[]", Entry: "other", Patterns: []string{`^x$`}, Live: ShapesOf(fmt.Sprintf("a string of %d bytes", i%60+2)), Recorded: ShapesOf(fmt.Sprintf("a string of %d bytes", i%50+2)), Container: i == 2999})
	}
	if m.Entry != "ids" || !slices.Equal(m.Patterns, pats) || !m.Container || m.Live.Len() != 3001 || m.Recorded.Len() != 3000 {
		t.Fatalf("folded: entry %q, patterns %v, container %v, %d live and %d recorded values; want ids, %v, a container, 3001 and 3000", m.Entry, m.Patterns, m.Container, m.Live.Len(), m.Recorded.Len(), pats)
	}
	if why := m.Why("body.ids[]"); len(why) > 400 || !strings.Contains(why, "and 2850 more) and the recorded values (") {
		t.Errorf("the folded reason is %d bytes: %q; want at most 400, counting the values past three shapes", len(why), why)
	}
}
