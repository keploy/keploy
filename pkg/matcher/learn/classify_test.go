package learn

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// callOf reads a recorded path beginning with "//" as a path, as the matcher
// does: no host, and its segments counted from the empty one after the
// leading slash.
func TestCallOfAPathBeginningWithTwoSlashes(t *testing.T) {
	m := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "//fees/1"}}}
	if got, want := callOf(m), "Http GET  /3"; got != want {
		t.Fatalf("callOf = %q; want %q", got, want)
	}
}
