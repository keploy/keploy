package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// cliHandoff is api-server's CLI sign-in handoff as its e2e client drives
// it, signed in at the second signed: four parks of a code under the browser
// session (POST /auth/temp-code, the session token in the Cookie, its
// auth_time and iat the second it was signed), then the logout of the CLI
// session a redemption minted, the refresh token it got back sent by logout,
// each at its time from at.
func cliHandoff(signed int64, logout *models.HTTPReq, at time.Time) []*models.HTTPReq {
	park := func(i int) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/auth/temp-code", Timestamp: at.Add(time.Duration(i) * time.Millisecond),
			Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083", "Origin": "http://localhost:3000",
				"Cookie": "jwt_token_staging=" + signInJWT(signed)},
			Body: `{"code":"E2EHANDOFF` + nanos(at.UnixNano()+int64(i)) + `"}`}
	}
	logout.Timestamp = at.Add(7 * time.Millisecond)
	return []*models.HTTPReq{park(0), park(1), park(2), park(3), logout}
}

// cliRefresh is the refresh token the application mints at a redemption:
// the parking session's auth_time, an expiry 30 days from the second it
// minted it, and a session id and a token id made anew.
func cliRefresh(t *testing.T, signed, minted int64, sid, jti, sig string) string {
	t.Helper()
	return signedWith(t, map[string]any{"auth_time": signed, "exp": minted + 30*24*3600, "jti": jti, "sid": sid,
		"tokenType": "refresh", "userId": "68d4148edcd121253694486c"}, sig)
}

// cliLogoutRun is the two runs of the CLI handoff, signed in 69 s apart, each
// with the refresh token its redemption minted (rTok, fTok).
type cliLogoutRun struct {
	rAt, fAt         time.Time
	rSigned, fSigned int64
	rTok, fTok       string
}

func newCLILogoutRun(t *testing.T) cliLogoutRun {
	r := cliLogoutRun{rAt: time.Date(2026, 11, 12, 21, 4, 22, 672_310_000, time.UTC)}
	r.fAt = r.rAt.Add(69*time.Second + 3*time.Millisecond)
	r.rSigned, r.fSigned = r.rAt.Unix(), r.fAt.Unix()
	r.rTok = cliRefresh(t, r.rSigned, r.rSigned, "7e5df739-1e49-45d9-8d68-451416375331", "5663a564-ecf4-4b3a-9752-d4f35d2347f9", "ssykarGU4Lp7Ktw6QuR9f8fbNsyoQDGqhZGjDJhJUEs")
	r.fTok = cliRefresh(t, r.fSigned, r.fSigned, "c946d4c2-0b1e-4a7a-9d55-2f6b8e1c3a90", "0f2b7c1e-6a4d-4e8b-b3f1-9c2d5e7a8b61", "2-Eg58Sc-AWt1hpQzYRxPg-61F0f2C3G-dO0dsUnz4k")
	return r
}

// find is the decisions on the application's find by the refresh token
// (refreshToken's filter.token) after the handoff whose logout is recLogout
// as recorded and liveLogout in round F.
func (r cliLogoutRun) find(t *testing.T, recLogout, liveLogout *models.HTTPReq) []Decision {
	t.Helper()
	names := []string{"post-auth-temp-code-1", "post-auth-temp-code-2", "post-auth-temp-code-3", "post-auth-temp-code-4", "post-auth-logout-1"}
	earlier := clientSent(t, names, cliHandoff(r.rSigned, recLogout, r.rAt), cliHandoff(r.fSigned, liveLogout, r.fAt))
	in := iCall("body.cmd.filter.token", String, r.rTok, r.fTok, r.rAt.Add(8*time.Millisecond), r.fAt.Add(8*time.Millisecond), nil, earlier, nil)
	in.Drifts = oClientDrifts()
	return Decide(in)
}

// authTimeOf is the decision on the token's auth_time claim, the second the
// browser signed in at in each run.
func (r cliLogoutRun) authTimeOf(t *testing.T, ds []Decision) Decision {
	t.Helper()
	return oWord(t, ds, nanos(r.rSigned), nanos(r.fSigned))
}

