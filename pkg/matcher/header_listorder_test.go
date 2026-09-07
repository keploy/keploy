package matcher

import (
	"net/http"
	"testing"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// The exact shape reported in keploy/keploy#4349: a server that builds Allow
// from an unordered collection emits the same set of methods in a different
// order on the replay, and the test failed on it.
func TestCompareHeaders_UnorderedListHeaderReorder(t *testing.T) {
	cases := []struct {
		name     string
		exp, act map[string]string
		want     bool
	}{
		{"issue 4349: Allow reordered", map[string]string{"Allow": "POST, OPTIONS"}, map[string]string{"Allow": "OPTIONS, POST"}, true},
		{"Allow identical", map[string]string{"Allow": "POST, OPTIONS"}, map[string]string{"Allow": "POST, OPTIONS"}, true},
		{"Allow gained a method", map[string]string{"Allow": "POST, OPTIONS"}, map[string]string{"Allow": "OPTIONS, POST, GET"}, false},
		{"Allow lost a method", map[string]string{"Allow": "POST, OPTIONS, GET"}, map[string]string{"Allow": "OPTIONS, POST"}, false},
		{"Allow swapped a method", map[string]string{"Allow": "POST, OPTIONS"}, map[string]string{"Allow": "PUT, OPTIONS"}, false},
		{"Vary reordered", map[string]string{"Vary": "Accept-Encoding, Origin"}, map[string]string{"Vary": "Origin, Accept-Encoding"}, true},
		{"Cache-Control reordered", map[string]string{"Cache-Control": "no-cache, max-age=0"}, map[string]string{"Cache-Control": "max-age=0, no-cache"}, true},
		{"CORS methods reordered", map[string]string{"Access-Control-Allow-Methods": "GET, POST"}, map[string]string{"Access-Control-Allow-Methods": "POST, GET"}, true},
		{"lowercase header name still matched", map[string]string{"allow": "POST, OPTIONS"}, map[string]string{"allow": "OPTIONS, POST"}, true},
		{"whitespace-only encoding change", map[string]string{"Allow": "POST,OPTIONS"}, map[string]string{"Allow": "POST, OPTIONS"}, true},

		// Order IS semantic for these: reordering must stay a failure.
		{"Content-Encoding reordered stays a failure", map[string]string{"Content-Encoding": "gzip, br"}, map[string]string{"Content-Encoding": "br, gzip"}, false},
		{"Via reordered stays a failure", map[string]string{"Via": "1.1 a, 1.1 b"}, map[string]string{"Via": "1.1 b, 1.1 a"}, false},
		{"Set-Cookie reordered stays a failure", map[string]string{"Set-Cookie": "a=1, b=2"}, map[string]string{"Set-Cookie": "b=2, a=1"}, false},
		{"unlisted header reordered stays a failure", map[string]string{"X-Thing": "a, b"}, map[string]string{"X-Thing": "b, a"}, false},

		// Unrelated regressions guarded.
		{"plain header value change stays a failure", map[string]string{"Content-Type": "application/json"}, map[string]string{"Content-Type": "text/plain"}, false},
		{"missing header stays a failure", map[string]string{"Allow": "POST"}, map[string]string{}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := &[]models.HeaderResult{}
			got := CompareHeaders(pkg.ToHTTPHeader(c.exp), pkg.ToHTTPHeader(c.act), res, map[string][]string{})
			if got != c.want {
				t.Errorf("CompareHeaders() = %v, want %v (results: %+v)", got, c.want, *res)
			}
		})
	}
}

// A comma-folded header and repeated same-name headers are the same header
// (RFC 9110 §5.3), so the fix must see through the encoding difference too.
func TestCompareHeaders_FoldedVsRepeated(t *testing.T) {
	exp := http.Header{"Allow": []string{"POST, OPTIONS"}}
	act := http.Header{"Allow": []string{"OPTIONS", "POST"}}
	res := &[]models.HeaderResult{}
	if !CompareHeaders(exp, act, res, map[string][]string{}) {
		t.Errorf("folded and repeated encodings of the same list should match, got mismatch: %+v", *res)
	}

	// but not when the elements actually differ
	act2 := http.Header{"Allow": []string{"OPTIONS", "GET"}}
	res2 := &[]models.HeaderResult{}
	if CompareHeaders(exp, act2, res2, map[string][]string{}) {
		t.Error("differing elements must not match")
	}
}

func TestSplitHTTPListValues(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"POST, OPTIONS"}, []string{"POST", "OPTIONS"}},
		{[]string{"POST,OPTIONS"}, []string{"POST", "OPTIONS"}},
		{[]string{"gzip,,deflate"}, []string{"gzip", "deflate"}},
		{[]string{"POST", "OPTIONS"}, []string{"POST", "OPTIONS"}},
		{[]string{`no-cache="X-Foo, X-Bar", max-age=0`}, []string{`no-cache="X-Foo, X-Bar"`, "max-age=0"}},
		{[]string{""}, nil},
	}
	for _, c := range cases {
		got := splitHTTPListValues(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("splitHTTPListValues(%q) = %q, want %q", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("splitHTTPListValues(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	}
}
