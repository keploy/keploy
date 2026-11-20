package matcher

import (
	"slices"
	"strings"
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

// Entry names the entry Covers reads for a leaf by the run of the leaf's
// path it matches, in the path's case, and gives its patterns: the entry a
// hint tells the user to edit, which may name the leaf by its last field or
// its array. keploy.yml's keys arrive lowercased (viper lowercases map
// keys), so the key the entry is held by would name "requestid" for an API
// field "requestId", a key keploy.yml does not spell so.
func TestNoiseLookupEntry(t *testing.T) {
	l := NewNoiseLookup(map[string][]string{"qty": {`^\d{3}$`}, "ids": {`^\d$`}, "order.qty": {`^1$`}, "ts": {}, "requestid": {`^r-`}, "user.authcode": {`^a`}, "Mixed": {`^m`}})
	for _, tc := range []struct {
		path, key string
		pats      []string
		ok        bool
	}{
		{"item.qty", "qty", []string{`^\d{3}$`}, true}, // by its last field
		{"item.Qty", "Qty", []string{`^\d{3}$`}, true}, // in the path's case
		{"ids[]", "ids", []string{`^\d$`}, true},       // by its array
		{"ids[].x", "ids", []string{`^\d$`}, true},     // what lies under it
		{"order.qty", "order.qty", []string{`^1$`}, true},
		{"ts", "ts", []string{}, true},
		{"name", "", []string{}, false},
		{"requestId", "requestId", []string{`^r-`}, true},
		{"meta.requestId", "requestId", []string{`^r-`}, true},
		{"items[].requestId", "requestId", []string{`^r-`}, true},
		{"requestIdx", "", []string{}, false},
		{"xrequestId", "", []string{}, false},
		{"User.authCode", "User.authCode", []string{`^a`}, true}, // a run of two fields
		{"a.User.authCode.b", "User.authCode", []string{`^a`}, true},
		{"mixed", "mixed", []string{`^m`}, true}, // the configuration's case is not the path's
		// The first run the entry names, as Covers finds it.
		{"requestID.requestId", "requestID", []string{`^r-`}, true},
		{"prequestId.requestId", "requestId", []string{`^r-`}, true},
		{"xRequestId.requestId", "requestId", []string{`^r-`}, true}, // never from inside a name
		{"RequestIdx.requestId", "requestId", []string{`^r-`}, true}, // nor to inside one
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

// An entry naming id keys names its field under every id key, as whole fields,
// in the request dialect too.
func TestNoiseLookupIDKeys(t *testing.T) {
	u := testUUID(4)
	l := NewNoiseLookup(map[string][]string{"options.{id}.traceid": {}})
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"options." + u + ".traceId", true},
		{"req.options." + u + ".traceId", true},
		{"options." + u + "[].traceId", false}, // an array under the id is another path
		{"options.summary.traceId", false},
		{"xoptions." + u + ".traceId", false}, // whole fields only
		{"options." + u + ".traceIdx", false},
	} {
		if got := l.Covers(tc.path, "v", true); got != tc.want {
			t.Errorf("Covers(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// A hint names an entry naming id keys as it is written, by the run of the
// path with its ids collapsed and in the path's case: never by an id, which
// is a value, nor by the whole path.
func TestNoiseLookupEntryNamesIDKeys(t *testing.T) {
	u := testUUID(4)
	l := NewNoiseLookup(map[string][]string{"options.{id}.traceid": {`^t-`}, "{id}": {`^x`}, "sessions." + u + ".token": {`^s`}})
	for _, tc := range []struct{ path, key string }{
		{"options." + u + ".traceId", "options.{id}.traceId"},
		{"req.Options." + strings.ToUpper(u) + ".traceId", "Options.{id}.traceId"},
		{"options." + u + ".traceId[]", "options.{id}.traceId"},
		{"tags." + u, "{id}"},
		{"tags." + u + "[]", "{id}"},
		// An entry that names an id as a literal is named as it is written.
		{"sessions." + u + ".token", "sessions." + u + ".token"},
	} {
		key, pats, ok := l.Entry(tc.path)
		if !ok || key != tc.key || len(pats) != 1 {
			t.Errorf("Entry(%q) = %q, %q, %v; want %q and its one pattern", tc.path, key, pats, ok, tc.key)
		}
	}
}
