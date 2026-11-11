package learn

import (
	"cmp"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
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

// RangeNeverChecked reports whether a replay never checks o, a clock rule's
// range (models.DeclaredSource.Clock), at a recording no learning read: a
// range all on one side of zero (an offset from the call's moment, as a
// cut-off a few seconds before it) whose unit is a second or finer
// (models.ClockOffset.Unit). models.ClockOffset.Holds holds no such offset
// (an application that cuts its value to that unit makes an offset changed
// by one look the same), so the check would refuse every value. DeclaredClocks
// leaves such a field out: it compares as a field no rule names. A range
// around zero (a plain read of the clock) is checked, and so is one whose
// unit is a minute or coarser.
func RangeNeverChecked(o models.ClockOffset) bool {
	off, _ := declaredOffset(o) // a range it refuses is empty: Plain
	return !off.Plain() && off.Unit() <= time.Second
}

// DeclaredBodySources is DeclaredSources of sources for a recording's
// leaves read by their exact leaf path (Leaf.Path): a rule names a field of
// the body by its place, which a replay reads at its leaf path
// (DeclaredClocks: placeLeafPath), so the learn step reads it there too. A
// rule of another part, or a place keploy does not parse, names no leaf. nil
// where none of sources names a body leaf, or as DeclaredSources.
func DeclaredBodySources(sources []models.DeclaredSource) func(path string) (Source, bool) {
	fields := map[string]string{}
	for _, s := range sources {
		p, err := variables.Parse(s.Field)
		if err != nil || p.Part != variables.PartBody {
			continue
		}
		if leaf, ok := placeLeafPath(p); ok {
			fields[leaf] = s.Field
		}
	}
	if len(fields) == 0 {
		return nil
	}
	return DeclaredSources(sources, func(path string) (string, bool) {
		f, ok := fields[path]
		return f, ok
	})
}

// DeclaredClocks is learned, a recording's clocks as the learn step stored
// them (models.MockSpec.Clocks, by leaf path), with the check each clock
// rule of sources makes at its field: the rule's range as learning stores a
// clock read (declaredOffset), keyed by the field's leaf path (LeafPath). A
// clock rule names a read of the application's clock wherever its field
// stands, so a replay checks it as one in a recording no learning read: a
// worker: series a judge placed, whose call the app makes only inside a
// test, where the background answers a call exactly and nothing is learned
// onto it. What the learn step learned at a place stays: learning read the
// call there. Only a field in the body counts (an index keyed by a URL or a
// header reads no declared source; such a field compares as recorded, which
// refuses a changed value), and only a range a replay checks
// (RangeNeverChecked). learned itself where sources declare no clock
// this keploy checks (models.CheckSources refuses them all, or none is a
// clock in the body the learn step learned nothing at); otherwise a new map.
func DeclaredClocks(sources []models.DeclaredSource, learned map[string]models.ClockOffset) map[string]models.ClockOffset {
	if models.CheckSources(sources) != nil {
		return learned
	}
	var out map[string]models.ClockOffset
	for _, s := range sources {
		if s.Kind != models.DeclaredSourceClock {
			continue
		}
		p, err := variables.Parse(s.Field)
		if err != nil || p.Part != variables.PartBody {
			continue
		}
		leaf, _ := placeLeafPath(p) // every body place has one
		if _, has := learned[leaf]; has {
			continue
		}
		// CheckSources held each clock source's range: a cut above zero,
		// its min at most its max (models.DeclaredSource.Check).
		if RangeNeverChecked(*s.Clock) {
			continue
		}
		off, _ := declaredOffset(*s.Clock)
		if out == nil {
			out = make(map[string]models.ClockOffset, len(learned)+1)
			for k, v := range learned {
				out[k] = v
			}
		}
		out[leaf] = off
	}
	if out == nil {
		return learned
	}
	return out
}

// ReplayClocks is the clocks a replay checks in a recording at its body
// fields: learned, as the learn step stored them (models.MockSpec.Clocks,
// by leaf path), and, in a recording the background serves (background: a
// worker: call, or a once series a judge placed there, onto which no
// test's drift is learned), the check each clock rule of sources makes at
// its field (DeclaredClocks). A test's own recording keeps what its
// learning decided: the learn step read it and decided each declared field
// of it by its rule there (DeclaredClockLearned, Input.Source), and a
// rule's range at a field learning decided otherwise (a copy of a stored
// time a variable binds, a zero stamp) would refuse the very value it
// learned.
//
// Under the time default (timeForm), a declared field of the background's
// recording is checked by its form alone, as the learn step learns a
// declared field under it: the form of the recording's leaf there
// (recorded, by leaf path: TimeForm), so a call passes with any value of
// that type and form, whatever the rule's range. Only a leaf that names a
// moment (a time of day names none), and, for an epoch number, one near
// the recording's own call (span: its request and response times, within
// epochNear), as the learn step requires of a recorded value; the call's
// value is checked by its form alone (CheckClockRead). Any other recorded
// value, and a field recorded cannot read, keeps the rule's range: the
// default never passes more than its form says. With the time default off
// (keploy.yml's mock.times: strict) the rule's range decides. learned
// itself where nothing is added; otherwise a new map.
func ReplayClocks(sources []models.DeclaredSource, learned map[string]models.ClockOffset, background, timeForm bool, recorded func(leaf string) (Leaf, bool), span Span) map[string]models.ClockOffset {
	if !background {
		return learned
	}
	out := DeclaredClocks(sources, learned)
	if len(out) == len(learned) || !timeForm || recorded == nil {
		return out
	}
	if span.To.IsZero() {
		span.To = span.From
	}
	for k := range out {
		if _, has := learned[k]; has {
			continue
		}
		rec, ok := recorded(k)
		if !ok || !isInstant(rec) || strings.HasPrefix(leafLayout(rec), "epoch-") && !nearSpan(rec, span) {
			continue
		}
		out[k] = models.ClockOffset{Form: TimeForm(rec)} // an instant has a form
	}
	return out
}
