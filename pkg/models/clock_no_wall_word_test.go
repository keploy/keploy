package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// olderClockRead is a read of the clock as a mock file that a keploy before
// ClockOffset.WallWord wrote holds it: a client test's Sequelize stamp at
// UTC ("2026-10-30 05:09:21.102 +00:00") read as a wall clock of no zone
// in UTC, the word of its date and time, the zone in the 7 bytes after it.
const olderClockRead = `min: 0s
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

// A mock written before ClockOffset.WallWord was, whose reads name none,
// reads, writes and meets as it did: a read decodes from its file and
// writes back the same bytes, in YAML and in JSON, and two meet as before:
// one word, or both whole, at the zone both hold; a word and the value
// whole, two words of a text, or one word in two zones no one zone holds,
// as Empty. The row names nothing WallWord added, so it builds and passes
// on a keploy before it, as it does here.
func TestAReadThatNamesNoWallWordReadsWritesAndMeetsAsBefore(t *testing.T) {
	var stored ClockOffset
	if err := yaml.Unmarshal([]byte(olderClockRead), &stored); err != nil {
		t.Fatal(err)
	}
	if b, err := yaml.Marshal(stored); err != nil || string(b) != olderClockRead {
		t.Fatalf("the older read writes back as %q (%v); want its own bytes", b, err)
	}
	j, err := json.Marshal(stored)
	var back ClockOffset
	if err != nil || strings.Contains(string(j), "wall") || json.Unmarshal(j, &back) != nil || back != stored {
		t.Fatalf("the older read's JSON %s (%v) reads back as %+v; want %+v, naming no wall word", j, err, back, stored)
	}
	ms := time.Millisecond
	read := func(zone, app string, w ClockWord) ClockOffset {
		return ClockOffset{Res: ms, Zone: zone, AppZone: app, Layout: "iso-second-space", Word: w}
	}
	word, other := stored.Word, ClockWordOf("2026-10-30 05:09:21.102 +00:00", 0, 19)
	for _, row := range []struct {
		name       string
		a, b, want ClockOffset
	}{
		{"one word", stored, stored, stored},
		{"one word in UTC and in either zone", stored, read(ZoneEither, "UTC", word), stored},
		{"both whole", read("", "", ClockWord{}), read("", "", ClockWord{}), read("", "", ClockWord{})},
		{"a word and the value whole", stored, read("", "", ClockWord{}), ClockOffset{Res: ms, Zone: ZoneApart, Layout: LayoutApart}},
		{"two words of a text", stored, read(ZoneUTC, "", other), ClockOffset{Res: ms, Zone: ZoneUTC, Layout: LayoutApart}},
		{"one word in UTC and in the application's zone", stored, read(ZoneApp, "+05:30", word), ClockOffset{Res: ms, Zone: ZoneApart, Layout: "iso-second-space", Word: word}},
	} {
		for _, m := range []ClockOffset{row.a.Meet(row.b), row.b.Meet(row.a)} {
			if m != row.want || m.Empty() != row.want.Empty() {
				t.Errorf("%s: %+v met %+v as %+v; want %+v", row.name, row.a, row.b, m, row.want)
			}
			if b, err := yaml.Marshal(m); err != nil || strings.Contains(string(b), "wall") {
				t.Errorf("%s: the meet writes %q (%v); want no wall word", row.name, b, err)
			}
		}
	}
}
