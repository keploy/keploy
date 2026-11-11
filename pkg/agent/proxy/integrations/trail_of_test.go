package integrations

import (
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
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

// trailBgStore is a MockMemDb that hands background recordings and tells
// which of them trail a test.
type trailBgStore struct {
	MockMemDb
	recs   []models.BackgroundRecording
	trails map[string]string
}

func (s trailBgStore) BackgroundRecordings(kind models.Kind) []models.BackgroundRecording {
	var out []models.BackgroundRecording
	for _, r := range s.recs {
		if r.Mock == nil || r.Mock.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func (s trailBgStore) TrailOf(mock string) learn.Trail {
	if t := s.trails[mock]; t != "" {
		return learn.Trail{Test: t, Mocks: []string{"mock-own"}}
	}
	return learn.Trail{}
}

// TrailRecordingsOf is, of the store's background recordings of a kind,
// those that trail a test, with that test, in the store's order: none of
// another kind, none that trails no test, no nil recording, and none from a
// store that hands no background recordings or tells no trail.
func TestTrailRecordingsOfAreTheBackgroundRecordingsThatTrailATest(t *testing.T) {
	rec := func(name string, kind models.Kind) models.BackgroundRecording {
		return models.BackgroundRecording{Mock: &models.Mock{Name: name, Kind: kind}}
	}
	s := trailBgStore{
		recs:   []models.BackgroundRecording{rec("mock-3", models.Mongo), {}, rec("mock-1", models.Mongo), rec("mock-2", models.Mongo), rec("mock-4", models.HTTP)},
		trails: map[string]string{"mock-3": "o::TestB", "mock-1": "o::TestA", "mock-4": "o::TestA"},
	}
	got := TrailRecordingsOf(s, models.Mongo)
	var names []string
	for _, r := range got {
		names = append(names, r.Mock.Name+"->"+r.Trails)
	}
	if want := []string{"mock-3->o::TestB", "mock-1->o::TestA"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("TrailRecordingsOf = %v; want %v", names, want)
	}
	if got := TrailRecordingsOf(trailStore{trail: learn.Trail{Test: "o::TestA"}}, models.Mongo); got != nil {
		t.Fatalf("a store with no background recordings trails %+v", got)
	}
	if got := TrailRecordingsOf(bgOnly{s: s}, models.Mongo); got != nil {
		t.Fatalf("a store that tells no trail trails %+v", got)
	}
}

// bgOnly hands its store's background recordings and tells no trail.
type bgOnly struct {
	MockMemDb
	s trailBgStore
}

func (b bgOnly) BackgroundRecordings(kind models.Kind) []models.BackgroundRecording {
	return b.s.BackgroundRecordings(kind)
}
