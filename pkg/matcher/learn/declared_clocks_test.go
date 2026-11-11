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
		{"a range all before the call is a read plus that offset", []models.DeclaredSource{clock("cutoff", "body.cutoff", &models.ClockOffset{Min: -31 * time.Second, Max: -30 * time.Second, Res: time.Second})}, nil,
			map[string]models.ClockOffset{"body.cutoff": {Min: -31 * time.Second, Max: -30 * time.Second, Res: time.Second}}},
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

// ReplayClocks reads a clock rule's field in a recording the background
// serves only: a test's own recording keeps what its learning decided, so a
// stored time an update copies (a variable binds it) is never refused by a
// rule's range. Under the time default a declared field of the background's
// recording is checked by the form its recorded value is written in; one
// whose recorded value keploy reads as no time, or cannot read, keeps the
// range, and with the time default off the range decides.
func TestReplayClocksReadADeclaredRangeOnTheBackgroundOnlyAndItsFormUnderTheDefault(t *testing.T) {
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	ranged := models.ClockOffset{Res: time.Second}
	ttl := models.ClockOffset{Min: time.Hour, Max: time.Hour + time.Second, Res: time.Second}
	clock := func(rule, field string) models.DeclaredSource {
		return models.DeclaredSource{Rule: rule, Kind: models.DeclaredSourceClock, Field: field, Clock: plain}
	}
	both := []models.DeclaredSource{clock("pat-last-used-at", "body.data.lastUsedAt"), clock("pat-updated-at", "body.data.updatedAt")}
	touch := map[string]Leaf{
		"body.data.lastUsedAt": {Path: "body.data.lastUsedAt", Noise: "body.data.lastUsedAt", Type: Number, Value: "1794406599"},
		"body.data.updatedAt":  {Path: "body.data.updatedAt", Noise: "body.data.updatedAt", Type: String, Value: "2026-11-11T14:06:39Z"},
	}
	const epoch, rfc = "number 9999999999", "text 9999-99-99a99:99:99a"
	// span is the recording's own call: its request and response times.
	span := Span{From: time.Date(2026, 11, 11, 14, 6, 39, 0, time.UTC), To: time.Date(2026, 11, 11, 14, 6, 39, 20e6, time.UTC)}
	with := func(path string, l Leaf) map[string]Leaf {
		out := map[string]Leaf{}
		for k, v := range touch {
			out[k] = v
		}
		l.Path, l.Noise = path, path
		out[path] = l
		return out
	}
	read := func(leaves map[string]Leaf) func(string) (Leaf, bool) {
		return func(p string) (Leaf, bool) { l, ok := leaves[p]; return l, ok }
	}
	for _, c := range []struct {
		name                 string
		learned              map[string]models.ClockOffset
		background, timeForm bool
		recorded             func(string) (Leaf, bool)
		want                 map[string]models.ClockOffset
	}{
		{"a test's own recording keeps what learning decided", map[string]models.ClockOffset{"body.data.lastUsedAt": ttl}, false, true, read(touch),
			map[string]models.ClockOffset{"body.data.lastUsedAt": ttl}},
		{"a test's own recording, nothing learned, strict", nil, false, false, read(touch), nil},
		{"the background's, strict: the rules' ranges", nil, true, false, read(touch),
			map[string]models.ClockOffset{"body.data.lastUsedAt": ranged, "body.data.updatedAt": ranged}},
		{"the background's, the time default: each field's form", nil, true, true, read(touch),
			map[string]models.ClockOffset{"body.data.lastUsedAt": {Form: epoch}, "body.data.updatedAt": {Form: rfc}}},
		{"what learning learned on it stays", map[string]models.ClockOffset{"body.data.lastUsedAt": ttl}, true, true, read(touch),
			map[string]models.ClockOffset{"body.data.lastUsedAt": ttl, "body.data.updatedAt": {Form: rfc}}},
		{"a recorded value that is no time keeps the range", nil, true, true, read(map[string]Leaf{
			"body.data.lastUsedAt": touch["body.data.lastUsedAt"],
			"body.data.updatedAt":  {Path: "body.data.updatedAt", Noise: "body.data.updatedAt", Type: String, Value: "never"},
		}), map[string]models.ClockOffset{"body.data.lastUsedAt": {Form: epoch}, "body.data.updatedAt": ranged}},
		{"a field the recording does not hold keeps the range", nil, true, true, read(map[string]Leaf{"body.data.updatedAt": touch["body.data.updatedAt"]}),
			map[string]models.ClockOffset{"body.data.lastUsedAt": ranged, "body.data.updatedAt": {Form: rfc}}},
		{"an epoch two years before its call keeps the range", nil, true, true, read(with("body.data.lastUsedAt", Leaf{Type: Number, Value: "1731334599"})),
			map[string]models.ClockOffset{"body.data.lastUsedAt": ranged, "body.data.updatedAt": {Form: rfc}}},
		{"a time of day keeps the range", nil, true, true, read(with("body.data.updatedAt", Leaf{Type: String, Value: "14:06:39"})),
			map[string]models.ClockOffset{"body.data.lastUsedAt": {Form: epoch}, "body.data.updatedAt": ranged}},
		{"a text instant far from its call is checked by its form", nil, true, true, read(with("body.data.updatedAt", Leaf{Type: String, Value: "2024-11-11T14:06:39Z"})),
			map[string]models.ClockOffset{"body.data.lastUsedAt": {Form: epoch}, "body.data.updatedAt": {Form: rfc}}},
		{"no reader keeps the ranges", nil, true, true, nil,
			map[string]models.ClockOffset{"body.data.lastUsedAt": ranged, "body.data.updatedAt": ranged}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := map[string]models.ClockOffset{}
			for k, v := range c.learned {
				before[k] = v
			}
			got := ReplayClocks(both, c.learned, c.background, c.timeForm, c.recorded, span)
			if len(got) != len(c.want) {
				t.Fatalf("got %+v; want %+v", got, c.want)
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
	// A recording with no response time is near its call at its request.
	got := ReplayClocks(both, nil, true, true, read(touch), Span{From: span.From})
	if got["body.data.lastUsedAt"] != (models.ClockOffset{Form: epoch}) {
		t.Fatalf("no response time: %+v; want lastUsedAt by its form", got)
	}
}
