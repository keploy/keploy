package http

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// F1 fix: repeated query params compare order-INDEPENDENTLY, while a genuinely
// different value still fails and single-value behavior is unchanged.
func TestQueryParamsMatch_MultiValueOrderIndependent(t *testing.T) {
	h := newHTTP()
	q := func(raw string) url.Values { v, _ := url.ParseQuery(raw); return v }

	cases := []struct {
		name  string
		mock  map[string]string
		live  url.Values
		noise []string
		want  bool
	}{
		{"repeated same order", map[string]string{"tag": "red, blue"}, q("tag=red&tag=blue"), nil, true},
		{"repeated reversed order (F1)", map[string]string{"tag": "red, blue"}, q("tag=blue&tag=red"), nil, true},
		{"repeated genuinely different value", map[string]string{"tag": "red, blue"}, q("tag=red&tag=green"), nil, false},
		{"repeated different count", map[string]string{"tag": "red, blue"}, q("tag=red&tag=blue&tag=green"), nil, false},
		{"single value still matches", map[string]string{"id": "A"}, q("id=A"), nil, true},
		{"single value still fails on diff", map[string]string{"id": "A"}, q("id=B"), nil, false},
		{"repeated noise-covered member", map[string]string{"t": "1, 2"}, q("t=9&t=8"), []string{`[0-9]+`}, true},
		{"repeated reorder with three", map[string]string{"k": "a, b, c"}, q("k=c&k=a&k=b"), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, h.QueryParamsMatch(tc.mock, tc.live, tc.noise))
		})
	}
}
