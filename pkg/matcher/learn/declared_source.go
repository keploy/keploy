package learn

import (
	"cmp"

	"go.keploy.io/server/v3/pkg/models"
)

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
		// CheckSources held: a clock source has its range, a minted one
		// its form.
		if s.Kind == models.DeclaredSourceClock {
			by[s.Field] = Source{Kind: SourceClock, Rule: s.Rule, Clock: *cmp.Or(s.Clock, &models.ClockOffset{})}
		} else {
			by[s.Field] = Source{Kind: SourceMinted, Rule: s.Rule, Minted: *cmp.Or(s.Minted, &models.MintedForm{})}
		}
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
