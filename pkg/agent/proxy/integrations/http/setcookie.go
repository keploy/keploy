package http

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// RECORDED COOKIES, SERVED LATER.
//
// A recorded Set-Cookie carries an absolute Expires the server stamped at
// record time (api-server: now + 15 min). Replayed later, that instant is in
// the past. Chromium copes on its own: it moves Expires by (its clock - the
// response's Date header). A client that stores Expires as written does not.
// Playwright's request client (page.request) parses Expires as an absolute
// time and writes the cookie into the browser context already expired, which
// DELETES the session cookie the context had. Measured on
// branch-review-flow:133: the test's first gqlAsUser call (page.request) got
// the recorded `jwt_token_staging=…; Expires=18:37:17 GMT` at 19:03 GMT, and
// every request after it went out with no jwt ("not signed in").
//
// So a recorded response that sets a cookie is served as if the server
// answered now: each Expires moves by (now - recorded Date) and Date becomes
// now, keeping Expires - Date exactly as recorded. A cookie recorded as
// already expired (Expires at or before Date: a logout) is left alone, so it
// still deletes. Max-Age is relative and needs nothing.
//
// FOLDED Set-Cookie. Recording folds repeated Set-Cookie headers into one
// comma-joined value (pkg.ToYamlHTTPHeader). No client splits Set-Cookie on
// commas, so served as one line every cookie after the first is lost. Each
// cookie is written back on its own header line, as the server sent it.

// cookieStart finds a comma that begins another cookie: one followed by a
// cookie name and '='. The comma inside "Expires=Wed, 07 Oct 2026 ..." is
// followed by "07 Oct", which is not, so it stays put.
var cookieStart = regexp.MustCompile(",\\s*[!#$%&'*+\\-.^_`|~0-9A-Za-z]+=")

// expiresAttr captures a cookie's Expires value (group 2).
var expiresAttr = regexp.MustCompile(`(?i)(;\s*expires\s*=)([^;]*)`)

// cookieTimeLayouts are the Expires spellings servers send, the RFC 1123 one
// first.
var cookieTimeLayouts = []string{
	http.TimeFormat,
	time.RFC1123,
	"Mon, 02-Jan-2006 15:04:05 MST",
	time.RFC850,
	time.ANSIC,
}

