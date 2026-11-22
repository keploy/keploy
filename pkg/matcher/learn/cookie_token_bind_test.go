package learn

import (
	"encoding/base64"
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// A Cookie header holds a token as one pair's value, and api-server's CLI
// logout sends its refresh token under two names (refresh_token and
// refresh_token_staging): the header is then no JWT a claim or signature step
// reads whole, and a value the token carries was bound at the header whole,
// which holds it only in another form; the Mongo adapter refused the
// application's find by the token ("holds this moment at header.Cookie in
// another form, and a variable holds one text"). The value is read in the
// pair instead, at its step (cookieToken).

// logoutCase is the client test's logout, its Cookie as cookie writes it.
func logoutCase(cookie string, at time.Time) *models.HTTPReq {
	return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/auth/logout", Timestamp: at,
		Header: map[string]string{"Host": "localhost:8083", "Cookie": cookie}}
}

// twoPairs is api-server's logout Cookie: the token under both names.
func twoPairs(token string) string {
	return "refresh_token=" + token + "; refresh_token_staging=" + token
}

// cookieTokens are the two runs of a refresh token: its sid and its iat made
// anew, signed with the two signatures (base64url text, the recorded one a
// single keploy word).
func cookieTokens(t *testing.T, rSid, fSid string, rAt, fAt time.Time) (string, string) {
	t.Helper()
	tok := func(sid string, at time.Time, sig string) string {
		return signedWith(t, map[string]any{"sid": sid, "tokenType": "refresh", "iat": at.Unix()}, sig)
	}
	return tok(rSid, rAt, "ssykarGU4Lp7Ktw6QuR9f8fbNsyoQDGqhZGjDJhJUEs"), tok(fSid, fAt, "2-Eg58Sc-AWt1hpQzYRxPg-61F0f2C3G-dO0dsUnz4k")
}

// signedWith is claimToken with sig as the signature's own base64url text.
func signedWith(t *testing.T, claims map[string]any, sig string) string {
	t.Helper()
	return claimToken(t, claims, "") + sig
}

// TestAValueAClientTestSentInACookiesTokenIsBoundAtItsPair: the sid the
// application finds by is a Value bound at the case's Cookie through its
// first pair (header.Cookie|cookie.refresh_token|jwt.sid#0), and the token's
// signature at its jwtsig there, though its recorded text is one word of the
// header; each reads the two runs' values back. A call's request whose
// Cookie holds the token is no place to read a claim at: its value is named
// as before, and only its signature is read in the pair.
func TestAValueAClientTestSentInACookiesTokenIsBoundAtItsPair(t *testing.T) {
	const rSid, fSid = "ac28bb2f", "c946d4c2"
	rReq := time.Date(2026, 11, 12, 1, 23, 13, 146_000_000, time.UTC)
	fReq := rReq.Add(67 * time.Second)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	rTok, fTok := cookieTokens(t, rSid, fSid, rReq, fReq)
	rSig, _ := variables.Decode(variables.StepJWTSig, rTok)
	fSig, _ := variables.Decode(variables.StepJWTSig, fTok)
	for _, tc := range []struct {
		name, path, rec, live, want string
		client                      bool
	}{
		{"the sid, a client test's", "body.cmd.filter.session_id", rSid, fSid, "header.Cookie|cookie.refresh_token|jwt.sid#0", true},
		{"the signature, a client test's", "body.cmd.filter.sig", rSig, fSig, "header.Cookie|cookie.refresh_token|jwtsig", true},
		{"the signature, a call's", "body.cmd.filter.sig", rSig, fSig, "header.Cookie|cookie.refresh_token|jwtsig", false},
		{"the sid, a call's", "body.cmd.filter.session_id", rSid, fSid, "header.Cookie", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			earlier := []Earlier{iSent("mock-0", rReq.Add(-time.Millisecond), "header.Cookie", String, twoPairs(rTok), twoPairs(fTok))}
			if tc.client {
				earlier = clientPlaced(t, "post-auth-logout-3", logoutCase(twoPairs(rTok), rReq), logoutCase(twoPairs(fTok), fReq), fReq)
			}
			in := iCall(tc.path, String, tc.rec, tc.live, rCall, fCall, nil, earlier, nil)
			if tc.client {
				in.Drifts = oClientDrifts()
			}
			d := oWord(t, Decide(in), tc.rec, tc.live)
			if d.Outcome != OutcomeValue || d.From.Client != tc.client || d.From.Path != "header.Cookie" {
				t.Fatalf("%s: outcome %v from %s %s (client %v); want a Value of the Cookie", tc.name, d.Outcome, d.From.Mock, d.From.Path, d.From.Client)
			}
			if got := d.FromAt.String(); got != tc.want {
				t.Fatalf("%s: bound at %q; want %q", tc.name, got, tc.want)
			}
			if tc.want == "header.Cookie" {
				return
			}
			if v, ok := d.FromAt.WordOf(twoPairs(rTok)); !ok || v != tc.rec {
				t.Fatalf("%s: the place reads %q (%v) in the recorded Cookie; want %q", tc.name, v, ok, tc.rec)
			}
			if v, ok := d.FromAt.WordOf(twoPairs(fTok)); !ok || v != tc.live {
				t.Fatalf("%s: the place reads %q (%v) in round F's Cookie; want %q", tc.name, v, ok, tc.live)
			}
		})
	}
}

