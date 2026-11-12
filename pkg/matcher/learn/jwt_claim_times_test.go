package learn

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// claimsToken is a JWT of an HS256 header, the claims given encoded by enc,
// and the signature sig.
func claimsToken(enc *base64.Encoding, claims, sig string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(claims)) + "." + sig
}

const (
	claimSigA = "c2lnbmF0dXJlLWJ5dGVzLXJlY29yZGVkLTAwMDAwMDA"
	claimSigB = "c2lnbmF0dXJlLWJ5dGVzLW9mLXRoaXMtcnVuLTExMTE"
)

// A claim's recorded value written back is the token as it was, byte for
// byte, in either base64url encoding of its payload, whatever the claim's
// JSON is; a payload that holds the claim twice, or none, has no place for
// it.
func TestAClaimWrittenBackIsTheTokenAsItWas(t *testing.T) {
	for _, c := range []struct {
		claims string
		claim  string
		ok     bool
	}{
		{claims: `{"sub":"a","iat":1762900000}`, claim: "iat", ok: true},
		{claims: `{ "sub" : "a" , "iat" : 1762900000 , "ctx" : {"iat": 1} }`, claim: "iat", ok: true},
		{claims: `{"ctx":{"iat":1},"iat":"2026-11-12T01:00:00Z"}`, claim: "iat", ok: true},
		{claims: `{"iat":1,"iat":2}`, claim: "iat"},
		{claims: `{"sub":"a"}`, claim: "iat"},
		{claims: `["iat",1]`, claim: "iat"},
		{claims: `{"iat":1}{}`, claim: "iat"},
	} {
		for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
			tok := claimsToken(enc, c.claims, claimSigA)
			raw, ok := claimRawOf(tok, c.claim)
			if ok != c.ok {
				t.Fatalf("%s: claimRawOf ok %v, want %v", c.claims, ok, c.ok)
			}
			if !ok {
				if _, ok := claimPutBack(tok, c.claim, "1"); ok {
					t.Fatalf("%s: wrote a claim back where the payload holds no one place for it", c.claims)
				}
				continue
			}
			back, ok := claimPutBack(tok, c.claim, raw)
			if !ok || back != tok {
				t.Fatalf("%s: written back %q (%v), want the token as it was", c.claims, back, ok)
			}
		}
	}
}

// A recorded token's times learned by form at their claims hold a replay's
// token whose times moved, under the time default alone; every other part
// of it is held as recorded, its signature by its form.
func TestATokensTimeClaimsHoldByFormAndTheRestAsRecorded(t *testing.T) {
	at := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	claims := func(sub string, at time.Time) string {
		return `{"sub":"` + sub + `","iat":` + strconv.FormatInt(at.Unix(), 10) + `}`
	}
	rec := "Bearer " + claimsToken(base64.RawURLEncoding, claims("a", at), claimSigA)
	form := TimeForm(Leaf{Type: Number, Value: strconv.FormatInt(at.Unix(), 10)})
	offs := []models.ClockOffset{{Form: form, Claim: "iat"}}
	moved := "Bearer " + claimsToken(base64.RawURLEncoding, claims("a", at.Add(time.Hour)), claimSigB)
	later := strconv.FormatInt(at.Add(time.Hour).Unix(), 10)
	for _, c := range []struct {
		what     string
		offs     []models.ClockOffset
		rec      string // the recorded token, where not rec
		live     string
		timeForm bool
		held     bool
		why      string // a refusal's reason holds it
	}{
		{what: "times moved", offs: offs, live: moved, timeForm: true, held: true},
		{what: "times moved, strict", offs: offs, live: moved, why: "mock.times: strict"},
		{what: "no time learned", live: moved, timeForm: true, why: "no time was learned"},
		{what: "another subject", offs: offs, live: "Bearer " + claimsToken(base64.RawURLEncoding, claims("b", at.Add(time.Hour)), claimSigB), timeForm: true, why: "its header or another claim changed"},
		{what: "another scheme", offs: offs, live: "Basic " + claimsToken(base64.RawURLEncoding, claims("a", at.Add(time.Hour)), claimSigB), timeForm: true, why: "its header or another claim changed"},
		{what: "another signature length", offs: offs, live: moved[:len(moved)-1], timeForm: true, why: "signature is not of the recorded one's form"},
		{what: "a time of another form", offs: offs, live: "Bearer " + claimsToken(base64.RawURLEncoding, `{"sub":"a","iat":"1762909200"}`, claimSigB), timeForm: true, why: "iat claim was learned as a time"},
		{what: "a time in milliseconds", offs: offs, live: "Bearer " + claimsToken(base64.RawURLEncoding, `{"sub":"a","iat":1762909200000}`, claimSigB), timeForm: true, why: "iat claim was learned as a time"},
		{what: "the claim gone", offs: offs, live: "Bearer " + claimsToken(base64.RawURLEncoding, `{"sub":"a"}`, claimSigB), timeForm: true, why: "iat claim was learned as a time"},
		{what: "a padded payload", offs: offs, live: "Bearer " + claimsToken(base64.URLEncoding, claims("a", at.Add(time.Hour)), claimSigB), timeForm: true, why: "its header or another claim changed"},
		{what: "no time learned, the signature alone moved", live: "Bearer " + claimsToken(base64.RawURLEncoding, claims("a", at), claimSigB), timeForm: true, why: "no time was learned"},
		{what: "a recorded token of no such claim", offs: offs, rec: "Bearer " + claimsToken(base64.RawURLEncoding, `{"sub":"a"}`, claimSigA), live: moved, timeForm: true, why: "the recorded JWT holds no one iat claim"},
		{what: "the claim twice in this call's token", offs: offs, live: "Bearer " + claimsToken(base64.RawURLEncoding, `{"sub":"a","iat":`+later+`,"iat":`+later+`}`, claimSigB), timeForm: true, why: "this call's JWT holds no one iat claim"},
	} {
		r := rec
		if c.rec != "" {
			r = c.rec
		}
		held, why := HoldsClaimsByForm(c.offs, r, c.live, c.timeForm)
		if held != c.held || held == (why != "") || !strings.Contains(why, c.why) {
			t.Errorf("%s: held %v (%q), want %v (%q)", c.what, held, why, c.held, c.why)
		}
	}
}

