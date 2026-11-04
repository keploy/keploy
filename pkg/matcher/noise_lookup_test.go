package matcher

import (
	"slices"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
)

func TestNoiseLookup(t *testing.T) {
	l := NewNoiseLookup(map[string][]string{"ts": {}, "items[].id": {}, "code": {`^A-\d+$`}})
	for _, tc := range []struct {
		path, value string
		scalar      bool
		want        bool
	}{
		{"ts", "1", true, true},
		{"user.ts", "1", true, true}, // a single-segment entry is a field name at any depth
		{"items[].id", "x", true, true},
		{"code", "A-12", true, true},
		{"code", "B-12", true, false}, // a pattern excuses only what it matches
		{"code", "", false, false},    // and never a container
		{"name", "x", true, false},
	} {
		if got := l.Covers(tc.path, tc.value, tc.scalar); got != tc.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", tc.path, tc.value, got, tc.want)
		}
	}
}

// Entry names the entry Covers reads for a leaf, as the configuration
// writes its key, and its patterns: the entry a hint tells the user to
// edit, which may name the leaf by its last field or its array.
func TestNoiseLookupEntry(t *testing.T) {
	l := NewNoiseLookup(map[string][]string{"Qty": {`^\d{3}$`}, "ids": {`^\d$`}, "order.qty": {`^1$`}, "ts": {}})
	for _, tc := range []struct {
		path, key string
		pats      []string
		ok        bool
	}{
		{"item.qty", "Qty", []string{`^\d{3}$`}, true}, // by its last field, in any case
		{"ids[]", "ids", []string{`^\d$`}, true},       // by its array
		{"order.qty", "order.qty", []string{`^1$`}, true},
		{"ts", "ts", []string{}, true},
		{"name", "", []string{}, false},
	} {
		key, pats, ok := l.Entry(tc.path)
		if key != tc.key || !slices.Equal(pats, tc.pats) || ok != tc.ok {
			t.Errorf("Entry(%q) = %q, %q, %v; want %q, %q, %v", tc.path, key, pats, ok, tc.key, tc.pats, tc.ok)
		}
		if pats, ok := l.Patterns(tc.path); !slices.Equal(pats, tc.pats) || ok != tc.ok {
			t.Errorf("Patterns(%q) = %q, %v; want %q, %v", tc.path, pats, ok, tc.pats, tc.ok)
		}
	}
}

// Under the proven switch a learned (type-strict) entry names exactly the
// field that drifted and its subtree; without it, it is a substring as every
// entry is.
func TestLearnedEntryIsExactUnderTheSwitch(t *testing.T) {
	idx := buildNoiseIndex(map[string][]string{"items[].id": {}}, nil)
	idx.markTypeStrict(map[string]struct{}{"items[].id": {}})
	for _, on := range []bool{false, true} {
		if on {
			t.Setenv(provenswitch.Env, "1")
		} else {
			t.Setenv(provenswitch.Env, "")
		}
		if _, noisy, _ := idx.matchKind("items[].id"); !noisy {
			t.Fatalf("switch %v: the field itself is excused", on)
		}
		if _, noisy, _ := idx.matchKind("items[].id.sub"); !noisy {
			t.Fatalf("switch %v: its subtree is excused", on)
		}
		if _, noisy, _ := idx.matchKind("items[].identifier"); noisy == on {
			t.Fatalf("switch %v: a sibling whose name it prefixes is excused: %v", on, noisy)
		}
	}
}
