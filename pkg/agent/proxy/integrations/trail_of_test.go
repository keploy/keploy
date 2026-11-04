package integrations

import (
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// trailStore is a MockMemDb that tells the test a recording trails.
type trailStore struct {
	MockMemDb
	trail learn.Trail
}

func (s trailStore) TrailOf(string) learn.Trail { return s.trail }

// TrailOf reads the store's trail where it can tell one, and none where it
// cannot.
func TestTrailOfReadsTheStoresTrail(t *testing.T) {
	want := learn.Trail{Test: "o::TestA", Mocks: []string{"mock-1"}}
	if got := TrailOf(trailStore{trail: want}, "mock-9"); got.Test != want.Test || len(got.Mocks) != 1 {
		t.Fatalf("TrailOf = %+v; want %+v", got, want)
	}
	if got := TrailOf(nil, "mock-9"); got.Test != "" || got.Mocks != nil {
		t.Fatalf("a store that cannot tell trails %+v; want none", got)
	}
}
