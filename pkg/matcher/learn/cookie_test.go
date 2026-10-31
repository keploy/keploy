package learn

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// cookieToken is the harness's session JWT (harnessToken) without a scheme,
// as a cookie's value holds it.
func cookieToken(authTime, jti string) string {
	return strings.TrimPrefix(harnessToken(authTime, jti), "Bearer ")
}

// A Cookie header's cookies that hold a JWT are read apart, each at its own
// place ("header.<name>{<cookie>}"), its value replaced byte for byte in the
// header; the rest of the header (every other cookie, the names, their
// order) is what stays compared whole. A cookie two pairs name has no one
// place, and a header keploy cannot read as pairs holds none; a header that
// is no Cookie header is read whole, as before.
func TestACookieHeadersTokensAreReadApart(t *testing.T) {
	tok := cookieToken("1793373215", "j-1")
	v := "theme=dark;  jwt_token_staging= " + tok + " ; lang=en"
	held := HeaderJWTs("header.Cookie", v)
	if len(held) != 1 || held[0].Place != "header.Cookie{jwt_token_staging}" || held[0].Cookie != "jwt_token_staging" || held[0].JWT.Scheme != "" {
		t.Fatalf("held %+v", held)
	}
	if got, ok := CookieValue(v, "jwt_token_staging"); !ok || got != tok {
		t.Errorf("value %q %v", got, ok)
	}
	if got, ok := WithCookieValue(v, "jwt_token_staging", "x.y.z"); !ok || got != "theme=dark;  jwt_token_staging= x.y.z ; lang=en" {
		t.Errorf("with %q %v", got, ok)
	}
	if got, ok := CookiesBesideJWTs(v); !ok || got != "theme=dark;  jwt_token_staging= {jwt} ; lang=en" {
		t.Errorf("beside %q %v", got, ok)
	}
	if TokenHeader(held[0].Place) != "header.Cookie" || TokenHeader("header.authorization") != "header.authorization" {
		t.Errorf("token header %q", TokenHeader(held[0].Place))
	}
	for name, c := range map[string]struct{ place, v string }{
		"a cookie two pairs name":   {"header.cookie", "s=" + tok + "; s=" + tok},
		"a part that is no pair":    {"header.cookie", "theme; s=" + tok},
		"a token after a scheme":    {"header.cookie", "s=Bearer " + tok},
		"no Cookie header":          {"header.x-pref", "s=" + tok},
		"no token":                  {"header.cookie", "s=kep_opaque; lang=en"},
		"a header's name and value": {"header.cookie", ""},
	} {
		if h := HeaderJWTs(c.place, c.v); len(h) != 0 {
			t.Errorf("%s: read %+v", name, h)
		}
		if _, ok := CookiesBesideJWTs(c.v); ok && c.place == "header.cookie" {
			t.Errorf("%s: a rest beside no token", name)
		}
	}
	if _, ok := CookieValue("s=1; s=2", "s"); ok {
		t.Error("a cookie two pairs name read as one value")
	}
	if h := HeaderJWTs("header.Authorization", "Bearer "+tok); len(h) != 1 || h[0].Place != "header.Authorization" || h[0].Cookie != "" {
		t.Errorf("a header's whole token: %+v", h)
	}
}

// A claim of a cookie's token has a variable place
// ("header.Cookie{<cookie>}{jwt}.<claim>") that reads back to its leaf, and
// the cookie's name ends at its brace: a cookie named jwt is one too, and
// "header.Cookie{jwt}.<claim>" is a claim of the header's whole value.
func TestACookiesClaimHasAPlace(t *testing.T) {
	const leaf = "header.cookie{jwt_token_staging}{jwt}.auth_time"
	place, ok := VarPlace(Leaf{Path: leaf}, false)
	if !ok || place != "header.cookie{jwt_token_staging}{jwt}.auth_time" {
		t.Fatalf("place %q %v", place, ok)
	}
	p, err := variables.Parse("header.Cookie{jwt_token_staging}{jwt}.auth_time")
	if err != nil || p.Name != "Cookie" || p.Cookie != "jwt_token_staging" || len(p.Claim) != 1 || p.String() != "header.Cookie{jwt_token_staging}{jwt}.auth_time" {
		t.Fatalf("parsed %+v %v", p, err)
	}
	if back, ok := LeafPath("header.Cookie{jwt_token_staging}{jwt}.auth_time"); !ok || back != leaf {
		t.Errorf("leaf %q %v", back, ok)
	}
	if p, err := variables.Parse("header.Cookie{jwt}{jwt}.sub"); err != nil || p.Cookie != "jwt" || p.Name != "Cookie" {
		t.Errorf("a cookie named jwt: %+v %v", p, err)
	}
	if p, err := variables.Parse("header.Cookie{jwt}.sub"); err != nil || p.Cookie != "" || p.Name != "Cookie" {
		t.Errorf("the header's whole token: %+v %v", p, err)
	}
	if place, ok := VarPlace(Leaf{Path: "header.cookie{jwt}{jwt}.sub"}, false); !ok || place != "header.cookie{jwt}{jwt}.sub" {
		t.Errorf("a cookie named jwt: %q %v", place, ok)
	}
	if _, err := variables.Parse("header.Cookie{a=b}{jwt}.sub"); err == nil {
		t.Error("a cookie's name holding '=' parsed")
	}
}

