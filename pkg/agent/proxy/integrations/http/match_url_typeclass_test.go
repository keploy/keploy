package http

import "testing"

// TestURLSegmentTypeClass pins the classifier behind type-aware dynamic path
// matching, and confirms looksDynamicSegment stays byte-for-byte equivalent to
// "class != \"\"".
func TestURLSegmentTypeClass(t *testing.T) {
	cases := []struct {
		in    string
		class string
	}{
		{"123", "digits"},
		{"0", "digits"},
		{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", "uuid"},
		{"9f86d081884c7d65", "hex"},         // 16 hex chars
		{"amit1781794443438x", "token"},     // >=16, mixes letters and digits, not all-hex
		{"507f1f77bcf86cd799439011", "hex"}, // mongo ObjectId (24 hex)
		{"users", ""},                       // plain word
		{"v1alpha1", ""},                    // short composite
		{"prod", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := urlSegmentTypeClass(c.in); got != c.class {
			t.Errorf("urlSegmentTypeClass(%q) = %q, want %q", c.in, got, c.class)
		}
		if gotBool, wantBool := looksDynamicSegment(c.in), c.class != ""; gotBool != wantBool {
			t.Errorf("looksDynamicSegment(%q) = %v, inconsistent with class %q", c.in, gotBool, c.class)
		}
	}
}

// TestMatchURLPath_AutoDynamicTypeAware pins the type-aware tightening of the
// zero-config dynamic-segment fallback: a changed id matches only when BOTH sides
// are the SAME dynamic type-class. Same-class drift still matches (the zero-config
// behavior kept), but a type change (digits -> uuid) no longer collapses onto the
// mock — it is a different resource.
func TestMatchURLPath_AutoDynamicTypeAware(t *testing.T) {
	h := newHTTP()
	const uuid = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

	// same class still matches (unchanged zero-config behavior)
	if !h.MatchURLPath("http://svc/users/123", "/users/999", nil, true) {
		t.Fatalf("digits->digits id drift must still match")
	}
	if !h.MatchURLPath("http://svc/items/"+uuid, "/items/6ba7b810-9dad-11d1-80b4-00c04fd430c8", nil, true) {
		t.Fatalf("uuid->uuid id drift must still match")
	}

	// cross type-class must NOT match (different type = different resource)
	if h.MatchURLPath("http://svc/users/123", "/users/"+uuid, nil, true) {
		t.Fatalf("digits->uuid must NOT match (a type change is a different resource)")
	}
	if h.MatchURLPath("http://svc/items/"+uuid, "/items/123", nil, true) {
		t.Fatalf("uuid->digits must NOT match")
	}
}
