package learn

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// signedToken is a JWT of payload, signed sig, as an application mints it.
func signedToken(payload, sig string) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(payload)) + "." + enc.EncodeToString([]byte(sig))
}

// refreshToken is the refresh token an application signs and stores: its
// auth_time a read of its clock in seconds, exp thirty days after it, sid
// and jti minted ids, and kind a claim that is no time.
func refreshToken(at time.Time, sid, jti, kind, sig string) string {
	s := strconv.FormatInt(at.Unix(), 10)
	e := strconv.FormatInt(at.Add(30*24*time.Hour).Unix(), 10)
	return signedToken(`{"userId":"68d4148edcd121253694486c","tokenType":"`+kind+`","auth_time":`+s+`,"sid":"`+sid+`","exp":`+e+`,"jti":"`+jti+`"}`, sig)
}

// A refresh token the application signs and sends (a MongoDB insert of it),
// whose auth_time and exp claims are reads of its clock: under the time
// default, where the call's adapter checks a claim at replay
// (Caps.ClaimByForm), each is a time checked by its form alone at its claim
// (models.ClockOffset.Claim), the claim's JSON type kept, and the minted
// claims and the signature are decided as before. Strict times, an adapter
// that checks no claim, and a time that is one word of a longer claim are
// refused as before; a claim that is no time never gets a check by form.
func TestDecideATimeInAClaimOfASignedTokenIsCheckedByItsFormAtTheClaim(t *testing.T) {
	rAt, fAt := oAt, oAt.Add(61*time.Second)
	rRead, fRead := rAt.Add(-300*time.Millisecond), fAt.Add(-200*time.Millisecond)
	const rSid, fSid = "a28e16aa-953f-4f5c-8328-99cf6f3cf6dc", "b38e16aa-953f-4f5c-8328-99cf6f3cf6dd"
	const rJti, fJti = "791c6d6c-6ecb-4c86-af82-935d73e5717a", "891c6d6c-6ecb-4c86-af82-935d73e5717b"
	secs := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	exp := func(t time.Time) string { return strconv.FormatInt(t.Add(30*24*time.Hour).Unix(), 10) }
	decide := func(timeForm, claims bool, rec, live string) []Decision {
		in := iCall("body.token", String, rec, live, rAt, fAt, iWin(rAt, fAt, 900*time.Millisecond).ComparesOffsets(), nil, nil)
		in.TimeForm, in.Caps.ClaimByForm = timeForm, claims
		return Decide(in)
	}
	rec := refreshToken(rRead, rSid, rJti, "refresh", "recorded-signature")
	live := refreshToken(fRead, fSid, fJti, "refresh", "this-run-signature")
	form := TimeForm(Leaf{Type: Number, Value: secs(rRead)})
	if form == "" || !strings.HasPrefix(form, "number ") {
		t.Fatalf("an epoch in seconds has form %q; want a number's", form)
	}
	ds := decide(true, true, rec, live)
	for _, tc := range []struct{ claim, r, f string }{{"auth_time", secs(rRead), secs(fRead)}, {"exp", exp(rRead), exp(fRead)}} {
		d := oWord(t, ds, tc.r, tc.f)
		if d.Outcome != OutcomeClock || d.Clock == nil || d.Clock.Claim != tc.claim || d.Clock.Form != form || !d.Clock.Word.IsZero() {
			t.Fatalf("the %s claim under the time default: %s; want a Clock checked by form %q at the claim", tc.claim, oDescribe(ds), form)
		}
		if !strings.Contains(d.Line, "the "+tc.claim+" claim of a JWT, checked by its form alone") {
			t.Errorf("the %s claim's line %q; want it to name the claim and the check by form", tc.claim, d.Line)
		}
		if !HoldsClaimByForm(*d.Clock, live, true) {
			t.Errorf("round F's token fails the %s claim's own check by form", tc.claim)
		}
	}
	for _, d := range ds {
		if d.Clock != nil && d.Clock.Claim == "" {
			t.Errorf("FALSE GREEN? %s: a Clock outside the time claims (%s->%s)", oDescribe(ds), d.Recorded, d.Full)
		}
	}
	for _, w := range [][2]string{{"791c6d6c", "891c6d6c"}, {"a28e16aa", "b38e16aa"}} {
		if d := oWord(t, ds, w[0], w[1]); d.Outcome != OutcomeFresh {
			t.Errorf("a minted claim's word %s: %s; want it decided as before (fresh)", w[0], oDescribe(ds))
		}
	}
	for _, tc := range []struct {
		what             string
		timeForm, claims bool
	}{
		{"strict times", false, true},
		{"an adapter that checks no claim at replay", true, false},
	} {
		ds := decide(tc.timeForm, tc.claims, rec, live)
		for _, r := range []string{secs(rRead), exp(rRead)} {
			d := oWord(t, ds, r, "")
			if d.Outcome == OutcomeClock || d.Clock != nil {
				t.Fatalf("FALSE GREEN? %s: %s; want the claim's time refused as before", tc.what, oDescribe(ds))
			}
			oRefused(t, tc.what, d, "cannot-check", "a part of the text keploy opened")
		}
	}
	// A time that is one word of a longer claim, or a claim that is no time.
	word := func(at time.Time, kind, sig string) string {
		return signedToken(`{"ref":"evt-`+secs(at)+`","tokenType":"`+kind+`"}`, sig)
	}
	ds = decide(true, true, word(rRead, "refresh", "s1"), word(fRead, "access", "s2"))
	for _, d := range ds {
		if d.Clock != nil || d.ByForm != nil {
			t.Fatalf("FALSE GREEN? a word of a claim, a claim that is no time: %s; want no check by form", oDescribe(ds))
		}
	}
	oRefused(t, "a time that is one word of a longer claim", oWord(t, ds, secs(rRead), secs(fRead)), "cannot-check", "a part of the text keploy opened")
	// The two runs write auth_time in two forms (seconds, then milliseconds):
	// a replay passes either, at the claim.
	ms := func(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }
	two := func(at time.Time, s string) string {
		return signedToken(`{"sub":"u-1","auth_time":`+s+`,"jti":"`+secs(at)+`x"}`, "sig-"+secs(at))
	}
	rec2, live2 := two(rRead, secs(rRead)), two(fRead, ms(fRead))
	ds = decide(true, true, rec2, live2)
	d := oWord(t, ds, secs(rRead), ms(fRead))
	msForm := TimeForm(Leaf{Type: Number, Value: ms(fRead)})
	wantF, wantFF := models.FormsOf(form, msForm)
	if d.Outcome != OutcomeClock || d.Clock == nil || d.Clock.Claim != "auth_time" || d.Clock.Form != wantF || d.Clock.FormF != wantFF {
		t.Fatalf("auth_time in seconds, then in milliseconds: %s; want a Clock at the claim checked by either form (%q, %q)", oDescribe(ds), wantF, wantFF)
	}
	if !HoldsClaimByForm(*d.Clock, rec2, true) || !HoldsClaimByForm(*d.Clock, live2, true) {
		t.Error("a token of either recorded form fails the claim's check by form")
	}
	if HoldsClaimByForm(*d.Clock, two(fRead, `"`+secs(fRead)+`"`), true) {
		t.Error("FALSE GREEN? a string claim passes a check by two number forms")
	}
}