// TestCookieTokenReadsTheTokenOneOfACookiesPairsHolds: cookieToken names the
// first pair, in the recorded order, whose token holds the two runs' values:
// at its signature (claims false) or at its claim's word (claims true);
// none where the place is no Cookie or Set-Cookie header's own text, a pair
// is named twice, the pairs stand in another order in round F, no token
// holds them so, or the other kind of part does.
func TestCookieTokenReadsTheTokenOneOfACookiesPairsHolds(t *testing.T) {
	enc := base64.RawURLEncoding
	tok := func(sid, sig string) string {
		return enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + enc.EncodeToString([]byte(`{"sid":"`+sid+`","n":1}`)) + "." + sig
	}
	const rSig, fSig = "ssykarGU4Lp7Ktw6QuR9f8fbNsyoQDGqhZGjDJhJUEs", "2-Eg58Sc-AWt1hpQzYRxPg-61F0f2C3G-dO0dsUnz4k"
	r, f := tok("u-a1", rSig), tok("u-b2", fSig)
	cookie := variables.Address{Place: variables.Place{Part: variables.PartHeader, Name: "Cookie"}, Word: -1}
	setCookie := variables.Address{Place: variables.Place{Part: variables.PartHeader, Name: "Set-Cookie"}, Word: -1}
	auth := variables.Address{Place: variables.Place{Part: variables.PartHeader, Name: "Authorization"}, Word: -1}
	pair := variables.Cookie("refresh_token")
	for _, tc := range []struct {
		name      string
		at        variables.Address
		rec, live string
		v, fv     string
		claims    bool
		steps     []variables.Step
		word      int
		ok        bool
	}{
		{"the signature, the first pair", cookie, twoPairs(r), twoPairs(f), rSig, fSig, false, []variables.Step{pair, variables.StepJWTSig}, -1, true},
		{"a claim's word, the first pair", cookie, twoPairs(r), twoPairs(f), "a1", "b2", true, []variables.Step{pair, variables.JWTClaim("sid")}, 1, true},
		{"a later pair", cookie, "theme=dark; refresh_token=" + r, "theme=light; refresh_token=" + f, "a1", "b2", true, []variables.Step{pair, variables.JWTClaim("sid")}, 1, true},
		{"a Set-Cookie header", setCookie, "refresh_token=" + r + "; Path=/", "refresh_token=" + f + "; Path=/", rSig, fSig, false, []variables.Step{pair, variables.StepJWTSig}, -1, true},
		{"no cookie header", auth, twoPairs(r), twoPairs(f), rSig, fSig, false, nil, 0, false},
		{"a cookie header read through a step", variables.Address{Place: cookie.Place, Steps: []variables.Step{variables.StepPct}, Word: -1}, twoPairs(r), twoPairs(f), rSig, fSig, false, nil, 0, false},
		{"a pair named twice", cookie, "refresh_token=" + r + "; refresh_token=" + r, "refresh_token=" + f + "; refresh_token=" + f, rSig, fSig, false, nil, 0, false},
		{"pairs in another order", cookie, twoPairs(r), "refresh_token_staging=" + f + "; refresh_token=" + f, rSig, fSig, false, nil, 0, false},
		{"round F's token holds another signature", cookie, twoPairs(r), twoPairs(f), rSig, rSig[1:] + "x", false, nil, 0, false},
		{"round F's claim holds another word", cookie, twoPairs(r), twoPairs(f), "a1", "b3", true, nil, 0, false},
		{"a signature asked as a claim", cookie, twoPairs(r), twoPairs(f), rSig, fSig, true, nil, 0, false},
		{"a claim asked as a signature", cookie, twoPairs(r), twoPairs(f), "a1", "b2", false, nil, 0, false},
		{"a pair that holds no token", cookie, "refresh_token=u-a1", "refresh_token=u-b2", "a1", "b2", true, nil, 0, false},
	} {
		steps, n, ok := cookieToken(tc.at, tc.rec, tc.live, tc.v, tc.fv, tc.claims)
		if ok != tc.ok || n != tc.word || !slices.Equal(steps, tc.steps) {
			t.Errorf("%s: cookieToken = %q, %d, %v; want %q, %d, %v", tc.name, steps, n, ok, tc.steps, tc.word, tc.ok)
		}
	}
}

// TestClaimAtReadsAClaimInACookiesPair: Classify's place of a value a client
// test's Cookie holds in a pair's token (Place.Claim) is the Cookie at the
// pair's claim's word; a token the header is whole keeps its own claim step
// (claimWord first); a call's request names none.
func TestClaimAtReadsAClaimInACookiesPair(t *testing.T) {
	rTok := claimToken(t, map[string]any{"sid": "u-a1"}, "recorded")
	fTok := claimToken(t, map[string]any{"sid": "u-b2"}, "this-run")
	for _, tc := range []struct {
		name, at, rec, live, want string
		client                    bool
	}{
		{"two pairs", "header.Cookie", twoPairs(rTok), twoPairs(fTok), "header.Cookie|cookie.refresh_token|jwt.sid#1", true},
		{"one pair, read as the header's token", "header.Cookie", "refresh_token=" + rTok, "refresh_token=" + fTok, "header.Cookie|jwt.sid#1", true},
		{"two pairs, a call's request", "header.Cookie", twoPairs(rTok), twoPairs(fTok), "", false},
		{"two pairs, no Cookie header", "header.X-Tokens", twoPairs(rTok), twoPairs(fTok), "", true},
	} {
		if got := claimAt(Place{Var: tc.at, Client: tc.client}, tc.rec, tc.live, "a1", "b2"); got != tc.want {
			t.Errorf("%s: claimAt = %q; want %q", tc.name, got, tc.want)
		}
	}
}