// splitFoldedSetCookie splits a recorded Set-Cookie value back into cookies.
func splitFoldedSetCookie(v string) []string {
	var parts []string
	for _, line := range strings.Split(v, "\n") {
		start := 0
		for _, loc := range cookieStart.FindAllStringIndex(line, -1) {
			parts = append(parts, line[start:loc[0]])
			start = loc[0] + 1
		}
		parts = append(parts, line[start:])
	}
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseCookieTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range cookieTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// shiftCookieExpiry moves a cookie's Expires by d if the cookie was live when
// recorded (Expires after recorded).
func shiftCookieExpiry(cookie string, recorded time.Time, d time.Duration) (string, bool) {
	loc := expiresAttr.FindStringSubmatchIndex(cookie)
	if loc == nil {
		return cookie, false
	}
	exp, ok := parseCookieTime(cookie[loc[4]:loc[5]])
	if !ok || !exp.After(recorded) {
		return cookie, false
	}
	return cookie[:loc[4]] + exp.Add(d).UTC().Format(http.TimeFormat) + cookie[loc[5]:], true
}

// rebaseSetCookies rewrites header in place: one Set-Cookie value per cookie
// and, for each cookie that was live when recorded, Expires and Date moved to
// now. header must be the caller's own copy, never the pooled mock's map.
// resTs is the fallback record time when the response carries no Date. It
// returns how many cookies were shifted and by how much.
func rebaseSetCookies(header http.Header, resTs, now time.Time) (int, time.Duration) {
	cookieKey, dateKey := "", ""
	for k := range header {
		switch {
		case strings.EqualFold(k, "Set-Cookie"):
			cookieKey = k
		case strings.EqualFold(k, "Date"):
			dateKey = k
		}
	}
	if cookieKey == "" {
		return 0, 0
	}
	var cookies []string
	for _, v := range header[cookieKey] {
		cookies = append(cookies, splitFoldedSetCookie(v)...)
	}
	delete(header, cookieKey)
	header["Set-Cookie"] = cookies

	recorded := resTs
	if dateKey != "" && len(header[dateKey]) > 0 {
		if t, err := http.ParseTime(header[dateKey][0]); err == nil {
			recorded = t
		}
	}
	d := now.Sub(recorded)
	if recorded.IsZero() || d <= 0 {
		return 0, 0
	}
	shifted := 0
	for i, c := range cookies {
		if s, ok := shiftCookieExpiry(c, recorded, d); ok {
			cookies[i] = s
			shifted++
		}
	}
	if shifted > 0 && dateKey != "" {
		header[dateKey] = []string{now.UTC().Format(http.TimeFormat)}
	}
	return shifted, d
}

// COOKIES THE SERVER NEVER SET (the header presence gate).
//
// The matcher requires every header NAME a recording carries to be on the live
// request. For Cookie that ties a recording to whatever the browser happened to
// hold at record time, including cookies the page's own JavaScript wrote. A
// third-party script that loaded during the record (Facebook's pixel writes
// `_fbp`) puts a Cookie header on requests that were otherwise cookieless; at
// replay the script does not run, the request has no Cookie, and every copy is
// refused. Measured on vscode-platform:201 (github): the pre-login
// GET /auth/check and POST /auth/login copies carry `Cookie: _fbp=…` and nothing
// else, the live calls carry no Cookie, both miss with no_schema_candidates,
// and the login answers 502. The google twin recorded no `_fbp` and passes.
//
// A cookie only says something about the caller's state if replay can ever
// produce it. So when the live request has no Cookie, a recorded Cookie header
// is not required if NONE of its cookies is "known": set by a recorded
// response in the pool, or carried by any live request earlier in this replay.
// `_fbp` is neither (only the record-time script wrote it). A session cookie
// is both: a login response sets it, and every signed-in page sends it from
// the saved login state, so a recording holding it still requires a Cookie and
// a logged-out request is never answered with a logged-in recording. The live
// half matters because the pool is one spec: measured on the atg tape, 4 specs
// never SET a cookie in their own recording, yet their requests carry the
// session cookie (from storage state) — 371 recordings that must stay gated.

// noteLiveCookies remembers the cookie names a live request carries.
func (h *HTTP) noteLiveCookies(header http.Header) {
	for _, v := range header.Values("Cookie") {
		for _, n := range cookieNames(v) {
			h.liveCookies.Store(n, struct{}{})
		}
	}
}

// knownCookieNames returns the cookies replay can produce: those a recorded
// response in mocks sets plus those live requests have carried.
func (h *HTTP) knownCookieNames(mocks []*models.Mock) map[string]struct{} {
	known := issuedCookieNames(mocks)
	h.liveCookies.Range(func(k, _ any) bool {
		known[k.(string)] = struct{}{}
		return true
	})
	return known
}

// cookieNames returns the cookie names in a Cookie request header value.
func cookieNames(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ";") {
		name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// issuedCookieNames returns the name of every cookie a recorded response in
// mocks sets. Responses parked on disk (nil HTTPResp) are skipped: Set-Cookie
// responses are small and never spilled.
func issuedCookieNames(mocks []*models.Mock) map[string]struct{} {
	out := map[string]struct{}{}
	for _, m := range mocks {
		if m == nil || m.Spec.HTTPResp == nil {
			continue
		}
		for k, v := range m.Spec.HTTPResp.Header {
			if !strings.EqualFold(k, "Set-Cookie") {
				continue
			}
			for _, c := range splitFoldedSetCookie(v) {
				if name, _, ok := strings.Cut(c, "="); ok {
					out[strings.TrimSpace(name)] = struct{}{}
				}
			}
		}
	}
	return out
}

// foreignCookieHeader reports whether recorded request headers carry a Cookie
// header made up only of cookies outside known, returning its names.
func foreignCookieHeader(recorded map[string]string, issued map[string]struct{}) ([]string, bool) {
	for k, v := range recorded {
		if !strings.EqualFold(k, "Cookie") {
			continue
		}
		names := cookieNames(v)
		if len(names) == 0 {
			return nil, false
		}
		for _, n := range names {
			if _, ok := issued[n]; ok {
				return nil, false
			}
		}
		return names, true
	}
	return nil, false
}

// withoutHeader returns a copy of h without the named header.
func withoutHeader(h map[string]string, name string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if !strings.EqualFold(k, name) {
			out[k] = v
		}
	}
	return out
}
