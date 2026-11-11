package learn

import "go.keploy.io/server/v3/pkg/models"

// DeclaredSources is Input.Source of sources, the sources keploy.yml's
// rules declare for one recording's fields (models.MockEntry.Sources, as
// keploy mock replay's scope table hands the agent), by the place placeOf
// names a drifted field's path at (the adapter's PlaceOf of its request
// leaf). nil where sources declare none, where placeOf is nil, and where
// they are no sources this keploy can decide fields by
// (models.CheckSources; the agent refuses a table that holds such): the
// learn step then reads no source, as today. A path placeOf cannot name,
// or names a place no source does, has none.
func DeclaredSources(sources []models.DeclaredSource, placeOf func(path string) (string, bool)) func(path string) (Source, bool) {
	if len(sources) == 0 || placeOf == nil || models.CheckSources(sources) != nil {
		return nil
	}
	by := make(map[string]Source, len(sources))
	for _, s := range sources {
		src := Source{Kind: SourceMinted, Rule: s.Rule}
		if s.Kind == models.DeclaredSourceClock {
			src = Source{Kind: SourceClock, Rule: s.Rule, Clock: *s.Clock}
		}
		by[s.Field] = src
	}
	return func(path string) (Source, bool) {
		place, ok := placeOf(path)
		if !ok {
			return Source{}, false
		}
		src, ok := by[place]
		return src, ok
	}
}
