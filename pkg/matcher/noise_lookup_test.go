package matcher

import (
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

// Names reports whether an entry names a field, whatever the field's value:
// every field Covers excuses, and every field an entry with patterns names
// that a value of it does not match. It never names what no entry does, nor
// part of a field's name. Signature is the fields named: one for lookups
// whose entries name the same fields, whatever their patterns, another for
// any other.
func TestNoiseLookupNamesWhatItMayExcuse(t *testing.T) {
	l := NewNoiseLookup(map[string][]string{"ts": {}, "user.ID": {`^u-\d+$`}})
	for _, tc := range []struct {
		path  string
		value string
		named bool
	}{
		{"ts", "1", true},
		{"items[].ts", "1", true},
		{"user.id", "u-1", true},
		{"user.id", "admin", true},
		{"paid", "1", false},
		{"superuser.id", "u-1", false},
		{"id", "u-1", false},
	} {
		if got := l.Names(tc.path); got != tc.named {
			t.Errorf("Names(%q) = %v, want %v", tc.path, got, tc.named)
		}
		if l.Covers(tc.path, tc.value, true) && !l.Names(tc.path) {
			t.Errorf("Covers excuses %q (%q), which Names does not name", tc.path, tc.value)
		}
	}
	same := NewNoiseLookup(map[string][]string{"user.id": {}, "TS": {`^\d+$`}})
	if l.Signature() != same.Signature() {
		t.Errorf("lookups naming one set of fields have signatures %q and %q", l.Signature(), same.Signature())
	}
	for _, other := range []map[string][]string{{"ts": {}}, {"ts": {}, "user.name": {}}, {"ts": {}, "user.id": {}, "x": {}}, nil} {
		if NewNoiseLookup(other).Signature() == l.Signature() {
			t.Errorf("%v has the signature of %v", other, l)
		}
	}
}