// A client test's harness that sends its session JWT in a cookie gives the
// application each claim of it: a value a call carries whole that the
// cookie's token holds (the harness's auth_time, copied into a stored
// document) is a variable of the incoming requests' claim, read again in
// this run's requests. A claim two requests hold two values of (a jti per
// token) is no place, and a value in no claim (another cookie's word) is
// not followed from the token.
func TestAClientTestsCookieTokenIsItsGivenPlace(t *testing.T) {
	t0 := time.Date(2026, 10, 31, 1, 2, 0, 0, time.UTC)
	req := func(name string, at time.Time, authTime, jti string) IncomingRequest {
		return IncomingRequest{Name: name, At: at, Parts: []models.TestRequestPart{
			{Name: "url", Recorded: "http://localhost:8080/v1/login/handoff"},
			{Name: "header.Cookie", Recorded: "theme=dark; jwt_token_staging=" + cookieToken(authTime, jti)},
		}}
	}
	recorded := []IncomingRequest{req("test-3", t0.Add(time.Second), "1793373215", "j-3"), req("test-4", t0.Add(2*time.Second), "1793373215", "j-4")}
	live := []IncomingRequest{req("", t0, "1793387031", "j-a"), req("", t0, "1793387031", "j-b")}
	call := t0.Add(5 * time.Second)
	e := IncomingGiven("TestCLILogin", recorded, live, true, call)
	const claim = "header.Cookie{jwt_token_staging}{jwt}.auth_time"
	if e.Unread != "" || !hasSeen(e, claim) || !hasSeen(e, "header.Cookie{jwt_token_staging}{jwt}.sid") || hasSeen(e, "header.Cookie{jwt_token_staging}{jwt}.jti") {
		t.Fatalf("place %+v", e)
	}
	if v, why := IncomingValueAt(live, claim); v != "1793387031" || why != "" {
		t.Errorf("this run's value %q (%s)", v, why)
	}
	const p = "body.docs.msgs[0].authTime"
	shape := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: Number, Value: v}}}
	}
	ln := ClassifyIn(clientTestCache(), []string{p}, shape("1793387031"), shape("1793373215"), call, []Earlier{e}, nil, nil, nil)[0]
	if ln.Kind != KindVariable || !ln.From.Given || !ln.From.Incoming || ln.From.Path != claim || !ln.From.Whole || ln.From.Mock != "incoming request test-3 of test TestCLILogin" {
		t.Errorf("the harness's auth_time copied into a document: %s from %+v (%s); want a variable of the cookie's claim", ln.Kind, ln.From, ln.Why)
	}
	// The same claim in the test case's request (keploy test): read so too.
	g := GivenRequest(&models.TestRequest{Test: "test-1", Parts: []models.TestRequestPart{
		{Name: "header.Cookie", Recorded: "jwt_token_staging=" + cookieToken("1793373215", "j-1"), Sent: "jwt_token_staging=" + cookieToken("1793387031", "j-2")},
	}})
	var got *Seen
	for i := range g.Seen {
		if g.Seen[i].Leaf.Path == claim {
			got = &g.Seen[i]
		}
	}
	if got == nil || got.Leaf.Value != "1793373215" || got.Live != "1793387031" {
		t.Errorf("the test case's cookie claim: %+v in %+v", got, g.Seen)
	}
}
