package learn

import (
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// utcStampForms are stamps whose zone after a space is UTC's, as an
// application writes its clock read: Sequelize's default, Go's
// time.Time.String() (the zone's name after it), Ruby's Time#to_s, and
// both with the zone's other sign; each with the text after the date and
// time a keploy that did not read such a zone left in the text after its
// word, and the text after the instant this keploy reads (Go's " UTC").
var utcStampForms = []struct {
	name, layout, wallAfter, after string
}{
	{"Sequelize", "2006-01-02 15:04:05.000 -07:00", " +00:00", ""},
	{"Sequelize, -00:00", "2006-01-02 15:04:05.000 -00:00", " -00:00", ""},
	{"Go", "2006-01-02 15:04:05.999999999 -0700 MST", " +0000 UTC", " UTC"},
	{"Ruby", "2006-01-02 15:04:05 -0700", " +0000", ""},
	{"Ruby, -0000", "2006-01-02 15:04:05 -0000", " -0000", ""},
}

// In keploy mock's client tests, a read of an instant whose zone after a
// space is UTC's names the word of its date and time
// (models.ClockOffset.WallWord): where a keploy that did not read such a
// zone learned the place, a wall clock of no zone with the zone in the text
// after it, as this one reads the instant, whole or at a word. One whose
// zone is not UTC's, an HTTP date that writes its zone after a space, and a
// read outside client tests name none.
func TestAClientTestsReadOfAUTCStampNamesTheWordOfItsDateAndTime(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	in := func(zone *time.Location, layout string) func(time.Time) string {
		return func(r time.Time) string { return r.In(zone).Format(layout) }
	}
	for _, f := range utcStampForms {
		for _, app := range []*time.Location{nil, time.UTC} {
			ln, rec := classifyWordAt(clientTestCacheIn(app), in(time.UTC, f.layout), -100*time.Millisecond, false)
			o, ok := ln.Clock()
			var word models.ClockWord
			if f.after != "" {
				word = sides{"", f.after}.at(t, rec)
			}
			if !ok || ln.Kind != KindClockRead || o.Zone != "" || o.Word != word || o.WallWord != (sides{"", f.wallAfter}.at(t, rec)) {
				t.Errorf("%s (%q), app zone %v: %s %+v (%s); want a read of the instant at %+v naming the word before %q", f.name, rec, app, ln.Kind, o, ln.Why, word, f.wallAfter)
			}
		}
	}
	for _, row := range []struct {
		name string
		dc   *DriftCache
		f    func(time.Time) string
	}{
		{"Sequelize at +05:30", clientTestCacheIn(ist), in(ist, utcStampForms[0].layout)},
		{"an HTTP date", clientTestCache(), in(time.UTC, time.RFC1123Z)},
		{"no client test", clockedDrifts(), in(time.UTC, utcStampForms[0].layout)},
	} {
		ln, rec := classifyWordAt(row.dc, row.f, -100*time.Millisecond, false)
		if o, ok := ln.Clock(); !ok || !o.WallWord.IsZero() {
			t.Errorf("%s (%q): %s %+v (%s); want a read that names no word", row.name, rec, ln.Kind, o, ln.Why)
		}
	}
	// A word of the value decoded once stands at places of the decoded
	// text, so the recorded text's bytes there say nothing of its zone: it
	// names no word, even where those bytes end in UTC's zone after a space.
	rec := "q=" + "2026-10-30 05:09:21.102 +00:00"
	asRecorded := models.ClockWordOf(rec, 2, len(rec))
	decoded := asRecorded
	decoded.Form = models.ClockWordDecoded
	for _, w := range []models.ClockWord{decoded, asRecorded} {
		read := models.ClockOffset{Res: time.Millisecond, Layout: "iso-second-space", Word: w}
		got := withWallWord(Learned{Kind: KindClockRead, Recorded: Leaf{Value: rec}, Read: &read})
		if named := !got.Read.WallWord.IsZero(); named != (w.Form == "") {
			t.Errorf("a read at %+v of %q named the word %+v; want one only of a word read as recorded", w, rec, got.Read.WallWord)
		}
	}
}

// olderWordRead is the clock read keploy before WallWord learned of a
// client test's Sequelize stamp at UTC ("2026-10-30 05:09:21.102 +00:00"),
// as its mock file holds it: a wall clock of no zone in UTC, the word of
// the stamp's date and time, the zone in the 7 bytes after it.
const olderWordRead = `
min: 0s
max: 0s
res: 1ms
zone: utc
layout: iso-second-space
word:
    start: 0
    end: 23
    after: 7
    before_sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
    after_sha256: b3ed0a051b925a16d3da3da40c56dc7676634499a1ad45f91238761f26080257
`

// A mock that holds an older keploy's word read of a client test's stamp
// whose zone after a space is UTC's (olderWordRead, or the same word read
// in either zone, as an application in UTC learned it), learned again by
// this keploy, which reads the instant whole or at a word, keeps one read
// of the place (MergeLearned): the word read in UTC, which holds what both
// hold, never the Empty read of two forms of the value, which refuses every
// value there.
func TestAnOlderWordReadOfAUTCStampMeetsThisKeploysReadAsTheWordInUTC(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const p = "body.cmd.query.app.name"
	var stored models.ClockOffset
	if err := yamlv3.Unmarshal([]byte(olderWordRead), &stored); err != nil {
		t.Fatal(err)
	}
	for _, f := range utcStampForms {
		f := f
		ln, rec := classifyWordAt(clientTestCache(), func(r time.Time) string { return r.UTC().Format(f.layout) }, -100*time.Millisecond, false)
		o, ok := ln.Clock()
		if !ok {
			t.Fatalf("%s: %s (%s); want a read", f.name, ln.Kind, ln.Why)
		}
		old := models.ClockOffset{Res: o.Res, Zone: models.ZoneUTC, Layout: o.Layout, Word: sides{"", f.wallAfter}.at(t, rec)}
		if f.name == "Sequelize" && old != stored {
			t.Fatalf("the older word read %+v; want it as its mock holds it, %+v", old, stored)
		}
		for _, zone := range []string{models.ZoneUTC, models.ZoneEither} {
			was := old
			was.Zone = zone
			if zone == models.ZoneEither {
				was.AppZone = "UTC"
			}
			m := &models.Mock{Name: "mock-1", Kind: models.HTTP, Spec: models.MockSpec{Clocks: map[string]models.ClockOffset{p: was}}}
			added := MergeLearned(m, models.MockState{Name: "mock-1", Clocks: map[string]models.ClockOffset{p: o}}, nil)
			got := m.Spec.Clocks[p]
			if got != old || got.Empty() || (len(added) == 0) != (zone == models.ZoneUTC) {
				t.Errorf("%s, the older read in %s, met this keploy's %+v as %+v, adding %q; want %+v", f.name, zone, o, got, added, old)
			}
		}
	}
}
