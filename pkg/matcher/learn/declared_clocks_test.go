package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A clock rule's field is checked as a read of the clock in a recording no
// learning read: DeclaredClocks keys the rule's range by the field's leaf
// path, beside what the learn step learned. What learning learned at a
// place stays; a minted source, a field outside the body and sources this
// keploy cannot decide by add nothing, and learned is never written to.
func TestDeclaredClocksNameAClockRulesFieldWhereverItStands(t *testing.T) {
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	ttl := models.ClockOffset{Min: time.Hour, Max: time.Hour + time.Second, Res: time.Second}
	clock := func(rule, field string, c *models.ClockOffset) models.DeclaredSource {
		return models.DeclaredSource{Rule: rule, Kind: models.DeclaredSourceClock, Field: field, Clock: c}
	}
	lastUsed := clock("pat-last-used-at", "body.data.lastUsedAt", plain)
	updated := clock("pat-updated-at", "body.data.updatedAt", plain)
	for _, c := range []struct {
		name    string
		sources []models.DeclaredSource
		learned map[string]models.ClockOffset
		want    map[string]models.ClockOffset
	}{
		{"both fields of the PAT touch", []models.DeclaredSource{lastUsed, updated}, nil,
			map[string]models.ClockOffset{"body.data.lastUsedAt": {Res: time.Second}, "body.data.updatedAt": {Res: time.Second}}},
		{"a range all before the call is a read plus that offset", []models.DeclaredSource{clock("cutoff", "body.cutoff", &models.ClockOffset{Min: -30 * time.Minute, Max: -30*time.Minute + time.Second, Res: time.Second})}, nil,
			map[string]models.ClockOffset{"body.cutoff": {Min: -30 * time.Minute, Max: -30*time.Minute + time.Second, Res: time.Second}}},
		// models.ClockOffset.Holds holds no offset off zero whose unit is a
		// second or finer, so such a range is left out (it compares as a
		// field no rule names), whatever its resolution.
		{"an off-zero range of seconds is never checked", []models.DeclaredSource{clock("cutoff", "body.cutoff", &models.ClockOffset{Min: -3 * time.Second, Max: -2 * time.Second, Res: time.Second})}, nil, nil},
		{"an off-zero range of milliseconds is never checked", []models.DeclaredSource{clock("cutoff", "body.cutoff", &models.ClockOffset{Min: 1500 * time.Millisecond, Max: 2 * time.Second, Res: time.Millisecond})}, nil, nil},
		{"an off-zero range of seconds beside a plain read", []models.DeclaredSource{lastUsed, clock("cutoff", "body.cutoff", &models.ClockOffset{Min: -3 * time.Second, Max: -2 * time.Second, Res: time.Second})}, nil,
			map[string]models.ClockOffset{"body.data.lastUsedAt": {Res: time.Second}}},
		{"what learning learned stays", []models.DeclaredSource{lastUsed, updated}, map[string]models.ClockOffset{"body.data.lastUsedAt": ttl},
			map[string]models.ClockOffset{"body.data.lastUsedAt": ttl, "body.data.updatedAt": {Res: time.Second}}},
		{"a minted source is no clock", []models.DeclaredSource{{Rule: "id", Kind: models.DeclaredSourceMinted, Field: "body.data._id"}}, nil, nil},
		{"a header is no body field", []models.DeclaredSource{clock("sent-at", "header.x-sent-at", plain)}, nil, nil},
		{"a field that does not parse", []models.DeclaredSource{clock("bad", "body.data[", plain)}, nil, nil},
		{"two rules on one field", []models.DeclaredSource{lastUsed, clock("again", "body.data.lastUsedAt", plain)}, nil, nil},
		{"no sources", nil, map[string]models.ClockOffset{"body.since": ttl}, map[string]models.ClockOffset{"body.since": ttl}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var before map[string]models.ClockOffset
			if c.learned != nil {
				before = map[string]models.ClockOffset{}
				for k, v := range c.learned {
					before[k] = v
				}
			}
			got := DeclaredClocks(c.sources, c.learned)
			if len(got) != len(c.want) {
				t.Fatalf("got %v; want %v", got, c.want)
			}
			for k, v := range c.want {
				if g, ok := got[k]; !ok || g != v {
					t.Fatalf("at %s got %+v (%v); want %+v", k, g, ok, v)
				}
			}
			if len(c.learned) != len(before) {
				t.Fatalf("learned was written to: %v; was %v", c.learned, before)
			}
			for k, v := range before {
				if c.learned[k] != v {
					t.Fatalf("learned[%s] was written to: %+v; was %+v", k, c.learned[k], v)
				}
			}
		})
	}
}

// A replay checks a clock rule's range unless Holds never holds it: a range
// all on one side of zero whose unit is a second or finer. A range around
// zero, one of minutes or hours, and one a rule cannot declare (no
// resolution, a max below its min) are no such range.
func TestRangeNeverCheckedIsAnOffZeroRangeOfASecondOrFiner(t *testing.T) {
	for _, c := range []struct {
		name string
		o    models.ClockOffset
		want bool
	}{
		{"a cut-off 3 s to 2 s before the call", models.ClockOffset{Min: -3 * time.Second, Max: -2 * time.Second, Res: time.Second}, true},
		{"an expiry 1.5 s to 2 s after it, in ms", models.ClockOffset{Min: 1500 * time.Millisecond, Max: 2 * time.Second, Res: time.Millisecond}, true},
		{"a plain read", models.ClockOffset{Min: -time.Second, Res: time.Second}, false},
		{"a range around zero", models.ClockOffset{Min: -3 * time.Second, Max: 2 * time.Second, Res: time.Second}, false},
		{"a cut-off of minutes", models.ClockOffset{Min: -30 * time.Minute, Max: -30*time.Minute + time.Second, Res: time.Second}, false},
		{"an expiry an hour on", models.ClockOffset{Min: time.Hour, Max: time.Hour + time.Second, Res: time.Second}, false},
		{"no resolution", models.ClockOffset{Min: -3 * time.Second, Max: -2 * time.Second}, false},
		{"a max below its min", models.ClockOffset{Min: -2 * time.Second, Max: -3 * time.Second, Res: time.Second}, false},
	} {
		if got := RangeNeverChecked(c.o); got != c.want {
			t.Errorf("%s: RangeNeverChecked(%+v) = %v; want %v", c.name, c.o, got, c.want)
		}
	}
}
