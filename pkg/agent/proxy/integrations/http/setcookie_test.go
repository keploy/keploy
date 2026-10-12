package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The shape recorded on the atg lane: api-server refreshes the session cookie
// with Expires = Date + 15 min.
const (
	recDate = "Wed, 07 Oct 2026 18:22:17 GMT"
	jwtLine = "jwt_token_staging=eyJ.a.b; Path=/; Expires=Wed, 07 Oct 2026 18:37:17 GMT; HttpOnly; Secure; SameSite=None"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := http.ParseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// branch-review-flow:133: served at 19:03 GMT, the recorded Expires (18:37)
// is already past, and a client that stores it as written deletes the
// session. Served now, the cookie must still have its recorded 15 minutes.
func TestRebaseSetCookiesKeepsTheRecordedLifetime(t *testing.T) {
	now := mustTime(t, "Wed, 07 Oct 2026 19:03:05 GMT")
	h := http.Header{"Date": {recDate}, "Set-Cookie": {jwtLine}}

	n, by := rebaseSetCookies(h, time.Time{}, now)
	if n != 1 || by != now.Sub(mustTime(t, recDate)) {
		t.Fatalf("shifted %d by %v", n, by)
	}
	if got := h.Get("Date"); got != "Wed, 07 Oct 2026 19:03:05 GMT" {
		t.Fatalf("Date = %q, want serve time", got)
	}
	want := "jwt_token_staging=eyJ.a.b; Path=/; Expires=Wed, 07 Oct 2026 19:18:05 GMT; HttpOnly; Secure; SameSite=None"
	if got := h["Set-Cookie"]; len(got) != 1 || got[0] != want {
		t.Fatalf("Set-Cookie = %q\nwant         %q", got, want)
	}
}

// Recording folds repeated Set-Cookie headers into one comma-joined value.
// Each cookie must go back on its own line, and the comma inside an Expires
// date must not split a cookie.
func TestRebaseSetCookiesUnfoldsRepeatedHeaders(t *testing.T) {
	now := mustTime(t, recDate) // no time has passed: only the unfolding is under test
	refresh := "refresh_token_staging=r.t; Path=/; Expires=Fri, 06 Nov 2026 18:22:17 GMT; HttpOnly"
	h := http.Header{"Date": {recDate}, "Set-Cookie": {jwtLine + "," + refresh}}

	rebaseSetCookies(h, time.Time{}, now)
	if got := h["Set-Cookie"]; len(got) != 2 || got[0] != jwtLine || got[1] != refresh {
		t.Fatalf("Set-Cookie = %q", got)
	}
}

func TestRebaseSetCookiesLeavesOthersAlone(t *testing.T) {
	now := mustTime(t, "Wed, 07 Oct 2026 19:03:05 GMT")

	t.Run("a deletion cookie still deletes", func(t *testing.T) {
		del := "jwt_token_staging=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0"
		h := http.Header{"Date": {recDate}, "Set-Cookie": {del}}
		if n, _ := rebaseSetCookies(h, time.Time{}, now); n != 0 || h["Set-Cookie"][0] != del || h.Get("Date") != recDate {
			t.Fatalf("a logout cookie must be served as recorded: %v", h)
		}
	})
	t.Run("Max-Age and session cookies need nothing", func(t *testing.T) {
		h := http.Header{"Date": {recDate}, "Set-Cookie": {"a=1; Max-Age=900", "b=2; Path=/"}}
		if n, _ := rebaseSetCookies(h, time.Time{}, now); n != 0 || h.Get("Date") != recDate {
			t.Fatalf("nothing to shift: %v", h)
		}
	})
	t.Run("no Set-Cookie leaves Date as recorded", func(t *testing.T) {
		h := http.Header{"Date": {recDate}}
		if n, _ := rebaseSetCookies(h, time.Time{}, now); n != 0 || h.Get("Date") != recDate {
			t.Fatalf("got %v", h)
		}
	})
	t.Run("no Date falls back to the recorded response time", func(t *testing.T) {
		h := http.Header{"Set-Cookie": {jwtLine}}
		n, _ := rebaseSetCookies(h, mustTime(t, recDate), now)
		if n != 1 || !strings.Contains(h["Set-Cookie"][0], "Expires=Wed, 07 Oct 2026 19:18:05 GMT") {
			t.Fatalf("got %v", h)
		}
		if _, ok := h["Date"]; ok {
			t.Fatal("a Date the server never sent must not be invented")
		}
	})
	t.Run("an unknown record time changes nothing", func(t *testing.T) {
		h := http.Header{"Set-Cookie": {jwtLine}}
		if n, _ := rebaseSetCookies(h, time.Time{}, now); n != 0 || h["Set-Cookie"][0] != jwtLine {
			t.Fatalf("got %v", h)
		}
	})
}

func TestSplitFoldedSetCookie(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"a=1", []string{"a=1"}},
		{"a=1; Expires=Wed, 07 Oct 2026 18:37:17 GMT, b=2", []string{"a=1; Expires=Wed, 07 Oct 2026 18:37:17 GMT", "b=2"}},
		{"a=1; Expires=Wed, 07-Oct-2026 18:37:17 GMT; Path=/,b=2", []string{"a=1; Expires=Wed, 07-Oct-2026 18:37:17 GMT; Path=/", "b=2"}},
		{"a=1\nb=2", []string{"a=1", "b=2"}},
	} {
		got := splitFoldedSetCookie(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%q: got %q want %q", c.in, got, c.want)
		}
	}
}

