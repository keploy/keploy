package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// DeclaredSources answers a drifted path with the source of the place the
// adapter names it at: a clock source with its rule and range, a minted
// one with its rule. A path the adapter cannot name, or names a place no
// source does, has none; sources that are none keploy can decide a field
// by, none at all, or no namer leave the learn step no source (nil).
func TestDeclaredSourcesAnswerAPathByItsPlace(t *testing.T) {
	off := models.ClockOffset{Min: -time.Second, Res: time.Second}
	sources := []models.DeclaredSource{
		{Rule: "at@0a1b2c3d", Kind: models.DeclaredSourceClock, Field: "body.docs.msgs.0.at", Clock: &off},
		{Rule: "id@44aa55bb", Kind: models.DeclaredSourceMinted, Field: "body.docs.msgs.0._id"},
	}
	placeOf := func(path string) (string, bool) {
		if path == "unnamed" {
			return "body.docs.msgs.0._id", false // a namer's place it cannot vouch for
		}
		return "body." + path, true
	}
	src := DeclaredSources(sources, placeOf)
	if src == nil {
		t.Fatal("no source func for two good sources")
	}
	if s, ok := src("docs.msgs.0.at"); !ok || s != (Source{Kind: SourceClock, Rule: "at@0a1b2c3d", Clock: off}) {
		t.Errorf("the clock place: %+v %v", s, ok)
	}
	if s, ok := src("docs.msgs.0._id"); !ok || s != (Source{Kind: SourceMinted, Rule: "id@44aa55bb"}) {
		t.Errorf("the minted place: %+v %v", s, ok)
	}
	for _, path := range []string{"unnamed", "docs.msgs.0.name", "docs.msgs.0._id.x"} {
		if s, ok := src(path); ok {
			t.Errorf("%q has the source %+v", path, s)
		}
	}
	bad := append([]models.DeclaredSource{{Rule: "x@00000000", Kind: "answer", Field: "body.x"}}, sources...)
	for name, f := range map[string]func(string) (Source, bool){
		"no sources":       DeclaredSources(nil, placeOf),
		"no namer":         DeclaredSources(sources, nil),
		"a source refused": DeclaredSources(bad, placeOf),
		"two on one field": DeclaredSources(append(sources, models.DeclaredSource{Rule: "id2@00000000", Kind: models.DeclaredSourceMinted, Field: "body.docs.msgs.0._id"}), placeOf),
	} {
		if f != nil {
			t.Errorf("%s: a source func", name)
		}
	}
}
