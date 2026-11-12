package learn

import (
	"encoding/base64"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// signInJWT is a session token as api-server's e2e client signs it: its
// auth_time and iat are the second it was signed, and it expires an hour
// later. The signature is not read.
func signInJWT(authTime int64) string {
	enc := base64.RawURLEncoding
	at := strconv.FormatInt(authTime, 10)
	head := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(`{"authMethod":"oauth","auth_time":` + at + `,"cid":"keploy.io","exp":` + strconv.FormatInt(authTime+3600, 10) + `,"iat":` + at + `,"sid":"e2e-go-session"}`))
	return head + "." + payload + ".c2lnbmF0dXJl"
}

// TestASignInTimeTheTestRequestsTokenCarriesIsItsVariable: a login code's
// authTime is the sign-in time of the session that parks it, which the
// application reads from the session token the request carries (a cookie
// named for the server's environment, or a bearer token), never from its
// clock. Where the test case's request is given, the token's auth_time claim
// is the place of the run that holds it in both runs: a variable of that
// request's header, bound there, never a read of the call's clock, whose
// replay would fail by how long before the call the client signed in.
func TestASignInTimeTheTestRequestsTokenCarriesIsItsVariable(t *testing.T) {
	at := time.Date(2026, 11, 9, 7, 1, 42, 498_000_000, time.UTC)
	rec, live := at.Unix(), at.Add(69*time.Second).Unix()
	const p = "body.docs.msgs.0.authTime.$numberLong"
	shape := func(n int64) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: strconv.FormatInt(n, 10)}}}
	}
	for _, tc := range []struct{ header, prefix string }{
		{"header.Cookie", "jwt_token_staging="},
		{"header.Authorization", "Bearer "},
	} {
		t.Run(tc.header, func(t *testing.T) {
			req := &models.TestRequest{Test: "post-auth-temp-code-4", Parts: []models.TestRequestPart{
				jsonType,
				{Name: tc.header, Recorded: tc.prefix + signInJWT(rec), Sent: tc.prefix + signInJWT(live)},
				{Name: "body", Recorded: `{"code":"E2EHANDOFF1"}`, Sent: `{"code":"E2EHANDOFF1"}`},
			}}
			ln := ClassifyIn(clockedDrifts(), []string{p}, shape(live), shape(rec), at.Add(69*time.Second), []Earlier{GivenRequest(req)}, nil, nil, nil)[0]
			if ln.Kind != KindVariable || !ln.From.Given || ln.From.Mock != "test case post-auth-temp-code-4" || ln.From.Path != tc.header || ln.From.Whole {
				t.Fatalf("authTime %d -> %d, the token's auth_time in the request's %s: %s from %+v (%s); want a variable of that header, a part of it", rec, live, tc.header, ln.Kind, ln.From, ln.Why)
			}
			if ln.Offset != nil || ln.Read != nil {
				t.Errorf("authTime learned with a clock offset %v or read %v; want none: the value is the token's, not the call's clock", ln.Offset, ln.Read)
			}
		})
	}
}
