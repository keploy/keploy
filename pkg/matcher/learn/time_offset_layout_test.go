package learn

import (
	"encoding/base64"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// RFC 3339 / ISO 8601 timezone offset forms (Z, +hh:mm, -hh:mm, +hhmm, -hhmm,
// +hh, -hh) are one format under the time default: a T2 recording made in IST
// (+05:30) and replayed under TZ=UTC (Z) — or with another numeric offset —
// passes by form, both for new recordings and for already-persisted recordings
// carrying legacy +99:99/-99:99 masks. Non-offset layout differences still fail.
func TestTimeFormTreatsISO8601OffsetsAsOneForm(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+30*60)
	rAt := time.Date(2026, 11, 26, 9, 35, 52, 123456000, time.UTC)
	fAt := rAt.Add(time.Hour)
	recIST := rAt.Add(-50 * time.Millisecond).In(ist).Format(time.RFC3339Nano)
	fIST := fAt.Add(-50 * time.Millisecond).In(ist).Format(time.RFC3339Nano)

	// Raw skeletons on disk remain compatible with prior releases (v3.7.26):
	// +05:30 and +00:00 keep +99:99, -05:00 keeps -99:99, Z keeps a.
	for _, tc := range []struct {
		sample, wantRaw string
	}{
		{recIST, "text 9999-99-99a99:99:99+99:99"},
		{"2026-11-26T09:35:52+00:00", "text 9999-99-99a99:99:99+99:99"},
		{"2026-11-26T04:35:52-05:00", "text 9999-99-99a99:99:99-99:99"},
		{"2026-11-26T09:35:52Z", tForm},
		{"2026-11-26T15:05:52+0530", "text 9999-99-99a99:99:99+9999"},
		{"2026-11-26T04:35:52-0500", "text 9999-99-99a99:99:99-9999"},
	} {
		if got := TimeForm(Leaf{Type: String, Value: tc.sample}); got != tc.wantRaw {
			t.Fatalf("TimeForm(%q) = %q; want raw %q for v3.7.26 compatibility", tc.sample, got, tc.wantRaw)
		}
	}

	in := iCall(cPath, String, recIST, fIST, rAt, fAt, iWin(rAt, fAt, 100*time.Millisecond), nil, nil)
	in.TimeForm = true
	ds := Decide(in)
	if len(ds) != 1 || ds[0].Outcome != OutcomeClock || ds[0].Clock == nil {
		t.Fatalf("Decide on IST recording: %s; want OutcomeClock", oDescribe(ds))
	}
	o := *ds[0].Clock
	const rawISTForm = "text 9999-99-99a99:99:99+99:99"
	if o.Form != rawISTForm || o.FormF != "" {
		t.Fatalf("IST recording learned form %q (%q); want raw %q with no FormF for v3.7.26 compatibility", o.Form, o.FormF, rawISTForm)
	}

	// Replay under TZ=UTC produces Z; also check other RFC 3339 / ISO 8601 offsets.
	for _, tc := range []struct {
		what string
		val  string
	}{
		{"UTC Z", time.Now().UTC().Format(time.RFC3339Nano)},
		{"UTC Z whole second", "2026-11-26T09:37:10Z"},
		{"IST +05:30", "2026-11-26T15:07:10.123456+05:30"},
		{"EST -05:00", "2026-11-26T04:37:10-05:00"},
		{"UTC +00:00", "2026-11-26T09:37:10+00:00"},
		{"compact +0530", "2026-11-26T15:07:10+0530"},
		{"compact -0500", "2026-11-26T04:37:10-0500"},
		{"hour-only +05", "2026-11-26T15:07:10+05"},
		{"hour-only -05", "2026-11-26T04:37:10-05"},
	} {
		live := Leaf{Type: String, Value: tc.val}
		if ok, why := CheckClockReadForm(cPath, o, live, Span{}, false, true); !ok {
			t.Errorf("%s (%q) against IST-recorded %q failed: %s", tc.what, tc.val, o.Form, why)
		}
		if !ReadsAsClockRead(o, live) {
			t.Errorf("%s (%q): ReadsAsClockRead = false; want true", tc.what, tc.val)
		}
		if !HoldsByForm(o, live, true) {
			t.Errorf("%s (%q): HoldsByForm = false; want true", tc.what, tc.val)
		}
	}

	// Legacy recordings already on disk with +99:99 / -99:99 / +9999 / -9999 / +99 / -99
	// must also pass UTC Z replays without re-recording.
	liveUTC := Leaf{Type: String, Value: "2026-11-26T09:37:10.456789Z"}
	for _, legacyForm := range []string{
		"text 9999-99-99a99:99:99+99:99",
		"text 9999-99-99a99:99:99-99:99",
		"text 9999-99-99a99:99:99+9999",
		"text 9999-99-99a99:99:99-9999",
		"text 9999-99-99a99:99:99+99",
		"text 9999-99-99a99:99:99-99",
	} {
		legacy := models.ClockOffset{Form: legacyForm}
		if ok, why := CheckClockReadForm(cPath, legacy, liveUTC, Span{}, false, true); !ok {
			t.Errorf("legacy %q replayed under UTC (%q) failed: %s", legacyForm, liveUTC.Value, why)
		}
		if !ReadsAsClockRead(legacy, liveUTC) {
			t.Errorf("legacy %q: ReadsAsClockRead(%q) = false; want true", legacyForm, liveUTC.Value)
		}
		if !HoldsByForm(legacy, liveUTC, true) {
			t.Errorf("legacy %q: HoldsByForm(%q) = false; want true", legacyForm, liveUTC.Value)
		}
	}

	// Typed Instant, space-separated ISO, minute/hour ISO, and space-before-offset.
	for _, tc := range []struct {
		what      string
		rec, live Leaf
		want      bool
	}{
		{"space-separated IST vs UTC Z", Leaf{Type: String, Value: "2026-11-26 15:05:52+05:30"}, Leaf{Type: String, Value: "2026-11-26 09:35:52Z"}, true},
		{"minute ISO IST vs UTC Z", Leaf{Type: String, Value: "2026-11-26T15:05+05:30"}, Leaf{Type: String, Value: "2026-11-26T09:35Z"}, true},
		{"hour ISO IST vs UTC Z", Leaf{Type: String, Value: "2026-11-26T15+05:30"}, Leaf{Type: String, Value: "2026-11-26T09Z"}, true},
		{"space-before-offset +05:30 vs -05:00", Leaf{Type: String, Value: "2026-11-26 15:05:52 +05:30"}, Leaf{Type: String, Value: "2026-11-26 04:05:52 -05:00"}, true},
		{"space-before-offset +05:30 vs +0530", Leaf{Type: String, Value: "2026-11-26 15:05:52 +05:30"}, Leaf{Type: String, Value: "2026-11-26 15:05:52 +0530"}, true},

		// Non-offset layout differences MUST still be refused.
		{"T vs space separator", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: String, Value: "2026-11-26 09:35:52Z"}, false},
		{"zoned vs naive", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: String, Value: "2026-11-26T09:35:52"}, false},
		{"naive vs zoned", Leaf{Type: String, Value: "2026-11-26T09:35:52"}, Leaf{Type: String, Value: "2026-11-26T09:35:52Z"}, false},
		{"datetime vs date-only", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: String, Value: "2026-11-26"}, false},
		{"second vs minute precision", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: String, Value: "2026-11-26T09:35Z"}, false},
		{"RFC 3339 vs RFC 1123", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: String, Value: "Thu, 26 Nov 2026 09:35:52 GMT"}, false},
		{"RFC 3339 vs named UTC zone", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: String, Value: "2026-11-26T09:35:52 UTC"}, false},
		{"space-before-offset vs no space before Z", Leaf{Type: String, Value: "2026-11-26 15:05:52 +05:30"}, Leaf{Type: String, Value: "2026-11-26 09:35:52Z"}, false},
		{"RFC 3339 vs epoch number", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: Number, Value: "1795685752"}, false},
		{"typed Instant vs untyped IST text", Leaf{Type: Instant, Value: "2026-11-26T09:35:52.000Z"}, Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, false},
		{"RFC 3339 vs non-time string", Leaf{Type: String, Value: "2026-11-26T15:05:52+05:30"}, Leaf{Type: String, Value: "not-a-time"}, false},
	} {
		t.Run(tc.what, func(t *testing.T) {
			recForm := TimeForm(tc.rec)
			if recForm == "" {
				t.Fatalf("TimeForm(%+v) = empty", tc.rec)
			}
			co := models.ClockOffset{Form: recForm}
			ok, why := CheckClockReadForm(cPath, co, tc.live, Span{}, false, true)
			if ok != tc.want {
				t.Fatalf("CheckClockReadForm(%q vs %+v) = %v (%s); want %v", recForm, tc.live, ok, why, tc.want)
			}
			if got := ReadsAsClockRead(co, tc.live); got != tc.want {
				t.Fatalf("ReadsAsClockRead(%q vs %+v) = %v; want %v", recForm, tc.live, got, tc.want)
			}
		})
	}

	// JWT string time claim recorded with legacy IST form passes live UTC Z token.
	enc := base64.RawURLEncoding
	jwt := func(payload string) string {
		return "Bearer " + enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(payload)) + ".sig"
	}
	claimOff := models.ClockOffset{Form: "text 9999-99-99a99:99:99+99:99", Claim: "issued_at"}
	if !HoldsClaimByForm(claimOff, jwt(`{"issued_at":"2026-11-26T09:35:52Z"}`), true) {
		t.Error("HoldsClaimByForm rejected UTC Z JWT claim against legacy IST +99:99 form")
	}
	if HoldsClaimByForm(claimOff, jwt(`{"issued_at":"2026-11-26 09:35:52Z"}`), true) {
		t.Error("FALSE GREEN: HoldsClaimByForm accepted space-separated timestamp against T-separated form")
	}
}
