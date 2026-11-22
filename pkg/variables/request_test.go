package variables

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// InRequest counts a recorded path's segments as the matcher does: a path
// beginning with "//" has an empty first segment, not a host.
func TestInRequestCountsAPathBeginningWithTwoSlashes(t *testing.T) {
	held := InRequest(&models.HTTPReq{URL: "//fees/42"})
	for i, want := range []bool{true, true, true, false} {
		if got := held(Place{Part: PartPath, Index: i}); got != want {
			t.Fatalf("segment %d held = %v; want %v (segments \"\", fees, 42)", i, got, want)
		}
	}
}