// TestATokenAClientTestSentWholeInACookiesPairIsBoundThere: the application
// finds the CLI session by the refresh token the logout's Cookie carries
// (refreshToken's find, filter.token), under both its names. The token's
// auth_time is the browser session's, which the four parks before it sent in
// their session token: the moment stands in two texts, and as two separate
// reads it was refused. The logout's pair holds the field's whole text in
// both runs: the call copied the token from there, so each of its changed
// claims is bound at that pair, the auth_time at its own claim, and the
// parks, which hold only the moment, do not compete; the line says so.
func TestATokenAClientTestSentWholeInACookiesPairIsBoundThere(t *testing.T) {
	r := newCLILogoutRun(t)
	ds := r.find(t, logoutCase(twoPairs(r.rTok), time.Time{}), logoutCase(twoPairs(r.fTok), time.Time{}))
	if len(ds) == 0 {
		t.Fatal("no decision on the refresh token")
	}
	for _, d := range ds {
		if d.Outcome != OutcomeValue || d.From.Mock != "test case post-auth-logout-1" || d.From.Path != "header.Cookie" || !d.From.Client ||
			!strings.HasPrefix(d.FromAt.String(), "header.Cookie|cookie.refresh_token|") {
			t.Fatalf("the refresh token's claims: %s; want each a Value of test case post-auth-logout-1's Cookie, at its refresh_token pair", oDescribe(ds))
		}
	}
	d := r.authTimeOf(t, ds)
	if got := d.FromAt.String(); got != "header.Cookie|cookie.refresh_token|jwt.auth_time#0" {
		t.Fatalf("the auth_time bound at %q; want header.Cookie|cookie.refresh_token|jwt.auth_time#0", got)
	}
	if d.Clock != nil || !strings.Contains(d.Line, "test case post-auth-temp-code-1 header.Cookie") || !strings.Contains(d.Line, "held only its word") {
		t.Fatalf("the auth_time's line %q (clock %v); want no clock, the line naming the parks as holding only its word", d.Line, d.Clock)
	}
}

// TestATokenAPairHoldsInOneRunOnlyIsNoWholeSource: a pair is the field's
// whole source only where it holds the field's text in both runs, and only
// in a Cookie header, where a replay reads a pair by its name. Where one
// run's logout sent another token, or the token stands in "name=value" pairs
// of a header that is no Cookie, the parks and the logout each hold the
// moment, and the auth_time stays refused as two separate reads.
func TestATokenAPairHoldsInOneRunOnlyIsNoWholeSource(t *testing.T) {
	r := newCLILogoutRun(t)
	other := cliRefresh(t, r.fSigned, r.fSigned, "1d2c3b4a-0b1e-4a7a-9d55-2f6b8e1c3a90", "9e8d7c6b-6a4d-4e8b-b3f1-9c2d5e7a8b61", "Qm9vdGgtc2lnbmF0dXJlLW9mLWFub3RoZXItdG9rZW4")
	rOther := cliRefresh(t, r.rSigned, r.rSigned, "2e3d4c5b-1e49-45d9-8d68-451416375331", "8f7e6d5c-ecf4-4b3a-9752-d4f35d2347f9", "QW5vdGhlci1yZWNvcmRlZC10b2tlbnMtc2lnbmF0dXJl")
	notCookie := func(tok string) *models.HTTPReq {
		return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/auth/logout",
			Header: map[string]string{"Host": "localhost:8083", "X-Session": twoPairs(tok)}}
	}
	for _, tc := range []struct {
		name      string
		rec, live *models.HTTPReq
	}{
		{"round F's logout sent another token", logoutCase(twoPairs(r.rTok), time.Time{}), logoutCase(twoPairs(other), time.Time{})},
		{"the recorded logout sent another token", logoutCase(twoPairs(rOther), time.Time{}), logoutCase(twoPairs(r.fTok), time.Time{})},
		{"the pairs stand in a header that is no Cookie", notCookie(r.rTok), notCookie(r.fTok)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := r.authTimeOf(t, r.find(t, tc.rec, tc.live))
			if d.Refusal == nil || d.Refusal.Kind != RefusalTwoSources {
				t.Fatalf("the auth_time: %s; want refused, two sources", oDescribe([]Decision{d}))
			}
		})
	}
}