// End to end through the serializer: the bytes on the wire carry one
// Set-Cookie line per cookie, with Expires moved, and the pooled mock is not
// touched.
func TestBuildMockResponseBytesServesCookiesAtServeTime(t *testing.T) {
	refresh := "refresh_token_staging=r.t; Path=/; Expires=Fri, 06 Nov 2026 18:22:17 GMT; HttpOnly"
	folded := jwtLine + "," + refresh
	m := &models.Mock{
		Name: "auth-check",
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{Method: "GET", URL: "/auth/check", ProtoMajor: 1, ProtoMinor: 1},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Date": recDate, "Set-Cookie": folded, "Content-Length": "2"},
				Body:       "{}",
			},
		},
	}
	h := &HTTP{Logger: zap.NewNop()}
	out, err := h.buildMockResponseBytes(m)
	if err != nil {
		t.Fatal(err)
	}
	var cookies []string
	for _, line := range strings.Split(string(out), "\r\n") {
		if v, ok := strings.CutPrefix(line, "Set-Cookie: "); ok {
			cookies = append(cookies, v)
		}
	}
	if len(cookies) != 2 {
		t.Fatalf("want one Set-Cookie line per cookie, got %q", cookies)
	}
	for _, c := range cookies {
		if strings.Contains(c, "Expires=Wed, 07 Oct 2026 18:37:17 GMT") {
			t.Fatalf("the recorded, already-past Expires was served as is: %q", c)
		}
	}
	if m.Spec.HTTPResp.Header["Set-Cookie"] != folded || m.Spec.HTTPResp.Header["Date"] != recDate {
		t.Fatal("serving must never rewrite the pooled mock")
	}
}

func cookieAuthMock(name, method, path, cookie string, setCookie string) *models.Mock {
	h := map[string]string{"Host": "localhost:8083", "Origin": "http://localhost:3000"}
	if cookie != "" {
		h["Cookie"] = cookie
	}
	resp := &models.HTTPResp{StatusCode: 200, Header: map[string]string{}}
	if setCookie != "" {
		resp.Header["Set-Cookie"] = setCookie
	}
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			HTTPReq:  &models.HTTPReq{Method: models.Method(method), URL: "http://localhost:8083" + path, Header: h},
			HTTPResp: resp,
		},
	}
}

func liveNoCookie(method, path string) *req {
	u, _ := url.Parse("http://localhost:8083" + path)
	return &req{method: method, url: u, header: http.Header{"Host": {"localhost:8083"}, "Origin": {"http://localhost:3000"}}}
}

// vscode-platform:201 (github): the pre-login calls were recorded carrying only
// the Facebook pixel's `_fbp` cookie. At replay the pixel does not run and the
// calls carry no Cookie; the recording must still be usable.
func TestSchemaMatchIgnoresACookieTheServerNeverSet(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	login := cookieAuthMock("login", "POST", "/auth/login", "_fbp=fb.0.1.2", "jwt_token_staging=j; Path=/; HttpOnly")
	check := cookieAuthMock("check-pre-login", "GET", "/auth/check", "_fbp=fb.0.1.2", "")
	pool := []*models.Mock{login, check}

	got, err := h.SchemaMatch(context.Background(), liveNoCookie("POST", "/auth/login"), pool, nil, nil, false)
	if err != nil || len(got) != 1 || got[0] != login {
		t.Fatalf("got %v, %v: a recording whose only cookie came from a third-party script must match a cookieless call", names(got), err)
	}
	got, _ = h.SchemaMatch(context.Background(), liveNoCookie("GET", "/auth/check"), pool, nil, nil, false)
	if len(got) != 1 || got[0] != check {
		t.Fatalf("got %v", names(got))
	}
}

// A cookie the server DID hand out says the caller was signed in. A cookieless
// call must never be answered with that recording.
func TestSchemaMatchStillRequiresACookieTheServerSet(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	login := cookieAuthMock("login", "POST", "/auth/login", "", "jwt_token_staging=j; Path=/; HttpOnly, refresh_token_staging=r; Path=/")
	signedIn := cookieAuthMock("check-signed-in", "GET", "/auth/check", "_fbp=fb.0.1.2; jwt_token_staging=j", "")
	got, _ := h.SchemaMatch(context.Background(), liveNoCookie("GET", "/auth/check"), []*models.Mock{login, signedIn}, nil, nil, false)
	if len(got) != 0 {
		t.Fatalf("got %v: a signed-in recording must not answer a cookieless call", names(got))
	}
}

func TestCookieNames(t *testing.T) {
	if got := strings.Join(cookieNames(" _fbp=fb.1; jwt_token_staging=a=b ;x"), ","); got != "_fbp,jwt_token_staging,x" {
		t.Fatalf("got %q", got)
	}
}

// A cookie that live requests carry (the session cookie, from the saved login
// state) stays required even in a pool that never sets it.
func TestSchemaMatchRequiresACookieLiveRequestsCarry(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}
	signedIn := cookieAuthMock("check-signed-in", "GET", "/auth/check", "jwt_token_staging=j", "")
	pool := []*models.Mock{signedIn}
	if got, _ := h.SchemaMatch(context.Background(), liveNoCookie("GET", "/auth/check"), pool, nil, nil, false); len(got) != 1 {
		t.Fatalf("before any live request carried it, the cookie is unknown: got %v", names(got))
	}
	withCookie := liveNoCookie("GET", "/auth/check")
	withCookie.header.Set("Cookie", "jwt_token_staging=live")
	h.SchemaMatch(context.Background(), withCookie, pool, nil, nil, false)
	if got, _ := h.SchemaMatch(context.Background(), liveNoCookie("GET", "/auth/check"), pool, nil, nil, false); len(got) != 0 {
		t.Fatalf("got %v: once a live request carried the session cookie, a signed-in recording must not answer a cookieless call", names(got))
	}
}
