package util

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// IsNoisyWithin finds a pattern's value in a text that holds it among other
// text, with the pattern's own anchors (of each alternative too) dropped,
// and never reads a pattern of anchors alone as one that matches any text.
func TestIsNoisyWithin(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
		value    string
		want     bool
	}{
		{"the value alone", []string{`^xq7Lm2$`}, "xq7Lm2", true},
		{"the value inside a statement", []string{`^xq7Lm2$`}, "SELECT 1 FROM t WHERE k = 'xq7Lm2'", true},
		{"after a scheme", []string{`^xq7Lm2$`}, "Bearer xq7Lm2", true},
		{"an alternative of an alternation", []string{`^aaa$|^xq7Lm2$`}, "k = 'xq7Lm2'", true},
		{"a group", []string{`(^xq7Lm2$)`}, "k = 'xq7Lm2'", true},
		{"an alternation after a shared begin anchor", []string{`^(?:aaa$|xq7Lm2$)`}, "k = 'xq7Lm2'", true},
		{"a pattern with no anchors", []string{`xq7Lm2`}, "k = 'xq7Lm2'", true},
		{"the value of a pattern after another", []string{`^aaa$`, `^bbb$`, `^xq7Lm2$`}, "k = 'xq7Lm2'", true},
		{"a text without the value", []string{`^xq7Lm2$`}, "SELECT 1 FROM t WHERE k = 'other'", false},
		{"a pattern of anchors alone", []string{`^$`}, "SELECT 1", false},
		{"the value inside XML", []string{`^REDACTED_abc$`}, `<a>REDACTED_abc</a>`, true},
		{"a quoted dollar is the value's", []string{`REDACTED_abc\$`}, `pay REDACTED_abc$ now`, true},
		{"a quoted dollar is not an anchor", []string{`REDACTED_abc\$`}, `pay REDACTED_abc now`, false},
		{"a quoted backslash before the anchor", []string{`^REDACTED\\$`}, `x REDACTED\ y`, true},
		{"a pattern that does not compile", []string{`^(REDACTED$`}, `(REDACTED`, false},
		{"no patterns", nil, "xq7Lm2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewNoiseChecker(tc.patterns).IsNoisyWithin(tc.value); got != tc.want {
				t.Fatalf("IsNoisyWithin(%q) with %q = %v, want %v", tc.value, tc.patterns, got, tc.want)
			}
		})
	}
}

var withinRun atomic.Int64

// A pattern is compiled without its anchors once, however many values and
// checkers (a recording's, a copy's) read it.
func TestIsNoisyWithinCompilesAPatternOnce(t *testing.T) {
	const n = 20
	run := withinRun.Add(1)
	patterns := make([]string, n)
	for i := range patterns {
		patterns[i] = fmt.Sprintf("^REDACTED_once%d_%d$", run, i)
	}
	compiles := UnanchoredCompiles()
	for checker := 0; checker < 3; checker++ {
		nc := NewNoiseChecker(patterns)
		for i := 0; i < 5; i++ {
			if !nc.IsNoisyWithin(fmt.Sprintf("k = 'REDACTED_once%d_%d'", run, i)) || nc.IsNoisyWithin("k = 'other'") {
				t.Fatal("the row is of values the patterns name, and one they do not")
			}
		}
	}
	if got := UnanchoredCompiles() - compiles; got != n {
		t.Errorf("%d patterns read by 3 checkers were compiled without their anchors %d times, want %d", n, got, n)
	}
}

// A pattern that does not compile is kept as one (getCachedRegexp), never
// read within a text, and never compiled without its anchors.
func TestIsNoisyWithinReadsNoPatternThatDoesNotCompile(t *testing.T) {
	pat := fmt.Sprintf("^(unclosed%d$", withinRun.Add(1))
	compiles := UnanchoredCompiles()
	for i := 0; i < 2; i++ {
		if NewNoiseChecker([]string{pat}).IsNoisyWithin("(unclosed") {
			t.Fatal("a pattern that does not compile matched")
		}
	}
	noiseCacheMu.RLock()
	re, kept := noiseCache[pat]
	noiseCacheMu.RUnlock()
	if !kept || re != nil {
		t.Errorf("a pattern that does not compile is not kept as one (kept %v, %v)", kept, re)
	}
	if got := UnanchoredCompiles() - compiles; got != 0 {
		t.Errorf("a pattern that does not compile was compiled without its anchors %d times", got)
	}
}

// The patterns compiled without their anchors are bounded: a full cache is
// emptied.
func TestIsNoisyWithinPatternsAreBounded(t *testing.T) {
	run := withinRun.Add(1)
	for i := 0; i <= maxNoiseCacheSize; i++ {
		_ = NewNoiseChecker([]string{fmt.Sprintf("^bounded%d_%d$", run, i)}).IsNoisyWithin("x")
		unanchoredMu.RLock()
		n := len(unanchoredCache)
		unanchoredMu.RUnlock()
		if n > maxNoiseCacheSize {
			t.Fatalf("%d patterns compiled without their anchors are kept, more than %d", n, maxNoiseCacheSize)
		}
	}
}
