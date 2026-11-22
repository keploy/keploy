package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
)

// wallWordStamp is a Sequelize stamp at UTC as a client test's call sent
// it, and its date and time alone: the word a keploy that did not read a
// zone written after a space learned there, a wall clock of no zone.
const wallWordStamp = "2026-10-30 05:09:21.102 +00:00"

// A word read as a wall clock of an instant's date and time (Word), and
// that instant read with its zone after a space, UTC's (WallWord names the
// word), are one read of the clock: they meet as the word read in UTC, in
// either order and grouped either way, holding what both hold. Not where
// the word was read in the application's zone alone (ZoneApart), nor where
// the other names no such word, another word, or no moment of its own, or
// where the word read names a WallWord of its own: those stay two forms of
// the value (Empty), as before. WallWord survives a YAML, JSON and BSON
// round trip, and a read of none writes no field.
func TestAWallClockWordAndTheInstantItIsMeetAsTheWordInUTC(t *testing.T) {
	ms := time.Millisecond
	word := ClockWordOf(wallWordStamp, 0, 23)
	other := ClockWordOf(wallWordStamp, 0, 19)
	goWord := ClockWordOf("2026-10-30 05:09:21.102 +0000 UTC", 0, 23)
	wall := func(zone, app string) ClockOffset {
		return ClockOffset{Res: ms, Zone: zone, AppZone: app, Layout: "iso-second-space", Word: word}
	}
	instant := ClockOffset{Res: ms, Layout: "iso-second-space", WallWord: word}
	goInstant := ClockOffset{Res: time.Nanosecond, Layout: "iso-second-space", Word: ClockWordOf("2026-10-30 05:09:21.102 +0000 UTC", 0, 29), WallWord: goWord}
	goWall := ClockOffset{Res: time.Nanosecond, Zone: ZoneUTC, Layout: "iso-second-space", Word: goWord}
	at := time.Date(2026, 10, 30, 5, 9, 21, 102e6, time.UTC)
	from, to := at.Add(-ms), at.Add(ms)
	for _, row := range []struct {
		name     string
		a, b     ClockOffset
		want     ClockOffset
		apart    bool
		zoneApar bool
	}{
		{name: "a word in UTC", a: wall(ZoneUTC, ""), b: instant, want: wall(ZoneUTC, "")},
		{name: "a word in either zone", a: wall(ZoneEither, "UTC"), b: instant, want: wall(ZoneUTC, "")},
		{name: "Go's word, the zone and its name after it", a: goWall, b: goInstant, want: goWall},
		{name: "a word in the application's zone alone", a: wall(ZoneApp, "+05:30"), b: instant, zoneApar: true},
		{name: "an instant that names no word", a: wall(ZoneUTC, ""), b: ClockOffset{Res: ms, Layout: "iso-second-space"}, apart: true},
		{name: "an instant that names another word", a: wall(ZoneUTC, ""), b: ClockOffset{Res: ms, Layout: "iso-second-space", WallWord: other}, apart: true},
		{name: "a wall clock that names the word", a: wall(ZoneUTC, ""), b: ClockOffset{Res: ms, Zone: ZoneUTC, Layout: "iso-second-space", WallWord: word}, zoneApar: true},
		{name: "two reads of the instant", a: instant, b: instant, want: instant},
		{name: "a word that names a WallWord of its own", a: ClockOffset{Res: ms, Zone: ZoneUTC, Layout: "iso-second-space", Word: word, WallWord: other}, b: instant, apart: true},
		// No learning names the word of its date and time at two places of
		// the text (withWallWord's is the read's own), so a mock that does
		// was written otherwise: its two reads stay two forms of the value.
		{name: "two reads that name the word, read at two places", a: instant, b: ClockOffset{Res: ms, Layout: "iso-second-space", Word: other, WallWord: word}, apart: true},
		{name: "a whole read and a word that names no WallWord", a: ClockOffset{Res: ms, Zone: ZoneUTC, Layout: "iso-second-space"}, b: ClockOffset{Res: time.Nanosecond, Layout: "iso-second-space", Word: goInstant.Word}, apart: true},
	} {
		for i, got := range []ClockOffset{row.a.Meet(row.b), row.b.Meet(row.a)} {
			switch {
			case row.apart:
				if got.Layout != LayoutApart || !got.Empty() || got.Holds(at, ms, from, to) {
					t.Errorf("%s, order %d: met as %+v; want LayoutApart, Empty, holding nothing", row.name, i, got)
				}
			case row.zoneApar:
				if got.Zone != ZoneApart || !got.Empty() || got.Holds(at, ms, from, to) {
					t.Errorf("%s, order %d: met as %+v; want ZoneApart, Empty, holding nothing", row.name, i, got)
				}
			case got != row.want || got.Empty() || !got.Holds(at, got.Res, from, to):
				t.Errorf("%s, order %d: met as %+v (Empty %v); want %+v, holding the read", row.name, i, got, got.Empty(), row.want)
			}
		}
	}
	// A read of the instant that names the word and one that names none
	// (no learning of keploy mock's client tests, where each read of such
	// an instant names it) read it at two places: they hold nothing
	// together, met with the word read too, in any grouping.
	plain := ClockOffset{Res: ms, Layout: "iso-second-space"}
	for _, got := range []ClockOffset{instant.Meet(plain), instant.Meet(plain).Meet(wall(ZoneUTC, "")), instant.Meet(plain.Meet(wall(ZoneUTC, ""))), instant.Meet(wall(ZoneUTC, "")).Meet(plain)} {
		if !got.Empty() || !got.WallWord.IsZero() || !got.Word.IsZero() {
			t.Errorf("an instant that names the word met one that does not as %+v; want Empty, no Word, no WallWord", got)
		}
	}
	// Meet is the same in either order and however grouped, over the
	// learnings of a place these fields make: a word read as a wall clock,
	// whole or at a word, in any zone, and a read of an instant at the
	// text's end (a Sequelize stamp) or at a word (Go's, its zone's name
	// after it) that names the word of its date and time, in any zone (none
	// but "" is learned), each in any layout. A read of an instant at a
	// Word its WallWord does not fix ({other, word}) is left out: no
	// learning makes it, and grouping tells there (meetWord).
	var all []ClockOffset
	for _, pair := range [][2]ClockWord{{}, {word, {}}, {other, {}}, {goWord, {}}, {goInstant.Word, {}}, {{}, word}, {goInstant.Word, goWord}} {
		for _, z := range []string{"", ZoneUTC, ZoneApp, ZoneEither, ZoneApart} {
			for _, l := range []string{"", "iso-second-space", LayoutApart} {
				all = append(all, ClockOffset{Res: ms, Zone: z, Layout: l, Word: pair[0], WallWord: pair[1]})
			}
		}
	}
	for _, a := range all {
		for _, b := range all {
			ab := a.Meet(b)
			if ab != b.Meet(a) {
				t.Fatalf("%+v and %+v meet as %+v one way and %+v the other", a, b, ab, b.Meet(a))
			}
			for _, c := range all {
				if x, y := ab.Meet(c), a.Meet(b.Meet(c)); x != y {
					t.Fatalf("%+v, %+v, %+v grouped two ways: %+v and %+v", a, b, c, x, y)
				}
			}
		}
	}
	y, err := yaml.Marshal(instant)
	if err != nil || !strings.Contains(string(y), "wall_word:") {
		t.Fatalf("YAML of an instant that names its word: %q, %v", y, err)
	}
	var back ClockOffset
	if err := yaml.Unmarshal(y, &back); err != nil || back != instant {
		t.Errorf("YAML round trip: %+v, %v; want %+v", back, err, instant)
	}
	j, err := json.Marshal(instant)
	back = ClockOffset{}
	if err != nil || json.Unmarshal(j, &back) != nil || back != instant {
		t.Errorf("JSON round trip of %s: %+v; want %+v", j, back, instant)
	}
	b, err := bson.Marshal(instant)
	back = ClockOffset{}
	if err != nil || bson.Unmarshal(b, &back) != nil || back != instant {
		t.Errorf("BSON round trip: %+v, %v; want %+v", back, err, instant)
	}
	for _, enc := range []func(any) ([]byte, error){yaml.Marshal, json.Marshal} {
		if out, _ := enc(wall(ZoneUTC, "")); strings.Contains(string(out), "wall_word") {
			t.Errorf("a read that names no WallWord wrote one: %s", out)
		}
	}
}
