package pathsafe

import (
	"fmt"
	"strings"
	"testing"
)

// TestValidateSingleSegment is the shared table for the predicate both
// DebugFileSink.RotateForScope and MockYaml.DeleteMocksForSet enforce on the
// test-set ID before it reaches the filesystem.
func TestValidateSingleSegment(t *testing.T) {
	valid := []string{
		"test-set-0",  // the real-world shape
		"v1..v2",      // ".." as a substring, not as a path element
		"..hidden",    // leading dots are fine, the element is not ".."
		"a.b.c",       //
		"UPPER_case1", //
	}
	for _, name := range valid {
		if err := ValidateSingleSegment(name, false); err != nil {
			t.Errorf("ValidateSingleSegment(%q, false): want nil, got %v", name, err)
		}
		if err := ValidateSingleSegment(name, true); err != nil {
			t.Errorf("ValidateSingleSegment(%q, true): want nil, got %v", name, err)
		}
	}

	invalid := []string{
		".", "..", "../x", "a/b", "/abs", `a\b`, `..\x`, `\\server\share`,
		`\\?\C:`, "C:", "C:/x", "D:foo", "./a", "a/", "x/..",
	}
	for _, name := range invalid {
		err := ValidateSingleSegment(name, true)
		if err == nil {
			t.Errorf("ValidateSingleSegment(%q, true): want rejection, got nil", name)
			continue
		}
		// The rejection must name the offending value — a silent rewrite is
		// exactly what these call sites must not do. Errors quote it with
		// %q, so compare against the quoted form.
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
			t.Errorf("ValidateSingleSegment(%q): error must name the value, got %v", name, err)
		}
	}

	// The empty string is the one case the two call sites disagree on, so it
	// is a parameter rather than a hard-coded rule.
	if err := ValidateSingleSegment("", true); err != nil {
		t.Errorf(`ValidateSingleSegment("", true): want nil, got %v`, err)
	}
	if err := ValidateSingleSegment("", false); err == nil {
		t.Error(`ValidateSingleSegment("", false): want rejection, got nil`)
	}
}