// A replay checks a time in a claim by the claim's form in round F's token
// alone, under the time default: the claim of another unit, of another JSON
// type, missing, or in no JWT fails; strict times fail; and a keploy that
// reads only the form checks the token whole, which is no time.
func TestHoldsClaimByFormReadsTheClaimOfTheLiveToken(t *testing.T) {
	at := oAt.Add(-time.Second)
	sec := strconv.FormatInt(at.Unix(), 10)
	form := TimeForm(Leaf{Type: Number, Value: sec})
	o := models.ClockOffset{Form: form, Claim: "exp"}
	msForm := TimeForm(Leaf{Type: Number, Value: strconv.FormatInt(at.UnixMilli(), 10)})
	tok := func(exp string) string { return signedToken(`{"sub":"u-1","exp":`+exp+`}`, "sig") }
	for _, tc := range []struct {
		what     string
		o        models.ClockOffset
		live     string
		timeForm bool
		want     bool
	}{
		{"another second, in seconds", o, tok(strconv.FormatInt(at.Add(47*time.Hour).Unix(), 10)), true, true},
		{"as Bearer", o, "Bearer " + tok(sec), true, true},
		{"strict times", o, tok(sec), false, false},
		{"in milliseconds", o, tok(strconv.FormatInt(at.UnixMilli(), 10)), true, false},
		{"a string, not a number", o, tok(`"` + sec + `"`), true, false},
		{"no such claim", models.ClockOffset{Form: form, Claim: "iat"}, tok(sec), true, false},
		{"no time", o, tok(`"soon"`), true, false},
		{"no JWT", o, sec, true, false},
		{"no claim learned", models.ClockOffset{Form: form}, tok(sec), true, false},
		{"no form learned", models.ClockOffset{Claim: "exp"}, tok(sec), true, false},
		{"a word's check", models.ClockOffset{Form: form, Claim: "exp", Word: models.ClockWord{Start: 1, End: 2}}, tok(sec), true, false},
		{"either of two forms: the second", models.ClockOffset{Form: form, FormF: msForm, Claim: "exp"}, tok(strconv.FormatInt(at.UnixMilli(), 10)), true, true},
		{"either of two forms: the first", models.ClockOffset{Form: form, FormF: msForm, Claim: "exp"}, tok(sec), true, true},
		{"either of two forms: neither", models.ClockOffset{Form: form, FormF: msForm, Claim: "exp"}, tok(`"` + sec + `"`), true, false},
	} {
		if got := HoldsClaimByForm(tc.o, tc.live, tc.timeForm); got != tc.want {
			t.Errorf("%s: HoldsClaimByForm %v; want %v", tc.what, got, tc.want)
		}
	}
	if HoldsByForm(o, Leaf{Type: String, Value: tok(sec)}, true) {
		t.Error("FALSE GREEN? a keploy that reads the form alone passes the whole token as a time")
	}
	if m := o.Meet(models.ClockOffset{Form: form, Claim: "auth_time"}); !m.Empty() {
		t.Errorf("two claims' times met as %+v; want no one read (Empty)", m)
	}
	if m := o.Meet(models.ClockOffset{Form: form}); !m.Empty() {
		t.Errorf("a claim's time and a whole value's met as %+v; want no one read (Empty)", m)
	}
	if m := o.Meet(o); m.Empty() || m.Claim != "exp" {
		t.Errorf("one claim's time met with itself as %+v; want it kept", m)
	}
}