// A clock key names a claim only where its offset names that claim, of a
// leaf's path: a key of the claim of no name, with an offset of no claim,
// is a word's read, not a claim's.
func TestAClaimClockKeyNamesItsClaim(t *testing.T) {
	k := ClaimClockKey("body.token", "iat")
	for _, c := range []struct {
		key   string
		o     models.ClockOffset
		field string
	}{
		{key: k, o: models.ClockOffset{Claim: "iat"}, field: "body.token"},
		{key: k, o: models.ClockOffset{}},
		{key: ClaimClockKey("body.token", ""), o: models.ClockOffset{}},
		{key: k, o: models.ClockOffset{Claim: "exp"}},
		{key: "body.token", o: models.ClockOffset{Claim: "iat"}},
		{key: "|jwt.iat", o: models.ClockOffset{Claim: "iat"}},
		{key: "body.a|cookie.s|jwt.iat", o: models.ClockOffset{Claim: "iat"}},
	} {
		field, ok := ClaimClockField(c.key, c.o)
		if ok != (c.field != "") || field != c.field && ok {
			t.Errorf("%s %+v: field %q ok %v, want %q", c.key, c.o, field, ok, c.field)
		}
	}
}

// Only a text leaf learned as noise, or refused as an instant its call's
// window shows no read of the clock in, at a header or a body field, under
// the time default, is learned as the times in its token's claims.
func TestOnlyNoiseOrAnUnreadInstantIsLearnedAsAToken(t *testing.T) {
	recAt := time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
	liveAt := recAt.Add(10 * time.Minute)
	w := &Window{Rec: Span{From: recAt.Add(-time.Second), To: recAt.Add(time.Second)}, F: Span{From: liveAt.Add(-time.Second), To: liveAt.Add(time.Second)}}
	tok := func(at time.Time, sig string) string {
		return claimsToken(base64.RawURLEncoding, `{"sub":"a","iat":`+strconv.FormatInt(at.Unix(), 10)+`}`, sig)
	}
	leaf := func(path string, at time.Time, sig string) Leaf {
		return Leaf{Path: path, Type: String, Value: tok(at, sig)}
	}
	unread := "body.token changed, and no clock read by the app matched it"
	num := func(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }
	text := func(at time.Time, zone string) string { return `"` + at.Format("2006-01-02T15:04:05") + zone + `"` }
	// other is noise at body.token: a token whose claim c is rec as
	// recorded and live in round F, beside an unchanged sub.
	other := func(c, rec, live string) Learned {
		tk := func(v, sig string) string {
			return claimsToken(base64.RawURLEncoding, `{"sub":"a","`+c+`":`+v+`}`, sig)
		}
		return Learned{Kind: KindNoise, Recorded: Leaf{Path: "body.token", Type: String, Value: tk(rec, claimSigA)}, Live: Leaf{Path: "body.token", Type: String, Value: tk(live, claimSigB)}}
	}
	for _, c := range []struct {
		what              string
		ln                Learned
		timeForm          bool
		learned           bool
		claim             string // learned at this claim, not iat
		noWindow, noStart bool
	}{
		{what: "noise at a body field", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true, learned: true},
		{what: "noise at a header", ln: Learned{Kind: KindNoise, Recorded: leaf("header.x-token", recAt, claimSigA), Live: leaf("header.x-token", liveAt, claimSigB)}, timeForm: true, learned: true},
		{what: "noise, strict", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}},
		{what: "noise at a body field whose name holds a '|'", ln: Learned{Kind: KindNoise, Recorded: leaf("body.a|b", recAt, claimSigA), Live: leaf("body.a|b", liveAt, claimSigB)}, timeForm: true},
		{what: "a token in a leaf typed a number as recorded", ln: Learned{Kind: KindNoise, Recorded: Leaf{Path: "body.token", Type: Number, Value: tok(recAt, claimSigA)}, Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true},
		{what: "a token in a leaf typed a number in round F", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt, claimSigA), Live: Leaf{Path: "body.token", Type: Number, Value: tok(liveAt, claimSigB)}}, timeForm: true},
		{what: "noise at a query value", ln: Learned{Kind: KindNoise, Recorded: leaf("url.query.t", recAt, claimSigA), Live: leaf("url.query.t", liveAt, claimSigB)}, timeForm: true},
		{what: "an unread instant", ln: Learned{Kind: KindUnaccounted, Why: unread, unreadWhy: unread, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true, learned: true},
		{what: "an unread instant refused again for another reason", ln: Learned{Kind: KindUnaccounted, Why: "another", unreadWhy: unread, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true},
		{what: "an unread instant the offset rule refused", ln: Learned{Kind: KindUnaccounted, Why: unread, unreadWhy: unread, byOffset: true, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true},
		{what: "an unread instant of which source", ln: Learned{Kind: KindUnaccounted, Why: unread, unreadWhy: unread, sourceWhy: unread, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true},
		{what: "a variable", ln: Learned{Kind: KindVariable, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true},
		{what: "a signature of another length", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB+"AAAA")}, timeForm: true},
		{what: "a time far from its call", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt.AddDate(2, 0, 0), claimSigB)}, timeForm: true},
		{what: "no window", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true},
		{what: "a recorded time far from its call", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt.AddDate(-2, 0, 0), claimSigA), Live: leaf("body.token", liveAt, claimSigB)}, timeForm: true},
		{what: "only the signature changed", ln: Learned{Kind: KindNoise, Recorded: leaf("body.token", recAt, claimSigA), Live: leaf("body.token", recAt, claimSigB)}, timeForm: true},
		{what: "a claim of a name no key names alone", ln: other("i|at", num(recAt), num(liveAt)), timeForm: true},
		{what: "another claim read as the clock", ln: other("issued", num(recAt), num(liveAt)), timeForm: true, claim: "issued"},
		{what: "another claim, its record run's no read", ln: other("issued", num(recAt.Add(-time.Hour)), num(liveAt)), timeForm: true},
		{what: "another claim, round F's no read", ln: other("issued", num(recAt), num(liveAt.Add(-time.Hour))), timeForm: true},
		{what: "another claim, a moment after its call", ln: other("issued", num(recAt), num(liveAt.Add(time.Hour))), timeForm: true},
		{what: "another claim as text read as the clock", ln: other("issued", text(recAt, "Z"), text(liveAt, "Z")), timeForm: true, claim: "issued"},
		{what: "another claim as text of no zone", ln: other("issued", text(recAt, ""), text(liveAt, "")), timeForm: true},
		{what: "another claim as text, no window", ln: other("issued", text(recAt, "Z"), text(liveAt, "Z")), timeForm: true, noWindow: true},
		{what: "another claim as text, a span of no start", ln: other("issued", text(recAt, "Z"), text(liveAt, "Z")), timeForm: true, noStart: true},
		{what: "a NumericDate as text, no read of the clock", ln: other("iat", text(recAt.Add(-48*time.Hour), "Z"), text(liveAt.Add(-48*time.Hour), "Z")), timeForm: true},
	} {
		win := w
		switch {
		case c.what == "no window" || c.noWindow:
			win = nil
		case c.noStart:
			win = &Window{Rec: Span{To: w.Rec.To}, F: Span{To: w.F.To}}
		}
		claim := c.claim
		if claim != "" {
			c.learned = true
		} else {
			claim = "iat"
		}
		got, ok := ClaimTimesLearned(c.ln, win, c.timeForm)
		if ok != c.learned {
			t.Errorf("%s: learned %v (%+v), want %v", c.what, ok, got, c.learned)
			continue
		}
		if !ok {
			continue
		}
		if len(got) != 1 || got[0].Kind != KindClockRead || got[0].Recorded.Path != ClaimClockKey(c.ln.Recorded.Path, claim) {
			t.Errorf("%s: learned %+v, want the %s claim's read at its key", c.what, got, claim)
			continue
		}
		if o, ok := got[0].Clock(); !ok || o.Claim != claim || o.Form == "" {
			t.Errorf("%s: learned %+v, want a time in the iat claim checked by its form", c.what, o)
		}
	}
}
