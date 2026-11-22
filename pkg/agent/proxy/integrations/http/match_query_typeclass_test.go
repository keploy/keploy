package http

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQueryValueTypeClass pins the query-value classifier behind type-aware
// auto-dynamic query matching, and confirms looksDynamicQueryValue stays
// byte-for-byte equivalent to "class != \"\"". The key query-specific rule is
// that a SHORT bare integer (page / limit / offset) is never dynamic.
func TestQueryValueTypeClass(t *testing.T) {
	cases := []struct {
		in    string
		class string
	}{
		{"2", ""},                   // page number
		{"7", ""},                   // short id
		{"50", ""},                  // limit
		{"123456789", ""},           // 9 digits, below the long-run threshold
		{"1234567890", "digits"},    // 10 digits = long run (epoch/snowflake)
		{"1712345678901", "digits"}, // epoch millis
		{"3f2504e0-4f89-11d3-9a0c-0305e82c3301", "uuid"},
		{"9f86d081884c7d65", "hex"},         // 16 hex chars
		{"amit1781794443438", "token"},      // >=16 mixed letters+digits, not all-hex
		{"507f1f77bcf86cd799439011", "hex"}, // mongo ObjectId (24 hex)
		{"active", ""},                      // word value
		{"prod", ""},                        // short slug
		{"", ""},
	}
	for _, c := range cases {
		if got := queryValueTypeClass(c.in); got != c.class {
			t.Errorf("queryValueTypeClass(%q) = %q, want %q", c.in, got, c.class)
		}
		if gotBool, wantBool := looksDynamicQueryValue(c.in), c.class != ""; gotBool != wantBool {
			t.Errorf("looksDynamicQueryValue(%q) = %v, inconsistent with class %q", c.in, gotBool, c.class)
		}
	}
}

// TestQueryParamsMatch_AutoDynamicTypeAware pins the type-aware tightening of the
// zero-config dynamic-query fallback: a differing value is relaxed only when both
// sides share the SAME dynamic type-class. Same-class drift still matches (the
// zero-config behavior kept), but a type change (uuid -> number, number -> hash,
// ...) no longer collapses distinct queries onto one mock. The cross-type cases
// below return true on the old "both look dynamic (any shape)" code, so this test
// is non-vacuous.
func TestQueryParamsMatch_AutoDynamicTypeAware(t *testing.T) {
	h := newHTTP()
	const uuid = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	const uuid2 = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

	cases := []struct {
		name string
		mock map[string]string
		live url.Values
		want bool
	}{
		// same type-class still matches (unchanged zero-config behavior)
		{"uuid -> uuid", map[string]string{"id": uuid}, url.Values{"id": {uuid2}}, true},
		{"long digits -> long digits", map[string]string{"ts": "1712345678901"},
			url.Values{"ts": {"1712345680000"}}, true},
		{"hex -> hex", map[string]string{"h": "9f86d081884c7d65"},
			url.Values{"h": {"a1b2c3d4e5f60718"}}, true},

		// cross type-class must NOT match (different value space = different resource)
		{"uuid -> long digits", map[string]string{"id": uuid},
			url.Values{"id": {"1712345678901"}}, false},
		{"long digits -> uuid", map[string]string{"id": "1712345678901"},
			url.Values{"id": {uuid}}, false},
		{"long digits -> hex", map[string]string{"id": "1712345678901"},
			url.Values{"id": {"9f86d081884c7d65"}}, false},
		{"hex -> token", map[string]string{"id": "9f86d081884c7d65"},
			url.Values{"id": {"amit1781794443438"}}, false},
		{"uuid -> hex", map[string]string{"id": uuid},
			url.Values{"id": {"9f86d081884c7d65"}}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, h.QueryParamsMatch(tc.mock, tc.live, nil, true))
		})
	}
}
