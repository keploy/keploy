package learn

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func changed(path, recorded, live string) models.MockFieldDiff {
	return models.MockFieldDiff{Path: path, Kind: models.DiffKindValueChanged, Expected: recorded, Actual: live}
}

// api-server's ordinal recompute, a timer: findAndModify ptw_endpoints
// {dirtyAt: {$lte: <now>}}. Its recording and the call a later round made
// differ only in the instant, which a Mongo call's canonical form writes as
// a typed date: the recording with the clock moved.
func TestAMongoTimersNowFilterIsTheRecordingWithTheClockMoved(t *testing.T) {
	d := []models.MockFieldDiff{changed("body.filter.dirtyAt.$lte.$date.$numberLong", "1798456789123", "1798456851977")}
	if !ClockOnly(d, false) {
		t.Fatalf("ClockOnly(%v) = false: the call differs from its recording only in the typed date's instant", d)
	}
}

// Instants whose text names their day are clock reads wherever they stand:
// an ISO 8601 value, one inside other text, an RFC 1123 one. Each moved by
// an amount no round's lead explains: a timer fires at another moment.
func TestATextInstantThatMovedByAnyAmountIsTheClock(t *testing.T) {
	for _, d := range []models.MockFieldDiff{
		changed("body.expiresBefore", "2026-10-29T08:00:00Z", "2026-10-29T08:03:17.512Z"),
		changed("body.note", "swept before 2026-10-29 08:00:00 by the janitor", "swept before 2026-10-29 09:12:44 by the janitor"),
		changed("header.If-Modified-Since", "Thu, 29 Oct 2026 08:00:00 GMT", "Thu, 29 Oct 2026 08:00:09 GMT"),
		changed("query.since", "20261029T080000Z", "20261029T081500Z"),
	} {
		if !ClockOnly([]models.MockFieldDiff{d}, false) {
			t.Errorf("ClockOnly(%+v) = false: only an instant changed", d)
		}
	}
}

// What is not the clock alone: a number that only looks like an epoch
// outside a typed date (a phone number), a time of day (it names no day),
// an instant beside another word that changed or was added, a UUIDv7 (its
// random part changed too), an instant out of keploy's range on either side,
// a field present on one side only, a type change, a test variable's place
// (the variable did not hold: its own reason), and two fields of which one
// is no instant.
func TestWhatElseChangedIsNotTheClock(t *testing.T) {
	clock := changed("body.filter.dirtyAt.$lte.$date.$numberLong", "1798456789123", "1798456851977")
	for name, d := range map[string][]models.MockFieldDiff{
		"phone number":        {changed("body.phone", "2025551234", "2025559876")},
		"epoch not typed":     {changed("body.ts", "1798456789123", "1798456851977")},
		"time of day":         {changed("body.at", "10:00:41", "10:00:45")},
		"another word":        {changed("body.note", "job-17 at 2026-10-29T08:00:00Z", "job-18 at 2026-10-29T08:00:05Z")},
		"uuid v7":             {changed("body.id", "019a2f3c-8d1e-7abc-9def-0123456789ab", "019a2f3d-1122-7f00-8abc-fedcba987654")},
		"missing in live":     {{Path: "body.dirtyAt", Kind: models.DiffKindMissingInLive, Expected: "2026-10-29T08:00:00Z"}},
		"type changed":        {{Path: "body.dirtyAt", Kind: models.DiffKindTypeChanged, Expected: "string: 2026-10-29T08:00:00Z", Actual: "null: "}},
		"second field":        {clock, changed("body.update.$set.owner", "worker-1", "worker-2")},
		"same text":           {changed("body.dirtyAt", "2026-10-29T08:00:00Z", "2026-10-29T08:00:00Z")},
		"instant out of use":  {changed("body.v", "1999-01-01T00:00:00Z", "1999-01-01T00:00:05Z")},
		"recorded out of use": {changed("body.v", "1999-12-31T23:59:59Z", "2026-10-29T08:00:00Z")},
		"a word added":        {changed("body.note", "at 2026-10-29T08:00:00Z", "at 2026-10-29T08:00:05Z again")},
		"a variable's place":  {{Path: "body.since", Kind: models.DiffKindVariable, Expected: "2026-10-29T08:00:00Z", Actual: "2026-10-29T08:00:05Z"}},
	} {
		if ClockOnly(d, false) {
			t.Errorf("%s: ClockOnly(%+v) = true", name, d)
		}
	}
}

// A diff that may be cut says nothing of what it left out: a list the parser
// cut at its bound (cut), a value cut at its bound ("…"), and no diff at all
// (a miss with no closest recording, or one whose parser gives no diff).
func TestACutOrEmptyDiffIsNotTheClock(t *testing.T) {
	clock := changed("body.filter.dirtyAt.$lte.$date.$numberLong", "1798456789123", "1798456851977")
	if ClockOnly([]models.MockFieldDiff{clock}, true) {
		t.Error("ClockOnly(cut list) = true: the fields the parser left out may differ")
	}
	long := "swept before 2026-10-29T08:00:00Z " + strings.Repeat("x", 300)
	cutValue := changed("body.note", long[:256]+"…", strings.Replace(long, "08:00:00", "08:00:05", 1)[:256]+"…")
	if ClockOnly([]models.MockFieldDiff{cutValue}, false) {
		t.Error("ClockOnly(cut value) = true: the value's rest may differ")
	}
	if ClockOnly(nil, false) {
		t.Error("ClockOnly(nil) = true: no diff says no field is the clock")
	}
}
