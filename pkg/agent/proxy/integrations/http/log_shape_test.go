package http

import "testing"

func TestPathDiffersAtNamesTheSegmentNotItsValue(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"/u", "/v", 0},
		{"/orders/1/items", "/orders/2/items", 1},
		{"/orders", "/orders/2", 1},
		{"/orders/2", "/orders/2", -1},
	} {
		if got := pathDiffersAt(tc.a, tc.b); got != tc.want {
			t.Errorf("pathDiffersAt(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
