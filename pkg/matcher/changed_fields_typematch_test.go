package matcher

import (
	"sort"
	"testing"
)

// TestChangedJSONFieldPaths_TypeStrictLearnedField pins the learned-dynamic +
// type-match contract (PR-B / §P0b, founder directive): a field learned as dynamic
// (in typeStrict, and also carried in the known-noise set as it is in production)
// ignores VALUE drift but a TYPE change must STILL be reported. Non-learned fields
// stay strict; a user full-ignore (empty regex, not type-strict) drops value AND
// type.
func TestChangedJSONFieldPaths_TypeStrictLearnedField(t *testing.T) {
	// Learned paths ride in `known` (KnownNoise merges learned+user) and are marked.
	known := map[string][]string{"id": {}}
	typeStrict := map[string]struct{}{"id": {}}

	// Value drift, same type (number->number), is tolerated.
	if got := ChangedJSONFieldPaths(`{"id":1}`, `{"id":2}`, known, typeStrict, false, nil); len(got) != 0 {
		t.Fatalf("value drift on a type-strict field must be tolerated; got %v", got)
	}

	// TYPE drift (number->string) must still be reported.
	got := ChangedJSONFieldPaths(`{"id":1}`, `{"id":"1"}`, known, typeStrict, false, nil)
	sort.Strings(got)
	if len(got) != 1 || got[0] != "id" {
		t.Fatalf("a type-strict field must flag a TYPE change; got %v, want [id]", got)
	}

	// A sibling non-learned field stays fully strict: a value change is reported.
	got = ChangedJSONFieldPaths(`{"id":1,"name":"a"}`, `{"id":2,"name":"b"}`, known, typeStrict, false, nil)
	sort.Strings(got)
	if len(got) != 1 || got[0] != "name" {
		t.Fatalf("a non-learned field must stay strict; got %v, want [name]", got)
	}

	// A user full-ignore (empty regex in known, NOT type-strict) drops value AND type.
	userIgnore := map[string][]string{"name": {}}
	if got := ChangedJSONFieldPaths(`{"id":1,"name":"a"}`, `{"id":1,"name":123}`, userIgnore, nil, false, nil); len(got) != 0 {
		t.Fatalf("a user full-ignore field must drop value AND type; got %v", got)
	}
}

// TestChangedJSONFieldPaths_TypeStrictRespectsOrdering is the regression guard for
// the substring-collision blocker: a learned type-strict key ("id") must NOT
// override a MORE-SPECIFIC user value-regex entry ("userid"). A userid value that
// violates the user's regex must still be reported — not silently tolerated just
// because "id" is a substring of "userid".
func TestChangedJSONFieldPaths_TypeStrictRespectsOrdering(t *testing.T) {
	known := map[string][]string{
		"id":     {},           // learned, type-strict
		"userid": {`^[0-9]+$`}, // user value-regex: only all-digit userids are noise
	}
	typeStrict := map[string]struct{}{"id": {}}

	// userid drifts between two NON-numeric values -> the user's regex does not
	// cover it -> it is a real difference and must be reported.
	got := ChangedJSONFieldPaths(`{"userid":"abc"}`, `{"userid":"xyz"}`, known, typeStrict, false, nil)
	if len(got) != 1 || got[0] != "userid" {
		t.Fatalf("a more-specific user regex must win over a learned type-strict substring; got %v, want [userid]", got)
	}
}

// TestChangedJSONFieldPaths_ValueChangesOnly pins the learn-pass helper: with
// valueChangesOnly=true only value drift (same type) is returned, so the
// auto-noising learn pass never learns a type change or a removed field as
// ignorable noise. (Wired for the learn-value-only step; enforcement passes false.)
func TestChangedJSONFieldPaths_ValueChangesOnly(t *testing.T) {
	exp := `{"v":1,"t":1,"r":"x"}`
	act := `{"v":2,"t":"1"}` // v: value change; t: number->string type change; r: removed

	all := ChangedJSONFieldPaths(exp, act, nil, nil, false, nil)
	sort.Strings(all)
	if len(all) != 3 {
		t.Fatalf("default must report value+type+removed; got %v", all)
	}
	vco := ChangedJSONFieldPaths(exp, act, nil, nil, true, nil)
	if len(vco) != 1 || vco[0] != "v" {
		t.Fatalf("valueChangesOnly must return only the value change [v]; got %v", vco)
	}
}
