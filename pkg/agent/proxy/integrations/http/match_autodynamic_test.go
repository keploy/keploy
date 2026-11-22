package http

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

// Query values get the same zero-config dynamic-id fallback that path segments
// already had. Before this, a uuid moved from the path into a query param went
// from "matches with no config" to "502, configure url noise" — see
// TestQueryParamsMatch_AutoDynamicParityWithPath.
func TestQueryParamsMatch_AutoDynamic(t *testing.T) {
	h := newHTTP()

	cases := []struct {
		name        string
		mock        map[string]string
		live        url.Values
		autoDynamic bool
		want        bool
	}{
		// Pass 1 (autoDynamic=false) is never relaxed: a differing value fails
		// no matter how dynamic it looks.
		{"uuid, pass 1 (strict)", map[string]string{"id": "3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
			url.Values{"id": {"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}}, false, false},
		{"uuid, pass 2 (fallback)", map[string]string{"id": "3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
			url.Values{"id": {"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}}, true, true},

		{"epoch millis, pass 2", map[string]string{"ts": "1712345678901"},
			url.Values{"ts": {"1712345680000"}}, true, true},
		{"long hex hash, pass 2", map[string]string{"h": "9f86d081884c7d65"},
			url.Values{"h": {"a1b2c3d4e5f60718"}}, true, true},
		{"long mixed token, pass 2", map[string]string{"t": "amit1781794443438"},
			url.Values{"t": {"amit1781794999999"}}, true, true},

		// The reason looksDynamicQueryValue is stricter than
		// looksDynamicSegment: a bare small integer in a query is a page /
		// limit / offset, not an id. Relaxing it would re-open the exact bug
		// this gate closes.
		{"page number NOT relaxed", map[string]string{"page": "2"},
			url.Values{"page": {"3"}}, true, false},
		{"limit NOT relaxed", map[string]string{"limit": "50"},
			url.Values{"limit": {"100"}}, true, false},
		{"short id NOT relaxed", map[string]string{"id": "7"},
			url.Values{"id": {"8"}}, true, false},

		// Word-like values stay strict in both passes (same as path segments).
		{"word value NOT relaxed", map[string]string{"status": "active"},
			url.Values{"status": {"archived"}}, true, false},
		{"short slug NOT relaxed", map[string]string{"env": "prod"},
			url.Values{"env": {"stage"}}, true, false},

		// A dynamic-looking value on only ONE side is still a real difference.
		{"dynamic on one side only", map[string]string{"id": "3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
			url.Values{"id": {"active"}}, true, false},

		// Key set is still enforced under autoDynamic.
		{"extra key still fails", map[string]string{"id": "3f2504e0-4f89-11d3-9a0c-0305e82c3301"},
			url.Values{"id": {"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}, "page": {"2"}}, true, false},

		// Equal values are unaffected by the flag.
		{"equal values, pass 2", map[string]string{"id": "A"}, url.Values{"id": {"A"}}, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want,
				h.QueryParamsMatch(tc.mock, tc.live, nil, tc.autoDynamic))
		})
	}
}

// The same identifier must not change matchability just because it lives in the
// query instead of the path. This is the asymmetry the autoDynamic argument fixes.
func TestQueryParamsMatch_AutoDynamicParityWithPath(t *testing.T) {
	h := newHTTP()
	const recUUID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	const liveUUID = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

	inPath := h.MatchURLPath("http://svc/items/"+recUUID, "/items/"+liveUUID, nil, true)
	inQuery := h.QueryParamsMatch(map[string]string{"id": recUUID}, url.Values{"id": {liveUUID}}, nil, true)
	require.True(t, inPath, "path uuid should match on the auto-dynamic pass")
	require.Equal(t, inPath, inQuery, "a uuid in the query should behave like a uuid in the path")
}

// url noise still wins over (and is independent of) the dynamic fallback: it
// applies on BOTH passes, including for values the heuristic refuses to relax.
func TestQueryParamsMatch_NoiseIndependentOfAutoDynamic(t *testing.T) {
	h := newHTTP()
	for _, autoDynamic := range []bool{false, true} {
		require.True(t, h.QueryParamsMatch(
			map[string]string{"page": "2"}, url.Values{"page": {"3"}}, []string{`^[0-9]+$`}, autoDynamic),
			"url noise should cover the value regardless of autoDynamic=%v", autoDynamic)
	}
}
