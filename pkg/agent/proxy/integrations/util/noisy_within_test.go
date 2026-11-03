package util

import "testing"

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
		{"a pattern with no anchors", []string{`xq7Lm2`}, "k = 'xq7Lm2'", true},
		{"a text without the value", []string{`^xq7Lm2$`}, "SELECT 1 FROM t WHERE k = 'other'", false},
		{"a pattern of anchors alone", []string{`^$`}, "SELECT 1", false},
		{"no patterns", nil, "xq7Lm2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewNoiseChecker(tc.patterns).IsNoisyWithin(tc.value); got != tc.want {
				t.Fatalf("IsNoisyWithin(%q) with %q = %v, want %v", tc.value, tc.patterns, got, tc.want)
			}
		})
	}
}
