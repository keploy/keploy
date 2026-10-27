package templates

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// Rule 3: a capture takes the app's answer at its place when it is present,
// of the capture's JSON type, and not empty. Nothing else is asserted there.
func TestExpected_ACaptureTakesAPresentNonEmptyValueOfItsType(t *testing.T) {
	cases := []struct {
		name, exp, answer string
		want              string // the key's value after; "" = not captured
	}{
		{"string", `{"id":"{{capture string .id}}"}`, `{"id":"usr_LIVE"}`, "usr_LIVE"},
		{"int", `{"n":"{{capture int .id}}"}`, `{"n":7654321}`, "7654321"},
		{"float", `{"p":"{{capture float .id}}"}`, `{"p":12.50}`, "12.50"},
		{"inside a string", `{"href":"/users/{{capture string .id}}?v=1"}`, `{"href":"/users/usr_9?v=1"}`, "usr_9"},
		{"array element by index", `{"ids":["a","{{capture string .id}}"]}`, `{"ids":["a","b2"]}`, "b2"},
		{"string wants a string", `{"id":"{{capture string .id}}"}`, `{"id":42}`, ""},
		{"int wants a number", `{"n":"{{capture int .id}}"}`, `{"n":"42"}`, ""},
		{"int wants an integer", `{"n":"{{capture int .id}}"}`, `{"n":4.2}`, ""},
		{"null is not a value", `{"id":"{{capture string .id}}"}`, `{"id":null}`, ""},
		{"empty is not a value", `{"id":"{{capture string .id}}"}`, `{"id":""}`, ""},
		{"missing is not a value", `{"id":"{{capture string .id}}"}`, `{"other":"x"}`, ""},
		{"text around it must be there", `{"href":"/users/{{capture string .id}}"}`, `{"href":"/orders/9"}`, ""},
		{"an answer that is not JSON", `{"id":"{{capture string .id}}"}`, `oops`, ""},
		// encoding/json rejects these, and so does the template layer.
		{"an answer with a raw TAB in a string", `{"id":"{{capture string .id}}"}`, "{\"id\":\"usr\tLIVE\"}", ""},
		{"an answer with a bad escape", `{"id":"{{capture string .id}}"}`, `{"id":"usr\xLIVE"}`, ""},
		{"an answer with a placeholder where a value goes", `{"id":"{{capture string .id}}"}`, `{"id":{{string .x}}}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"id": "REC"})
			res := r.Expected(models.HTTPResp{Body: c.exp}, models.HTTPResp{Body: c.answer}, ExpectOptions{})
			if c.want == "" {
				assert.Equal(t, "REC", r.Values()["id"], "a capture took a value its rule does not allow")
				require.Len(t, res.Failures, 1)
				assert.Contains(t, res.Resp.Body, "{{capture", "a missed capture stays as written, so the comparison fails there")
				return
			}
			assert.Equal(t, c.want, r.Values()["id"])
			assert.Empty(t, res.Failures)
			require.Len(t, res.Captures, 1)
			assert.Equal(t, c.answer, res.Resp.Body, "a captured place holds what the app answered")
		})
	}
}

// Rule 4: the response's captures apply first, then its other placeholders
// take the values as they are now. {"id":V,"stored_id":V}: stored_id is
// asserted equal to the id this very answer gave.
func TestExpected_CapturesApplyBeforeTheResponsesOtherPlaceholders(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_REC"})
	exp := models.HTTPResp{
		Header: map[string]string{"Location": "/users/{{string .id}}"},
		Body:   `{"id":"{{capture string .id}}","stored_id":"{{string .id}}"}`,
	}
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Location": "/users/usr_LIVE"}, Body: `{"id":"usr_LIVE","stored_id":"usr_LIVE"}`}, ExpectOptions{})
	assert.Equal(t, `{"id":"usr_LIVE","stored_id":"usr_LIVE"}`, res.Resp.Body)
	assert.Equal(t, "/users/usr_LIVE", res.Resp.Header["Location"])
	assert.Equal(t, `{"id":"usr_REC","stored_id":"usr_REC"}`, res.RecordedBody, "the recorded body puts every key at its config.yaml value")
	assert.True(t, res.BodyTemplated)
	assert.Equal(t, `{"id":"{{capture string .id}}","stored_id":"{{string .id}}"}`, exp.Body, "exp is not changed")
}

// DEAD HEADER LEARNING (the bug: the copy learned from shared its Header map
// with the case rendered in place, so no header placeholder was left to learn
// from). A capture in a header takes the app's value, and the next request
// sends it.
func TestExpected_AHeaderCaptureReachesTheNextRequest(t *testing.T) {
	cases := []struct {
		name, header, expected, live string
		next                         models.HTTPReq
		sent                         func(models.HTTPReq) string
		want                         string
	}{
		{
			name: "Set-Cookie session issued at login", header: "Set-Cookie",
			expected: "sid={{capture string .sid}}; Path=/; HttpOnly", live: "sid=S1live0001; Path=/; HttpOnly",
			next: models.HTTPReq{Header: map[string]string{"Cookie": "theme=dark; sid={{string .sid}}"}},
			sent: func(q models.HTTPReq) string { return q.Header["Cookie"] }, want: "theme=dark; sid=S1live0001",
		},
		{
			name: "Location of a created resource", header: "Location",
			expected: "/users/{{capture string .sid}}", live: "/users/usr_LIVE01",
			next: models.HTTPReq{URL: "http://h/users/{{string .sid}}"},
			sent: func(q models.HTTPReq) string { return q.URL }, want: "http://h/users/usr_LIVE01",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"sid": "S0rec00001"})
			// The answer's header name in another case: header names are
			// case-insensitive.
			res := r.Expected(models.HTTPResp{Header: map[string]string{c.header: c.expected}},
				models.HTTPResp{Header: map[string]string{"x-ignored": "1", lower(c.header): c.live}}, ExpectOptions{})
			require.Empty(t, res.Failures)
			assert.Equal(t, c.live, res.Resp.Header[c.header])
			assert.Equal(t, c.want, c.sent(r.Request(c.next)))
		})
	}
}

func lower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// Rule 1: a value written out is compared as written. A literal that happens
// to equal a key's value is not a capture: it stays, and the key keeps its
// value, whatever the app answers there.
func TestExpected_ALiteralEqualToAKeysValueStaysLiteral(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_01J9REC", "one": "1", "qty": 42})
	exp := models.HTTPResp{
		Header: map[string]string{"X-User": "usr_01J9REC"},
		Body:   `{"id":"usr_01J9REC","count":1,"qty":"42","ids":["usr_01J9REC"]}`,
	}
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"X-User": "usr_OTHER01"}, Body: `{"id":"usr_OTHER01","count":2,"qty":"43","ids":["usr_OTHER01"]}`}, ExpectOptions{})
	assert.Equal(t, exp.Body, res.Resp.Body)
	assert.Equal(t, "usr_01J9REC", res.Resp.Header["X-User"])
	assert.Equal(t, map[string]string{"id": "usr_01J9REC", "one": "1", "qty": "42"}, r.Values())
	assert.Empty(t, res.Captures)
}

// A use placeholder in an expected response asserts the value the key has now.
func TestExpected_AUseIsTheKeysValueNow(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_REC", "n": "1"})
	r.Expected(models.HTTPResp{Body: `{"id":"{{capture string .id}}","n":"{{capture int .n}}"}`}, models.HTTPResp{Body: `{"id":"usr_LIVE","n":7654321}`}, ExpectOptions{})
	res := r.Expected(models.HTTPResp{Body: `{"id":"{{string .id}}","n":"{{int .n}}","nested":{"of":"{{string .id}}"}}`}, models.HTTPResp{Body: `{}`}, ExpectOptions{})
	assert.Equal(t, `{"id":"usr_LIVE","n":7654321,"nested":{"of":"usr_LIVE"}}`, res.Resp.Body)
	assert.Equal(t, `{"id":"usr_REC","n":1,"nested":{"of":"usr_REC"}}`, res.RecordedBody)
}

func TestExpected_ACaptureOfAnUndefinedKeyStaysAsWritten(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "x"})
	res := r.Expected(models.HTTPResp{Body: `{"id":"{{capture string .idd}}"}`}, models.HTTPResp{Body: `{"id":"y"}`}, ExpectOptions{})
	assert.Equal(t, `{"id":"{{capture string .idd}}"}`, res.Resp.Body)
	assert.Equal(t, map[string]string{"id": "x"}, r.Values())
}

func TestExpected_NoPlaceholderOrNoTemplatesIsTheExpectedAsWritten(t *testing.T) {
	exp := models.HTTPResp{StatusCode: 200, Header: map[string]string{"A": "{{.x}}"}, Body: `{"tpl":"{{ .Name }}","n":1.0}`}
	for _, r := range []*Run{newRun(t, nil), newRun(t, map[string]interface{}{"id": "1"})} {
		res := r.Expected(exp, models.HTTPResp{Body: `{}`}, ExpectOptions{})
		assert.Equal(t, exp, res.Resp)
		assert.Equal(t, exp.Body, res.RecordedBody)
		assert.Empty(t, res.Captures)
	}
}

func TestExpectedNoAnswer_RendersUsesAndLeavesCaptures(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_REC"})
	got := r.ExpectedNoAnswer(models.HTTPResp{
		Header: map[string]string{"Location": "/u/{{string .id}}"},
		Body:   `{"id":"{{capture string .id}}","of":"{{string .id}}"}`,
	})
	assert.Equal(t, `{"id":"{{capture string .id}}","of":"usr_REC"}`, got.Body)
	assert.Equal(t, "/u/usr_REC", got.Header["Location"])
}

// SET-COOKIE PER COOKIE. keploy joins an answer's Set-Cookie lines into one
// value. A capture is found by its cookie's identity (name, Domain, Path)
// and takes the value up to ';'. In a cookie with a placeholder, Expires is
// asserted as a lifetime ((Expires − Date) as recorded, within 5 s) and then
// takes the answer's text; Max-Age and every other attribute are compared as
// recorded. A cookie the answer deletes (Max-Age=0, an Expires before its
// Date) fails its capture: a deleted cookie is not set.
func TestExpected_SetCookieCapturesCookieByCookie(t *testing.T) {
	const recDate, ansDate = "Mon, 05 Oct 2026 10:00:00 GMT", "Tue, 06 Oct 2026 11:11:11 GMT"
	exp := models.HTTPResp{Header: map[string]string{
		"Date":       recDate,
		"Set-Cookie": "csrftoken={{capture string .csrf}}; Path=/; Expires=Fri, 23 Oct 2026 10:00:00 GMT,sessionid={{capture string .sid}}; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
	}}
	keep := copyStringMap(exp.Header)
	const fresh = "csrftoken=C1live; Path=/; Expires=Sat, 24 Oct 2026 11:11:11 GMT"
	cases := []struct {
		name, live string
		want       map[string]string // the keys after
		fails      int
		resolved   string // the header to compare; "" = the answer as it is
	}{
		{
			name: "both cookies, a fresh Expires with the recorded lifetime",
			live: fresh + ",sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
			want: map[string]string{"csrf": "C1live", "sid": "S1live"},
		},
		{
			name: "cookies in another order are found by identity",
			live: "sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly," + fresh + ",theme=dark; Path=/",
			want: map[string]string{"csrf": "C1live", "sid": "S1live"},
			// Compared as recorded, in the recorded order: the comparison
			// then fails on the order.
			resolved: fresh + ",sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
		},
		{
			name: "a Max-Age that changed is compared as recorded",
			live: fresh + ",sessionid=S1live; Max-Age=1209599; Path=/; HttpOnly,theme=dark; Path=/",
			want: map[string]string{"csrf": "C1live", "sid": "S1live"},
			// The comparison fails on it: Max-Age is a setting, not a date.
			resolved: fresh + ",sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
		},
		{
			name:     "an Expires with another lifetime is compared as recorded",
			live:     "csrftoken=C1live; Path=/; Expires=Tue, 06 Oct 2026 12:11:11 GMT,sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
			want:     map[string]string{"csrf": "C1live", "sid": "S1live"},
			resolved: "csrftoken=C1live; Path=/; Expires=Fri, 23 Oct 2026 10:00:00 GMT,sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
		},
		{
			name:  "a cookie of another Path is another cookie",
			live:  "csrftoken=C1live; Path=/admin; Expires=Sat, 24 Oct 2026 11:11:11 GMT,sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
			want:  map[string]string{"csrf": "C0rec", "sid": "S1live"},
			fails: 1,
		},
		{
			name:  "a cookie the answer does not set fails its capture",
			live:  fresh + ",theme=dark; Path=/",
			want:  map[string]string{"csrf": "C1live", "sid": "S0rec"},
			fails: 1,
		},
		{
			name:  "an empty cookie value is no value",
			live:  "csrftoken=; Path=/; Expires=Sat, 24 Oct 2026 11:11:11 GMT,sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
			want:  map[string]string{"csrf": "C0rec", "sid": "S1live"},
			fails: 1,
		},
		{
			name:  "two double quotes are an empty value too",
			live:  fresh + `,sessionid=""; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/`,
			want:  map[string]string{"csrf": "C1live", "sid": "S0rec"},
			fails: 1,
		},
		{
			name:  "Max-Age=0 deletes the cookie: no capture",
			live:  fresh + ",sessionid=S1live; Max-Age=0; Path=/; HttpOnly,theme=dark; Path=/",
			want:  map[string]string{"csrf": "C1live", "sid": "S0rec"},
			fails: 1,
		},
		{
			name:  "an Expires in 1970 deletes the cookie: no capture",
			live:  "csrftoken=C1live; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT,sessionid=S1live; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/",
			want:  map[string]string{"csrf": "C0rec", "sid": "S1live"},
			fails: 1,
		},
		{
			name:  "Laravel's deleted cookie (value \"deleted\", Max-Age=0): no capture",
			live:  fresh + ",sessionid=deleted; expires=Thu, 01-Jan-1970 00:00:01 GMT; Max-Age=0; Path=/; httponly,theme=dark; Path=/",
			want:  map[string]string{"csrf": "C1live", "sid": "S0rec"},
			fails: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"csrf": "C0rec", "sid": "S0rec"})
			res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"set-cookie": c.live, "date": ansDate}}, ExpectOptions{})
			assert.Equal(t, c.want, r.Values())
			assert.Len(t, res.Failures, c.fails, "%v", res.Failures)
			if c.fails == 0 {
				want := c.live
				if c.resolved != "" {
					want = c.resolved
				}
				assert.Equal(t, want, res.Resp.Header["Set-Cookie"])
			}
			assert.Equal(t, keep, exp.Header, "exp's header map is not written")
		})
	}
	r := newRun(t, map[string]interface{}{"csrf": "C0rec", "sid": "S0rec"})
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Date": ansDate, "Set-Cookie": "csrftoken=C1; Path=/; Expires=x,sessionid=S1; Max-Age=1209600; Path=/; HttpOnly,theme=dark; Path=/"}}, ExpectOptions{})
	require.Len(t, res.Captures, 2)
	assert.Equal(t, Capture{Key: "csrf", Value: "C1", Where: "header Set-Cookie.csrftoken", Type: TypeString}, res.Captures[0])
	assert.Equal(t, Capture{Key: "sid", Value: "S1", Where: "header Set-Cookie.sessionid", Type: TypeString}, res.Captures[1])
	assert.Contains(t, res.Resp.Header["Set-Cookie"], "Expires=Fri, 23 Oct 2026 10:00:00 GMT", "an Expires that does not read is compared as recorded")
}

// An answer without a Date header: its Expires must read and, where the
// recorded cookie was alive, not be before now.
func TestExpected_ACookieExpiresWithoutADateIsReadAgainstNow(t *testing.T) {
	exp := models.HTTPResp{Header: map[string]string{
		"Date":       "Mon, 05 Oct 2026 10:00:00 GMT",
		"Set-Cookie": "sid={{capture string .sid}}; Path=/; Expires=Mon, 05 Oct 2026 11:00:00 GMT",
	}}
	now := func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC) }
	for _, c := range []struct {
		name, expires string
		takes         bool // the answer's date stands
		fails         int
	}{
		{"a date after now", "Thu, 08 Oct 2026 13:00:00 GMT", true, 0},
		{"a date before now: the cookie is gone", "Thu, 08 Oct 2026 11:00:00 GMT", false, 1},
		{"a date that does not read", "soon", false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"sid": "S0"})
			res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=S1; Path=/; Expires=" + c.expires}}, ExpectOptions{Now: now})
			assert.Len(t, res.Failures, c.fails, "%v", res.Failures)
			assert.Equal(t, c.takes, strings.Contains(res.Resp.Header["Set-Cookie"], c.expires), res.Resp.Header["Set-Cookie"])
		})
	}
}

// A cookie the recording itself deleted (a logout clearing the session it was
// sent) is not asserted to be set: only its value is.
func TestExpected_ARecordedDeletionIsNotAsserted(t *testing.T) {
	exp := models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{string .sid}}; Path=/; Max-Age=0"}}
	r := newRun(t, map[string]interface{}{"sid": "S1"})
	assert.Empty(t, r.Expected(exp, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=S1; Path=/; Max-Age=0"}}, ExpectOptions{}).Failures)
	require.Len(t, r.Expected(exp, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=S2; Path=/; Max-Age=0"}}, ExpectOptions{}).Failures, 1)
	live := models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{string .sid}}; Path=/; Max-Age=60"}}
	fs := r.Expected(live, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=S1; Path=/; Max-Age=0"}}, ExpectOptions{}).Failures
	require.Len(t, fs, 1, "a cookie recorded alive that the answer deletes fails its use")
	assert.Contains(t, fs[0].Why, "deletes it (Max-Age=0)")
}

// RFC 6265 §5.3: of two cookies with one name, Domain and Path in one answer,
// the later replaces the earlier. A clear-then-set login (Express's
// res.clearCookie then res.cookie) is captured from the cookie that stays.
func TestExpected_AClearThenSetLoginCapturesTheCookieThatStays(t *testing.T) {
	exp := models.HTTPResp{Header: map[string]string{
		"Set-Cookie": "sid=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT,sid={{capture string .sid}}; Path=/; HttpOnly",
	}}
	r := newRun(t, map[string]interface{}{"sid": "S0rec"})
	live := "sid=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT,sid=S1live; Path=/; HttpOnly"
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Set-Cookie": live}}, ExpectOptions{})
	assert.Empty(t, res.Failures)
	assert.Equal(t, "S1live", r.Values()["sid"])
	assert.Equal(t, live, res.Resp.Header["Set-Cookie"])
}

// Cookies of one name and two Paths are two cookies: each capture takes its
// own, in whatever order the answer sets them.
func TestExpected_TwoPathsOfOneNameCaptureTheirOwnValues(t *testing.T) {
	exp := models.HTTPResp{Header: map[string]string{
		"Set-Cookie": "sid={{capture string .sid}}; Path=/,sid={{capture string .sidAdmin}}; Path=/admin",
	}}
	for _, live := range []string{"sid=ROOTnew; Path=/,sid=ADMINnew; Path=/admin", "sid=ADMINnew; Path=/admin,sid=ROOTnew; Path=/"} {
		r := newRun(t, map[string]interface{}{"sid": "R0", "sidAdmin": "A0"})
		res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Set-Cookie": live}}, ExpectOptions{})
		assert.Empty(t, res.Failures)
		assert.Equal(t, map[string]string{"sid": "ROOTnew", "sidAdmin": "ADMINnew"}, r.Values(), live)
		require.Len(t, res.Captures, 2)
		assert.Equal(t, "header Set-Cookie.sid (Path=/admin)", res.Captures[1].Where)
	}
}

// A HEADER CAPTURE THAT FINDS NOTHING FAILS: no such header, or a header
// whose text around the capture is not there (X-Request-Id, which auto
// header noise forgives in the comparison).
func TestExpected_AHeaderCaptureThatFindsNothingFails(t *testing.T) {
	exp := models.HTTPResp{Header: map[string]string{"X-Request-Id": "req-{{capture string .rid}}"}}
	for _, c := range []struct {
		name   string
		answer map[string]string
		why    string
	}{
		{"no such header", map[string]string{"Other": "x"}, "the answer has no such header"},
		{"the text around it is not there", map[string]string{"X-Request-Id": "abc123"}, "around it"},
		{"nothing between the text around it", map[string]string{"X-Request-Id": "req-"}, "needs a non-empty string"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"rid": "R0"})
			res := r.Expected(exp, models.HTTPResp{Header: c.answer}, ExpectOptions{})
			require.Len(t, res.Failures, 1)
			assert.Equal(t, "header X-Request-Id", res.Failures[0].Where)
			assert.Contains(t, res.Failures[0].Why, c.why)
			assert.Equal(t, "R0", r.Values()["rid"])
		})
	}
}

// A USE AMONG OTHER TEXT IS ANCHORED: the answer must read as the text does,
// its literal neighbours as written and the use's span its value. Containing
// the value somewhere is not holding it at its place.
func TestExpected_AUseInsideLongerTextIsAnchoredOnItsNeighbours(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "5"})
	exp := models.HTTPResp{
		Header: map[string]string{"Location": "/users/{{string .id}}", "Set-Cookie": "sid=v1.{{string .id}}; Path=/u/{{string .id}}"},
		Body:   `{"link":"/users/{{string .id}}/profile"}`,
	}
	res := r.Expected(exp, models.HTTPResp{
		Header: map[string]string{"Location": "/users/5", "Set-Cookie": "sid=v1.5; Path=/u/5"},
		Body:   `{"link":"/users/5/profile"}`,
	}, ExpectOptions{})
	assert.Empty(t, res.Failures)

	res = r.Expected(exp, models.HTTPResp{
		Header: map[string]string{"Location": "/users/15", "Set-Cookie": "sid=v1.15; Path=/u/5"},
		Body:   `{"link":"/users/15/profile"}`,
	}, ExpectOptions{})
	var where []string
	for _, f := range res.Failures {
		where = append(where, f.Where)
		assert.Contains(t, f.Why, `holds "15" there`, f.String())
	}
	assert.Equal(t, []string{"body.link", "header Location", "header Set-Cookie.sid"}, where)

	res = r.Expected(exp, models.HTTPResp{
		Header: map[string]string{"Location": "/accounts/5", "Set-Cookie": "sid=v1.5; Path=/u/5"},
		Body:   `{"link":"/users/5/settings"}`,
	}, ExpectOptions{})
	require.Len(t, res.Failures, 2, "%v", res.Failures)
	assert.Contains(t, res.Failures[0].Why, "as written around it")

	text := models.HTTPResp{Body: "user={{string .id}} count=5"}
	assert.Empty(t, r.Expected(text, models.HTTPResp{Body: "user=5 count=5"}, ExpectOptions{}).Failures)
	fs := r.Expected(text, models.HTTPResp{Body: "user=15 count=5"}, ExpectOptions{}).Failures
	require.Len(t, fs, 1)
	assert.Equal(t, "body", fs[0].Where)
}

// A PLACEHOLDER IN A JSON OBJECT KEY is a use: rendered where it is sent and
// where it is expected, and the answer must name that key.
func TestExpected_APlaceholderInAnObjectKey(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1", "n": "7"})
	req := models.HTTPReq{Body: `{"{{string .id}}":true,"v":"{{string .id}}","k{{int .n}}":1}`}
	sent := r.Request(req)
	assert.Equal(t, `{"usr_1":true,"v":"usr_1","k7":1}`, sent.Body)
	assert.Equal(t, `{"{{string .id}}":true,"v":"{{string .id}}","k{{int .n}}":1}`, req.Body, "the test case is not changed")

	exp := models.HTTPResp{Body: `{"{{string .id}}":{"name":"a","id":"{{string .id}}"}}`}
	res := r.Expected(exp, models.HTTPResp{Body: `{"usr_1":{"name":"a","id":"usr_1"}}`}, ExpectOptions{})
	assert.Empty(t, res.Failures)
	assert.Equal(t, `{"usr_1":{"name":"a","id":"usr_1"}}`, res.Resp.Body)
	assert.Equal(t, `{"usr_1":{"name":"a","id":"usr_1"}}`, res.RecordedBody)

	res = r.Expected(exp, models.HTTPResp{Body: `{"usr_2":{"name":"a","id":"usr_1"}}`}, ExpectOptions{})
	var where []string
	for _, f := range res.Failures {
		where = append(where, f.Where)
	}
	assert.Equal(t, []string{"body key usr_1", "body.usr_1.id"}, where)
	assert.Equal(t, `{"usr_1":1}`, r.ExpectedNoAnswer(models.HTTPResp{Body: `{"{{string .id}}":1}`}).Body)
}

// A streamed response's frames are what the stream comparison reads, so its
// uses are rendered there, field value by field value.
func TestExpectedNoAnswer_RendersEachStreamFrame(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1", "n": "7"})
	exp := models.HTTPResp{
		Body: `{"user":"{{string .id}}","n":"{{int .n}}"}`,
		StreamBody: []models.HTTPStreamChunk{
			{Data: []models.HTTPStreamDataField{{Key: "raw", Value: `{"user":"{{string .id}}","n":"{{int .n}}"}`}}},
			{Data: []models.HTTPStreamDataField{{Key: "event", Value: "user"}, {Key: "data", Value: `id={{string .id}}`}}},
		},
	}
	got := r.ExpectedNoAnswer(exp)
	assert.Equal(t, `{"user":"usr_1","n":7}`, got.StreamBody[0].Data[0].Value)
	assert.Equal(t, "user", got.StreamBody[1].Data[0].Value)
	assert.Equal(t, "id=usr_1", got.StreamBody[1].Data[1].Value)
	assert.Equal(t, `id={{string .id}}`, exp.StreamBody[1].Data[1].Value, "the test case's frames are not changed")

	// The frames are what the stream comparison reads, whatever the flat
	// body holds (a hand-written test case may leave it empty).
	frames := models.HTTPResp{StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "raw", Value: `{"user":"{{string .id}}"}`}}}}}
	assert.Equal(t, `{"user":"usr_1"}`, r.ExpectedNoAnswer(frames).StreamBody[0].Data[0].Value)
}

// A SANITIZED SECRET IN A STREAMED FRAME IS THE LITERAL IT REDACTS (disclosed
// verdict change 2). A set keploy sanitize wrote holds no template, only
// secrets; a frame that holds only a secret is rendered too, so the stream
// comparison reads the recorded value: an app that answers it passes, and a
// fresh token still fails. main compared the frame against the placeholder's
// own text, so such a test case failed on every run.
func TestExpectedStream_RendersASecretInAFrame(t *testing.T) {
	r := NewRun(nil, nil, map[string]interface{}{"k": "tok_REC1"})
	exp := models.HTTPResp{StreamBody: []models.HTTPStreamChunk{
		{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"token":"{{string .secret.k}}"}`}}},
		{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"tick":1}`}}},
	}}
	got := r.ExpectedStream(exp)
	assert.Equal(t, `{"token":"tok_REC1"}`, got.StreamBody[0].Data[0].Value)
	assert.Equal(t, `{"tick":1}`, got.StreamBody[1].Data[0].Value)
	assert.Equal(t, `{"token":"{{string .secret.k}}"}`, exp.StreamBody[0].Data[0].Value, "the test case's frames are not changed")
}

// A gRPC use is asserted on its own, like an HTTP one: a header that is one
// use must equal the key's value, and a message's text must read as written
// with each use at its value.
func TestGRPCUseFailures(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1"})
	exp := models.GrpcResp{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-user": "{{string .id}}", "x-trace": "t-{{string .id}}"}}}
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "{{string .id}}"` + "\n" + `2: "alice"`}})
	answer := func(user, trace, body string) models.GrpcResp {
		a := models.GrpcResp{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-user": user, "x-trace": trace}}}
		a.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: body}})
		return a
	}
	assert.Empty(t, r.GRPCUseFailures(exp, answer("usr_1", "t-usr_1", `1: "usr_1"`+"\n"+`2: "alice"`)))
	fs := r.GRPCUseFailures(exp, answer("usr_2", "t-usr_2", `1: "usr_2"`+"\n"+`2: "alice"`))
	var where []string
	for _, f := range fs {
		where = append(where, f.Where)
	}
	assert.Equal(t, []string{"header x-trace", "header x-user", "body"}, where)
	assert.Empty(t, newRun(t, nil).GRPCUseFailures(exp, answer("x", "y", "z")), "a set without templates asserts nothing")
}

// RULE 3 WHATEVER THE COMPARISON READS. A capture that finds no value is a
// Failure of its own, so it fails its test case even where the matcher does
// not look: a body that is not JSON, a field under noise.
func TestExpected_AMissedCaptureIsAFailureOfItsOwn(t *testing.T) {
	for _, answer := range []string{``, `{"id":"usr_LIVE`, `{"name":"alice"}`, `<html>oops</html>`} {
		r := newRun(t, map[string]interface{}{"id": "usr_REC"})
		res := r.Expected(models.HTTPResp{Body: `{"id":"{{capture string .id}}","name":"alice"}`}, models.HTTPResp{Body: answer}, ExpectOptions{})
		require.Len(t, res.Failures, 1, "answer %q", answer)
		assert.Equal(t, "body.id", res.Failures[0].Where)
		assert.Equal(t, "{{capture string .id}}", res.Failures[0].Placeholder)
		assert.Equal(t, "usr_REC", r.Values()["id"])
	}
}

// A VARIABLE PLACE IS ASSERTED ON ITS OWN. A use must hold its key's value in
// the answer, checked against the answer itself, so noise over the field or
// a body the comparison does not read cannot let it through.
func TestExpected_AUseIsCheckedAgainstTheAnswerOnItsOwn(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_LIVE", "n": "7654321"})
	exp := models.HTTPResp{
		Header: map[string]string{"Location": "/users/{{string .id}}", "X-Id": "{{string .id}}", "Set-Cookie": "uid={{string .id}}; Path=/"},
		Body:   `{"id":"{{string .id}}","n":"{{int .n}}","note":"made {{string .id}} today","items":[{"id":"x"},{"id":"{{string .id}}"}]}`,
	}
	good := models.HTTPResp{
		Header: map[string]string{"Location": "/users/usr_LIVE", "X-Id": "usr_LIVE", "Set-Cookie": "uid=usr_LIVE; Path=/"},
		Body:   `{"id":"usr_LIVE","n":7654321.0,"note":"made usr_LIVE today","items":[{"id":"x"},{"id":"usr_LIVE"}]}`,
	}
	res := r.Expected(exp, good, ExpectOptions{})
	assert.Empty(t, res.Failures, "every use holds its value; 7654321.0 is the number 7654321")

	bad := models.HTTPResp{
		Header: map[string]string{"Location": "/users/usr_OTHER", "X-Id": "usr_OTHER", "Set-Cookie": "uid=usr_OTHER; Path=/"},
		Body:   `{"id":"usr_OTHER","n":1,"note":"made usr_OTHER today","items":[{"id":"usr_LIVE"},{"id":"x"}]}`,
	}
	res = r.Expected(exp, bad, ExpectOptions{})
	var where []string
	for _, f := range res.Failures {
		where = append(where, f.Where)
	}
	assert.Equal(t, []string{"body.id", "body.n", "body.note", "body.items[1].id", "header Location", "header Set-Cookie.uid", "header X-Id"}, where)

	res = r.Expected(exp, bad, ExpectOptions{IgnoreOrdering: true})
	for _, f := range res.Failures {
		assert.NotEqual(t, "body.items[1].id", f.Where, "under ignoreOrdering any element of the array may hold it")
	}
	res = r.Expected(exp, models.HTTPResp{Body: "not json"}, ExpectOptions{})
	assert.NotEmpty(t, res.Failures, "an answer that is not JSON holds none of the values")
	// Nor one encoding/json rejects, whatever it holds: a value written as a
	// placeholder outside a string is an expected body's form, not an
	// answer's, and a raw TAB in a string is no JSON either.
	for _, notJSON := range []string{
		`{"id":"usr_LIVE","n":7654321,"note":"made usr_LIVE today","items":[{"id":"x"},{"id":"usr_LIVE"}],"t":{{string .x}}}`,
		"{\"id\":\"usr_LIVE\",\"n\":7654321,\"note\":\"made usr_LIVE today\",\"items\":[{\"id\":\"x\"},{\"id\":\"usr_LIVE\"}],\"t\":\"a\tb\"}",
	} {
		res = r.Expected(models.HTTPResp{Body: exp.Body}, models.HTTPResp{Body: notJSON}, ExpectOptions{})
		assert.NotEmpty(t, res.Failures, "%q holds none of the values: it is not JSON", notJSON)
	}

	res = r.Expected(exp, models.HTTPResp{Header: map[string]string{"Location": "/users/usr_LIVE", "X-Id": "usr_LIVE"}, Body: good.Body}, ExpectOptions{})
	require.Len(t, res.Failures, 1, "a cookie that holds a use must be set: %v", res.Failures)
	assert.Equal(t, "header Set-Cookie.uid", res.Failures[0].Where)
	res = r.Expected(exp, models.HTTPResp{Header: map[string]string{"Location": "/users/usr_LIVE", "X-Id": "usr_LIVE", "Set-Cookie": "other=1"}, Body: good.Body}, ExpectOptions{})
	require.Len(t, res.Failures, 1, "a cookie that holds a use must be set: %v", res.Failures)
	assert.Contains(t, res.Failures[0].Why, "set cookie uid")
}

// A SECRET IS THE LITERAL IT REDACTS, not a variable: keploy sanitize wrote
// {{string .secret.K}} over a value the recording held. In an expected
// response it is rendered to that value and compared as the literal would be,
// noise and all; it is never a variable place, so it fails nothing on its
// own. Beside a use in one string it is literal text the use is anchored on,
// and a failure there never says the secret's value.
func TestExpected_ASecretIsTheLiteralItRedacts(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"id": "1"}, map[string]interface{}{"apiKey": "sk-live-123"})
	exp := models.HTTPResp{
		Header: map[string]string{"X-Key": "{{string .secret.apiKey }}", "Location": "/keys/{{string .secret.apiKey}}"},
		Body:   `{"key":"{{string .secret.apiKey}}","n":"{{string .secret.apiKey}}"}`,
	}
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"X-Key": "sk-other", "Location": "/keys/sk%2Dlive%2D123"}, Body: `{"key":"sk-other"}`}, ExpectOptions{})
	assert.Empty(t, res.Failures, "a secret is compared by the matcher, as a literal")
	assert.Equal(t, `{"key":"sk-live-123","n":"sk-live-123"}`, res.Resp.Body)
	assert.Equal(t, "sk-live-123", res.Resp.Header["X-Key"])
	assert.Equal(t, "/keys/sk-live-123", res.Resp.Header["Location"], "a URL-valued header of secrets only is rendered as written, never read decoded")
	assert.Equal(t, `{"key":"sk-live-123","n":"sk-live-123"}`, res.RecordedBody)

	beside := models.HTTPResp{Body: `{"auth":"{{string .secret.apiKey}}:{{string .id}}"}`}
	assert.Empty(t, r.Expected(beside, models.HTTPResp{Body: `{"auth":"sk-live-123:1"}`}, ExpectOptions{}).Failures)
	fs := r.Expected(beside, models.HTTPResp{Body: `{"auth":"sk-other:1"}`}, ExpectOptions{}).Failures
	require.Len(t, fs, 1, "the use is anchored on the secret's literal")
	assert.Equal(t, "{{string .id}}", fs[0].Placeholder)
	assert.NotContains(t, fs[0].String(), "sk-live-123")

	// In a URL-valued header a secret beside a variable is the literal the
	// recording held there, already escaped for its place: read as written,
	// never as a value to decode ("a%2Fb", not "a%252Fb").
	esc := NewRun(nil, map[string]interface{}{"id": "1"}, map[string]interface{}{"part": "a%2Fb"})
	loc := models.HTTPResp{Header: map[string]string{"Location": "/k/{{string .secret.part}}/{{string .id}}"}}
	assert.Empty(t, esc.Expected(loc, models.HTTPResp{Header: map[string]string{"Location": "/k/a%2Fb/1"}}, ExpectOptions{}).Failures)
	assert.NotEmpty(t, esc.Expected(loc, models.HTTPResp{Header: map[string]string{"Location": "/k/a%252Fb/1"}}, ExpectOptions{}).Failures)

	cookie := models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{string .secret.apiKey}}; Path=/"}}
	assert.Empty(t, r.Expected(cookie, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=fresh; Path=/"}}, ExpectOptions{}).Failures)
	g := models.GrpcResp{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-key": "{{string .secret.apiKey}}"}}}
	g.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "{{string .secret.apiKey}}"`}})
	ga := models.GrpcResp{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-key": "fresh"}}}
	ga.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "fresh"`}})
	assert.Empty(t, r.GRPCUseFailures(g, ga))
	stream := models.HTTPResp{Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: `{"k":"{{string .secret.apiKey}}"}`}
	assert.False(t, r.StreamHasUses(stream), "a secret in a frame is not kept to be asserted")
	assert.Empty(t, r.StreamUseFailures(stream, `{"k":"fresh"}`, true, models.HTTPResp{}, ExpectOptions{}))
}

// The expected response handed in is not written: its header map is the
// test case's, re-used by the next pass and by a re-send.
func TestExpected_LeavesTheExpectedResponseAsWritten(t *testing.T) {
	r := newRun(t, map[string]interface{}{"sid": "S0", "id": "usr_REC"})
	exp := models.HTTPResp{
		Header: map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/", "Location": "/u/{{string .id}}", "X": "{{capture string .id}}"},
		Body:   `{"id":"{{string .id}}"}`,
	}
	keep := copyStringMap(exp.Header)
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=S1; Path=/", "Location": "/u/usr_1", "X": "usr_1"}, Body: `{"id":"usr_1"}`}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, keep, exp.Header)
	assert.Equal(t, "sid=S1; Path=/", res.Resp.Header["Set-Cookie"])
}

// A USE IS ANCHORED AT BOTH ENDS: text before or after what the expected value
// says is not the value at its place, so /users/5/extra, /v2/users/5 and a
// link with more path after it each fail the use, in a header, a JSON string,
// a cookie and a gRPC message alike.
func TestExpected_AUseIsAnchoredAtBothEnds(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "5"})
	exp := models.HTTPResp{
		Header: map[string]string{"Location": "/users/{{string .id}}", "Set-Cookie": "sid=v1.{{string .id}}; Path=/"},
		Body:   `{"link":"/users/{{string .id}}/profile"}`,
	}
	good := models.HTTPResp{Header: map[string]string{"Location": "/users/5", "Set-Cookie": "sid=v1.5; Path=/"}, Body: `{"link":"/users/5/profile"}`}
	require.Empty(t, r.Expected(exp, good, ExpectOptions{}).Failures)
	for _, c := range []struct {
		name, where string
		answer      func(a *models.HTTPResp)
	}{
		{"text after the use in a header", "header Location", func(a *models.HTTPResp) { a.Header["Location"] = "/users/5/extra" }},
		{"text before the use in a header", "header Location", func(a *models.HTTPResp) { a.Header["Location"] = "/v2/users/5" }},
		{"text after the string's end", "body.link", func(a *models.HTTPResp) { a.Body = `{"link":"/users/5/profile/x"}` }},
		{"text before the string's start", "body.link", func(a *models.HTTPResp) { a.Body = `{"link":"https://h/users/5/profile"}` }},
		{"text after a cookie value", "header Set-Cookie.sid", func(a *models.HTTPResp) { a.Header["Set-Cookie"] = "sid=v1.5x; Path=/" }},
		{"text before a cookie value", "header Set-Cookie.sid", func(a *models.HTTPResp) { a.Header["Set-Cookie"] = "sid=xv1.5; Path=/" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			answer := models.HTTPResp{Header: copyStringMap(good.Header), Body: good.Body}
			c.answer(&answer)
			fs := r.Expected(exp, answer, ExpectOptions{}).Failures
			require.Len(t, fs, 1, "%v", fs)
			assert.Equal(t, c.where, fs[0].Where)
		})
	}
	msg := models.GrpcResp{}
	msg.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "{{string .id}}"`}})
	for _, body := range []string{`1: "5"` + "\n" + `2: "x"`, `0: "y"` + "\n" + `1: "5"`} {
		answer := models.GrpcResp{}
		answer.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: body}})
		assert.Len(t, r.GRPCUseFailures(msg, answer), 1, "a gRPC message %q", body)
	}
}

// A KEY AN OBJECT REPEATS is read at its last value, as encoding/json,
// JSON.parse and json.loads read it: a capture takes the last, and a use must
// be the last, also inside an array under ignoreOrdering.
func TestExpected_ARepeatedKeyIsReadAtItsLastValue(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "REC"})
	res := r.Expected(models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}, models.HTTPResp{Body: `{"id":"FIRST","id":"LAST"}`}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "LAST", r.Values()["id"])
	assert.Equal(t, `{"id":"LAST"}`, res.Resp.Body)

	r = newRun(t, map[string]interface{}{"id": "LIVE"})
	use := models.HTTPResp{Body: `{"id":"{{string .id}}"}`}
	assert.Len(t, r.Expected(use, models.HTTPResp{Body: `{"id":"LIVE","id":"OTHER"}`}, ExpectOptions{}).Failures, 1, "a reader sees OTHER")
	assert.Empty(t, r.Expected(use, models.HTTPResp{Body: `{"id":"OTHER","id":"LIVE"}`}, ExpectOptions{}).Failures, "a reader sees LIVE")
	inArray := models.HTTPResp{Body: `{"items":[{"id":"{{string .id}}"}]}`}
	assert.Len(t, r.Expected(inArray, models.HTTPResp{Body: `{"items":[{"id":"LIVE","id":"OTHER"}]}`}, ExpectOptions{IgnoreOrdering: true}).Failures, 1,
		"under ignoreOrdering an element still holds its key's last value")
	assert.Empty(t, r.Expected(inArray, models.HTTPResp{Body: `{"items":[{"id":"x"},{"id":"OTHER","id":"LIVE"}]}`}, ExpectOptions{IgnoreOrdering: true}).Failures)
}

// A REPEATED KEY'S LAST OCCURRENCE IS ITS WHOLE VALUE, a scalar or not: a
// leaf counts only if, for every key on its path, it lies inside that key's
// last occurrence. {"id":"LIVE","id":{"x":"OTHER"}} has no scalar at id, and
// {"a":{"id":"X"},"a":{}} none at a.id: a use there fails, and a capture
// finds nothing (U2, U3).
func TestExpected_ARepeatedKeysLastOccurrenceIsItsWholeValue(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "LIVE"})
	use := models.HTTPResp{Body: `{"id":"{{string .id}}"}`}
	for _, answer := range []string{`{"id":"LIVE","id":{"x":"OTHER"}}`, `{"id":"LIVE","id":["LIVE"]}`, `{"id":"LIVE","id":null}`} {
		fs := r.Expected(use, models.HTTPResp{Body: answer}, ExpectOptions{}).Failures
		require.Len(t, fs, 1, "%s: a reader sees the last id", answer)
		assert.Equal(t, "body.id", fs[0].Where)
	}
	nested := models.HTTPResp{Body: `{"a":{"id":"{{string .id}}"}}`}
	assert.Len(t, r.Expected(nested, models.HTTPResp{Body: `{"a":{"id":"LIVE"},"a":{}}`}, ExpectOptions{}).Failures, 1, "a.id lies in a replaced a")
	assert.Empty(t, r.Expected(nested, models.HTTPResp{Body: `{"a":{},"a":{"id":"LIVE"}}`}, ExpectOptions{}).Failures)
	key := models.HTTPResp{Body: `{"m":{"{{string .id}}":1}}`}
	assert.Len(t, r.Expected(key, models.HTTPResp{Body: `{"m":{"LIVE":1},"m":{"x":1}}`}, ExpectOptions{}).Failures, 1, "an object key in a replaced member is not the answer's")

	for _, answer := range []string{`{"id":"usr_X","id":{"y":1}}`, `{"id":"usr_X","id":[1]}`} {
		c := newRun(t, map[string]interface{}{"id": "REC"})
		res := c.Expected(models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}, models.HTTPResp{Body: answer}, ExpectOptions{})
		require.Len(t, res.Failures, 1, "%s: no scalar is at id", answer)
		assert.Equal(t, "REC", c.Values()["id"], "a capture that finds nothing leaves the key")
	}
}

// A NUMBER USE IS EXACT: a value above 2^64, or with more digits than a float
// keeps, equals only itself.
func TestExpected_ANumberUseIsComparedExactly(t *testing.T) {
	r := newRun(t, map[string]interface{}{"n": "123456789012345678901", "f": "0.1000000000000000000001"})
	exp := models.HTTPResp{Body: `{"n":"{{int .n}}","f":"{{float .f}}"}`}
	assert.Empty(t, r.Expected(exp, models.HTTPResp{Body: `{"n":123456789012345678901,"f":0.10000000000000000000010}`}, ExpectOptions{}).Failures)
	fs := r.Expected(exp, models.HTTPResp{Body: `{"n":123456789012345678902,"f":0.1000000000000000000002}`}, ExpectOptions{}).Failures
	var where []string
	for _, f := range fs {
		where = append(where, f.Where)
	}
	assert.Equal(t, []string{"body.n", "body.f"}, where)
}

// A URL-VALUED HEADER (Location, Content-Location) is read as keploy
// templatize reads it: each path segment and query value decoded for its
// place. A capture takes its span decoded, so the next request sends it
// escaped once; a use holds when the answer's span decodes to the value,
// however the answer escapes it. Any other header is compared as written.
func TestExpected_AURLValuedHeaderIsReadDecodedForItsPlace(t *testing.T) {
	r := newRun(t, map[string]interface{}{"posts": "thé-42", "q": "a b", "id": "urn:x:1"})

	res := r.Expected(models.HTTPResp{Header: map[string]string{"Location": "/posts/{{capture string .posts}}"}},
		models.HTTPResp{Header: map[string]string{"Location": "/posts/th%C3%A9-43"}}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "thé-43", r.Values()["posts"])
	assert.Equal(t, "/posts/th%C3%A9-43", res.Resp.Header["Location"])
	assert.Equal(t, "http://h/posts/th%C3%A9-43", r.Request(models.HTTPReq{URL: "http://h/posts/{{string .posts}}"}).URL, "escaped once, never twice")

	res = r.Expected(models.HTTPResp{Header: map[string]string{"Content-Location": "/s?q={{capture string .q}}&v=1"}},
		models.HTTPResp{Header: map[string]string{"Content-Location": "/s?q=c+d%2Be&v=1"}}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "c d+e", r.Values()["q"], "a query value is query-decoded")

	r.Apply([]Capture{{Key: "id", Value: "urn:x:2"}})
	use := models.HTTPResp{Header: map[string]string{"Location": "/things/{{string .id}}?q={{string .q}}"}}
	for _, live := range []string{"/things/urn%3Ax%3A2?q=c+d%2Be", "/things/urn:x:2?q=c%20d%2be", "/things/urn%3ax%3a2?q=c+d%2Be"} {
		res = r.Expected(use, models.HTTPResp{Header: map[string]string{"Location": live}}, ExpectOptions{})
		assert.Empty(t, res.Failures, "%s holds urn:x:2 and c d+e", live)
		assert.Equal(t, live, res.Resp.Header["Location"], "a held URL header reads as the answer's, so the comparison agrees")
	}
	for _, live := range []string{"/things/urn%3Ax%3A3?q=c+d%2Be", "/things/urn%3Ax%3A2?q=c+d+e", "/things/urn%253Ax%253A2?q=c+d%2Be"} {
		fs := r.Expected(use, models.HTTPResp{Header: map[string]string{"Location": live}}, ExpectOptions{}).Failures
		require.Len(t, fs, 1, "%s does not hold both values: %v", live, fs)
		assert.Equal(t, "header Location", fs[0].Where)
	}
	// A '%' in a value is only ever its escape: /p/50%41 decodes to 50A,
	// which is not the value 50%41.
	pct := newRun(t, map[string]interface{}{"p": "50%41"})
	pctUse := models.HTTPResp{Header: map[string]string{"Location": "/p/{{string .p}}"}}
	assert.Empty(t, pct.Expected(pctUse, models.HTTPResp{Header: map[string]string{"Location": "/p/50%2541"}}, ExpectOptions{}).Failures)
	assert.Len(t, pct.Expected(pctUse, models.HTTPResp{Header: map[string]string{"Location": "/p/50%41"}}, ExpectOptions{}).Failures, 1)

	plain := models.HTTPResp{Header: map[string]string{"X-Path": "/things/{{string .id}}"}}
	assert.Len(t, r.Expected(plain, models.HTTPResp{Header: map[string]string{"X-Path": "/things/urn%3Ax%3A2"}}, ExpectOptions{}).Failures, 1,
		"a header that is not a URL is compared as written")

	// A raw '%' that is no escape is not the value: /p/50% is no URL that
	// holds 50%, though every span the failure's split finds reads as it.
	raw := newRun(t, map[string]interface{}{"p": "50%"})
	fs := raw.Expected(pctUse, models.HTTPResp{Header: map[string]string{"Location": "/p/50%"}}, ExpectOptions{}).Failures
	require.Len(t, fs, 1)
	assert.Contains(t, fs[0].Why, "as written around it")
	assert.Empty(t, raw.Expected(pctUse, models.HTTPResp{Header: map[string]string{"Location": "/p/50%25"}}, ExpectOptions{}).Failures)
}

// A URL-VALUED HEADER IS READ BY ITS PARTS (U1). A capture takes one path
// segment or one query value, never a '/' of the path or a '&', '=' or '#' of
// the query; a use's value holds only where the answer's part decodes to it,
// with the part's own syntax only escaped; and a value that holds a delimiter
// of its part (a '/' in a segment; '&', '=', '#' in a query value) cannot be
// told from two parts: a use of it fails, and so does a capture that decodes
// to one.
func TestExpected_AURLValuedHeaderIsReadByItsParts(t *testing.T) {
	loc := func(v string) models.HTTPResp { return models.HTTPResp{Header: map[string]string{"Location": v}} }
	for _, c := range []struct {
		name, value, exp, live string
		fails                  bool
	}{
		{"a '/' of the answer's path is not in the value", "a/b", "/things/{{string .id}}", "/things/a/b", true},
		{"a value holding '/' cannot be a path segment", "a/b", "/things/{{string .id}}", "/things/a%2Fb", true},
		{"a '?' of the answer starts its query", "a?b", "/things/{{string .id}}", "/things/a?b", true},
		{"an escaped '?' is the value's", "a?b", "/things/{{string .id}}", "/things/a%3Fb", false},
		{"a '#' of the answer starts its fragment", "a#b", "/things/{{string .id}}", "/things/a#b", true},
		{"a '&' of the answer's query ends the value", "x&y", "/search?q={{string .id}}&page=1", "/search?q=x&y&page=1", true},
		{"a value holding '&' cannot be a query value", "x&y", "/search?q={{string .id}}&page=1", "/search?q=x%26y&page=1", true},
		{"a value holding '=' cannot be a query value", "x=y", "/search?q={{string .id}}", "/search?q=x%3Dy", true},
		{"a '/' in a query value is the value's", "a/b", "/search?q={{string .id}}", "/search?q=a/b", false},
		{"a value holding '+' is read encoded", "a+b", "/search?q={{string .id}}", "/search?q=a%2Bb", false},
		{"a raw '+' in a query is a space", "a+b", "/search?q={{string .id}}", "/search?q=a+b", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"id": c.value})
			fs := r.Expected(loc(c.exp), loc(c.live), ExpectOptions{}).Failures
			if c.fails {
				require.Len(t, fs, 1, "%v", fs)
				assert.Equal(t, "header Location", fs[0].Where)
			} else {
				assert.Empty(t, fs)
			}
		})
	}
	// why is what the failure says: a capture never spans two parts (the
	// answer does not read as the URL around it), and one part that decodes
	// to a delimiter of its own cannot be told from two.
	const around, holds = "around it", "can hold"
	for _, c := range []struct {
		name, exp, live, want, why string
	}{
		{"a capture is one path segment", "/posts/{{capture string .id}}", "/posts/42?ref=x", "", around},
		{"a capture does not take a path's '/'", "/posts/{{capture string .id}}", "/posts/42/comments", "", around},
		{"a capture that decodes to a '/' fails", "/posts/{{capture string .id}}", "/posts/a%2Fb", "", holds},
		{"a capture is one query value", "/cb?code={{capture string .id}}&state=s", "/cb?code=abc&state=s", "abc", ""},
		{"a capture does not take a query's '&'", "/cb?code={{capture string .id}}&state=s", "/cb?code=a&b=c&state=s", "", around},
		{"a capture does not take a query's '='", "/cb?code={{capture string .id}}", "/cb?code=a=b", "", around},
		{"a capture that decodes to a '&' fails", "/cb?code={{capture string .id}}", "/cb?code=a%26b", "", holds},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"id": "REC"})
			res := r.Expected(loc(c.exp), loc(c.live), ExpectOptions{})
			if c.want == "" {
				require.Len(t, res.Failures, 1, "%v", res.Failures)
				assert.Contains(t, res.Failures[0].Why, c.why)
				assert.Equal(t, "REC", r.Values()["id"])
				return
			}
			require.Empty(t, res.Failures)
			assert.Equal(t, c.want, r.Values()["id"])
		})
	}
}

// A NESTED CREATE: a header capture beside a use of a key the response does
// not capture is anchored on the rendered text, so the child's id is taken
// from /orgs/<org>/users/<id> and the org is asserted where it sits.
func TestExpected_AHeaderCaptureBesideAUseIsAnchoredOnTheRenderedText(t *testing.T) {
	r := newRun(t, map[string]interface{}{"org": "org_1", "users": "usr_REC"})
	exp := models.HTTPResp{Header: map[string]string{"Location": "/orgs/{{string .org}}/users/{{capture string .users}}"}}
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Location": "/orgs/org_1/users/usr_LIVE"}}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "usr_LIVE", r.Values()["users"])
	assert.Equal(t, "/orgs/org_1/users/usr_LIVE", res.Resp.Header["Location"])

	res = r.Expected(exp, models.HTTPResp{Header: map[string]string{"Location": "/orgs/org_2/users/usr_NEW"}}, ExpectOptions{})
	var got []string
	for _, f := range res.Failures {
		got = append(got, f.Where+" "+f.Placeholder)
	}
	assert.Equal(t, []string{"header Location {{capture string .users}}", "header Location {{string .org}}"}, got)
	assert.Equal(t, "usr_LIVE", r.Values()["users"], "a capture that missed leaves the key as it was")

	x := newRun(t, map[string]interface{}{"org": "org_1", "users": "usr_REC"})
	res = x.Expected(models.HTTPResp{Header: map[string]string{"X-Child": "{{string .org}}:{{capture string .users}}"}},
		models.HTTPResp{Header: map[string]string{"X-Child": "org_1:usr_9"}}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "usr_9", x.Values()["users"])
}

// A COOKIE A LATER COOKIE OF THE SAME IDENTITY REPLACES at this run's values
// sets nothing a user agent keeps: a placeholder in it fails at run time,
// where Check, at the recorded values, could not see it.
func TestExpected_APlaceholderInACookieThisRunsValuesShadowFails(t *testing.T) {
	r := newRun(t, map[string]interface{}{"t": "rec", "sid": "S0", "u": "U0"})
	exp := models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/{{string .t}},sid=stale; Path=/acme,uid={{string .u}}; Path=/{{string .t}},uid=old; Path=/acme"}}
	require.Empty(t, r.Check([]*models.TestCase{httpCase("t", models.HTTPReq{}, exp)}, CheckOptions{}).Refused,
		"at the recorded values the cookies are of two Paths")
	r.Apply([]Capture{{Key: "t", Value: "acme"}})
	answer := models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=S1; Path=/acme,sid=stale; Path=/acme,uid=U0; Path=/acme,uid=old; Path=/acme"}}
	res := r.Expected(exp, answer, ExpectOptions{})
	var got []string
	for _, f := range res.Failures {
		got = append(got, f.Placeholder)
		assert.Equal(t, whyShadowedAtRun, f.Why)
	}
	assert.Equal(t, []string{"{{capture string .sid}}", "{{string .t}}", "{{string .u}}", "{{string .t}}"}, got)
	assert.Equal(t, "S0", r.Values()["sid"])
}

// EVERY COOKIE'S EXPIRES in a Set-Cookie value that holds a placeholder is
// compared as a lifetime, a literal sibling's too (Django's django_language
// next to its session): a later day's answer reads as the recording did.
func TestExpected_ALiteralSiblingCookieIsComparedByItsLifetime(t *testing.T) {
	r := newRun(t, map[string]interface{}{"sid": "S0"})
	exp := models.HTTPResp{Header: map[string]string{
		"Date":       "Mon, 05 Oct 2026 10:00:00 GMT",
		"Set-Cookie": "sessionid={{capture string .sid}}; expires=Mon, 19 Oct 2026 10:00:00 GMT; Path=/,django_language=en; expires=Tue, 05 Oct 2027 10:00:00 GMT; Path=/",
	}}
	live := "sessionid=S1; expires=Thu, 22 Oct 2026 12:00:00 GMT; Path=/,django_language=en; expires=Fri, 08 Oct 2027 12:00:00 GMT; Path=/"
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Date": "Thu, 08 Oct 2026 12:00:00 GMT", "Set-Cookie": live}}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, live, res.Resp.Header["Set-Cookie"])

	shorter := "sessionid=S2; expires=Thu, 22 Oct 2026 12:00:00 GMT; Path=/,django_language=en; expires=Fri, 09 Oct 2026 12:00:00 GMT; Path=/"
	res = r.Expected(exp, models.HTTPResp{Header: map[string]string{"Date": "Thu, 08 Oct 2026 12:00:00 GMT", "Set-Cookie": shorter}}, ExpectOptions{})
	assert.Contains(t, res.Resp.Header["Set-Cookie"], "django_language=en; expires=Tue, 05 Oct 2027 10:00:00 GMT", "another lifetime keeps the recorded date, so the comparison fails there")
}

// AN OBJECT KEY'S USE is asserted at its own path, array index included: the
// key in another element of the array does not hold it, unless the array is
// compared in any order.
func TestExpected_AnObjectKeyUseIsAtItsOwnIndex(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1"})
	exp := models.HTTPResp{Body: `{"items":[{"a":1},{"{{string .id}}":true}]}`}
	assert.Empty(t, r.Expected(exp, models.HTTPResp{Body: `{"items":[{"a":1},{"usr_1":true}]}`}, ExpectOptions{}).Failures)
	moved := models.HTTPResp{Body: `{"items":[{"usr_1":true},{"b":true}]}`}
	fs := r.Expected(exp, moved, ExpectOptions{}).Failures
	require.NotEmpty(t, fs)
	assert.Equal(t, "body key items[1].usr_1", fs[0].Where)
	for _, f := range r.Expected(exp, moved, ExpectOptions{IgnoreOrdering: true}).Failures {
		assert.NotContains(t, f.Where, "body key", "under ignoreOrdering any element may hold the key")
	}
}

// A STREAMED RESPONSE'S USES are asserted on their own: its headers against
// the answer's, and each recorded frame against the frame the answer
// delivered at its position (split as a recording splits a stream, by the
// recorded Content-Type), field by field, a JSON field by its paths and any
// other anchored on its text. A frame the answer did not deliver fails its
// uses when what was received is all it delivered; when the stream
// comparison stopped reading at a mismatch, the frames after it are not
// asserted (the test case fails on the mismatch).
func TestStreamUseFailures(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1"})
	frame := func(fields ...string) models.HTTPStreamChunk {
		c := models.HTTPStreamChunk{}
		for i := 0; i < len(fields); i += 2 {
			c.Data = append(c.Data, models.HTTPStreamDataField{Key: fields[i], Value: fields[i+1]})
		}
		return c
	}
	exp := models.HTTPResp{
		Header:     map[string]string{"X-User": "{{string .id}}", "Content-Type": "text/event-stream"},
		StreamBody: []models.HTTPStreamChunk{frame("event", "user", "data", `{"user":"{{string .id}}","n":1}`), frame("data", "id={{string .id}}")},
	}
	assert.True(t, r.StreamHasUses(exp))
	assert.False(t, r.StreamHasUses(models.HTTPResp{StreamBody: []models.HTTPStreamChunk{frame("data", "{{string .other}}")}}))
	answer := func(user string) models.HTTPResp { return models.HTTPResp{Header: map[string]string{"X-User": user}} }
	good := "event: user\ndata: {\"n\":1, \"user\":\"usr_1\"}\n\ndata: id=usr_1\n\n"
	assert.Empty(t, r.StreamUseFailures(exp, good, true, answer("usr_1"), ExpectOptions{}))

	bad := "event: user\ndata: {\"user\":\"usr_2\",\"n\":1}\n\ndata: id=usr_2\n\n"
	var where []string
	for _, f := range r.StreamUseFailures(exp, bad, true, answer("usr_2"), ExpectOptions{}) {
		where = append(where, f.Where)
	}
	assert.Equal(t, []string{"header X-User", "body frame 0 data.user", "body frame 1 data"}, where)

	fs := r.StreamUseFailures(exp, "event: user\n\n", true, answer("usr_1"), ExpectOptions{})
	require.Len(t, fs, 2, "frame 0 lacks its data field, and frame 1 was not delivered: %v", fs)
	assert.Contains(t, fs[0].Why, "data field")
	assert.Contains(t, fs[1].Why, "needs frame 1 of the answer; the answer delivered 1")
	fs = r.StreamUseFailures(exp, "event: user\n\n", false, answer("usr_1"), ExpectOptions{})
	require.Len(t, fs, 1, "a stream read only up to a mismatch: its later frames are not asserted")

	ndjson := models.HTTPResp{Header: map[string]string{"Content-Type": "application/x-ndjson"}, StreamBody: []models.HTTPStreamChunk{frame("raw", `{"user":"{{string .id}}"}`)}}
	fs = r.StreamUseFailures(ndjson, `{"user":"usr_2"}`+"\n", true, models.HTTPResp{}, ExpectOptions{})
	require.Len(t, fs, 1)
	assert.Equal(t, "body frame 0.user", fs[0].Where)
	// A frame encoding/json rejects is not read as JSON: the use finds no
	// value there.
	for _, notJSON := range []string{"{\"user\":\"usr_1\",\"t\":\"a\tb\"}", `{"user":"usr_1","t":"a\xb"}`, `{"user":"usr_1","t":{{string .x}}}`} {
		fs = r.StreamUseFailures(ndjson, notJSON+"\n", true, models.HTTPResp{}, ExpectOptions{})
		require.Len(t, fs, 1, "%q", notJSON)
		assert.Equal(t, "body frame 0.user", fs[0].Where)
	}
	assert.Empty(t, newRun(t, nil).StreamUseFailures(ndjson, "", true, models.HTTPResp{}, ExpectOptions{}))

	// A field a frame repeats is read at its own occurrence (ST2).
	twice := models.HTTPResp{Header: map[string]string{"Content-Type": "text/event-stream"}, StreamBody: []models.HTTPStreamChunk{frame("data", "{{string .id}}", "id", "1", "data", "{{string .id}}")}}
	assert.Empty(t, r.StreamUseFailures(twice, "data: usr_1\nid: 1\ndata: usr_1\n\n", true, models.HTTPResp{}, ExpectOptions{}))
	fs = r.StreamUseFailures(twice, "data: usr_1\nid: 1\ndata: usr_2\n\n", true, models.HTTPResp{}, ExpectOptions{})
	require.Len(t, fs, 1, "the second data field holds usr_2")
}

// A STREAMED BODY A RECORDING KEPT FLAT is read frame by frame like one kept
// as frames (S2): split as a recording splits a stream, so each use is
// asserted against the frame the answer delivered at its position, and the
// stream comparison is given those frames (ExpectedStream) instead of none,
// which matches any stream. A body that does not split (multipart) has no
// frames: StreamFrames is nil, and Check refuses a variable there.
func TestStreamUseFailures_AFlatStreamedBodyIsReadFrameByFrame(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1"})
	flat := models.HTTPResp{Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: `{"user":"{{string .id}}","n":1}` + "\n" + `{"user":"{{string .id}}","n":2}`}
	require.True(t, r.StreamHasUses(flat))
	assert.Empty(t, r.StreamUseFailures(flat, `{"user":"usr_1","n":1}`+"\n"+`{"n":2,"user":"usr_1"}`+"\n", true, models.HTTPResp{}, ExpectOptions{}))
	var where []string
	for _, f := range r.StreamUseFailures(flat, `{"user":"usr_1","n":1}`+"\n"+`{"user":"usr_9","n":2}`+"\n", true, models.HTTPResp{}, ExpectOptions{}) {
		where = append(where, f.Where)
	}
	assert.Equal(t, []string{"body frame 1.user"}, where)

	view := r.ExpectedStream(flat)
	require.Len(t, view.StreamBody, 2, "the comparison is given the frames")
	assert.Equal(t, `{"user":"usr_1","n":2}`, view.StreamBody[1].Data[0].Value)
	assert.Equal(t, `{"user":"{{string .id}}","n":1}`+"\n"+`{"user":"{{string .id}}","n":2}`, flat.Body, "the expected response is not written")
	plain := models.HTTPResp{Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: `{"n":1}`}
	assert.Empty(t, r.ExpectedStream(plain).StreamBody, "a stream with no use is compared as before")

	multipart := models.HTTPResp{Header: map[string]string{"Content-Type": "multipart/mixed; boundary=XYZ"}, Body: "--XYZ\r\nContent-Type: application/json\r\n\r\n{\"user\":\"{{string .id}}\"}\r\n--XYZ--\r\n"}
	assert.Nil(t, StreamFrames(multipart))
	assert.False(t, r.StreamHasUses(multipart))
}

// A CAPTURE INSIDE A LONGER STRING takes what its text leaves, and an answer
// shorter than that text, whose prefix and suffix overlap, holds nothing.
func TestExpected_ACaptureInALongerStringNeedsItsTextAroundIt(t *testing.T) {
	r := newRun(t, map[string]interface{}{"k": "REC"})
	exp := models.HTTPResp{Body: `{"t":"ab{{capture string .k}}ba"}`}
	res := r.Expected(exp, models.HTTPResp{Body: `{"t":"abXba"}`}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "X", r.Values()["k"])
	for _, live := range []string{`{"t":"aba"}`, `{"t":"abba"}`} {
		res = r.Expected(exp, models.HTTPResp{Body: live}, ExpectOptions{})
		require.Len(t, res.Failures, 1, "%s holds no capture between ab and ba", live)
	}

	// CA1': the text after the capture must be there too. An answer whose
	// prefix holds and whose suffix does not is no value of the capture, and
	// nothing is captured from it.
	path := NewRun(nil, map[string]interface{}{"k": "1"}, nil)
	pexp := models.HTTPResp{Body: `{"t":"/users/{{capture string .k}}/profile"}`}
	res = path.Expected(pexp, models.HTTPResp{Body: `{"t":"/users/4242424242/orders"}`}, ExpectOptions{})
	require.Len(t, res.Failures, 1, "the answer does not end /profile")
	assert.Contains(t, res.Failures[0].Why, `"/users/"…"/profile"`)
	assert.Empty(t, res.Captures)
	assert.Equal(t, "1", path.Values()["k"], "the key keeps its value")
	res = path.Expected(pexp, models.HTTPResp{Body: `{"t":"/users/4242424242/profile"}`}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "4242424242", path.Values()["k"])
}

// A gRPC MESSAGE THE ANSWER LACKS fails each use the recorded message holds.
func TestGRPCUseFailures_AMissingMessageFailsItsUses(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1"})
	exp := models.GrpcResp{}
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "x"`}, {DecodedData: `1: "{{string .id}}"`}})
	answer := models.GrpcResp{}
	answer.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "x"`}})
	fs := r.GRPCUseFailures(exp, answer)
	require.Len(t, fs, 1)
	assert.Equal(t, "body message 1", fs[0].Where)
	assert.Contains(t, fs[0].Why, "needs message 1 of the answer; it has 1")
}

// A gRPC response's uses are asserted message by message, each against the
// answer's message at its position, and in its pseudo headers too.
func TestGRPCUseFailures_EachMessageAndThePseudoHeaders(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1", "st": "200"})
	exp := models.GrpcResp{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":status": "{{string .st}}"}}}
	exp.SetMessages([]models.GrpcLengthPrefixedMessage{{DecodedData: `1: "{{string .id}}"`}, {DecodedData: `1: "{{string .id}}"`}})
	answer := func(status string, ids ...string) models.GrpcResp {
		a := models.GrpcResp{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":status": status}}}
		var ms []models.GrpcLengthPrefixedMessage
		for _, id := range ids {
			ms = append(ms, models.GrpcLengthPrefixedMessage{DecodedData: `1: "` + id + `"`})
		}
		a.SetMessages(ms)
		return a
	}
	assert.Empty(t, r.GRPCUseFailures(exp, answer("200", "usr_1", "usr_1")))
	var where []string
	for _, f := range r.GRPCUseFailures(exp, answer("500", "usr_1", "usr_2")) {
		where = append(where, f.Where)
	}
	assert.Equal(t, []string{"header :status", "body message 1"}, where)
}

// A CAPTURE IN A URL PATH SEGMENT takes one segment: never a '#' and what
// follows it (F6), as never a '/' or a '?'. An answer whose segment runs on
// into a fragment does not read as the expected Location.
func TestExpected_APathCaptureNeverSpansAFragment(t *testing.T) {
	r := newRun(t, map[string]interface{}{"f": "REC"})
	exp := models.HTTPResp{Header: map[string]string{"Location": "/files/{{capture string .f}}"}}
	res := r.Expected(exp, models.HTTPResp{Header: map[string]string{"Location": "/files/abc#frag"}}, ExpectOptions{})
	require.Len(t, res.Failures, 1)
	assert.Empty(t, res.Captures)
	res = r.Expected(exp, models.HTTPResp{Header: map[string]string{"Location": "/files/abc"}}, ExpectOptions{})
	require.Empty(t, res.Failures)
	assert.Equal(t, "abc", r.Values()["f"])
}

// A BARE USE INSIDE A JSON DOCUMENT ({"n":{{int .n}}}) is a whole JSON value
// (F9), read by its path, so a pretty-printed answer holding the value passes;
// only a body that is one placeholder alone is text.
func TestExpected_ABareUseInADocumentIsAJSONValue(t *testing.T) {
	r := newRun(t, map[string]interface{}{"n": "7"})
	exp := models.HTTPResp{Body: `{"n":{{int .n}}}`}
	assert.Empty(t, r.Expected(exp, models.HTTPResp{Body: "{\n  \"n\": 7\n}"}, ExpectOptions{}).Failures)
	assert.NotEmpty(t, r.Expected(exp, models.HTTPResp{Body: "{\n  \"n\": 8\n}"}, ExpectOptions{}).Failures)
}

// A SECRET IN A COOKIE'S OR AN ATTRIBUTE'S NAME IS THE NAME THE RECORDING HELD.
// A use beside it is read against the answer's cookie of that identity, and
// the attribute of that name: an app that answers the recording passes, one
// that answers another value fails, and no failure prints the secret. Before,
// the name was read as the placeholder's own text, so no answer's cookie had
// it and the use failed even the recorded answer.
func TestExpected_ASecretInACookiesNameIsTheNameTheRecordingHeld(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"v": "abc"}, map[string]interface{}{"n": "sid", "a": "X-Mode"})
	for _, c := range []struct {
		name, expected, recordedAnswer, otherAnswer, secret string
	}{
		{"the cookie's name", "{{string .secret.n}}={{string .v}}; Path=/", "sid=abc; Path=/", "sid=OTHER; Path=/", "sid"},
		{"an attribute's name", "id=1; {{string .secret.a}}={{string .v}}", "id=1; X-Mode=abc", "id=1; X-Mode=OTHER", "X-Mode"},
	} {
		t.Run(c.name, func(t *testing.T) {
			exp := models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": c.expected}}
			got := r.Expected(exp, models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": c.recordedAnswer}}, ExpectOptions{})
			assert.Empty(t, got.Failures, "the recorded answer")
			got = r.Expected(exp, models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": c.otherAnswer}}, ExpectOptions{})
			require.Len(t, got.Failures, 1, "another value")
			assert.NotContains(t, got.Failures[0].Where+" "+got.Failures[0].Why, c.secret, "a failure prints the secret")
			// An answer that sets no such cookie: the failure names the
			// cookie as written.
			got = r.Expected(exp, models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": "other=1"}}, ExpectOptions{})
			require.NotEmpty(t, got.Failures, "no such cookie")
			assert.NotContains(t, got.Failures[0].Where+" "+got.Failures[0].Why, c.secret, "a failure prints the secret")
		})
	}
}

// A capture carries its placeholder's type, wherever it stands: a number is
// one value whatever its spelling, a string is its text.
func TestExpected_ACaptureCarriesItsPlaceholdersType(t *testing.T) {
	r := newRun(t, map[string]interface{}{"n": 1, "id": "u_rec", "price": 1.5})
	res := r.Expected(models.HTTPResp{
		Body:   `{"n":"{{capture int .n}}","id":"{{capture string .id}}"}`,
		Header: map[string]string{"X-Price": "{{capture float .price}}"},
	}, models.HTTPResp{Body: `{"n":7,"id":"u_live"}`, Header: map[string]string{"X-Price": "1.50"}}, ExpectOptions{})
	require.Empty(t, res.Failures)
	got := map[string]string{}
	for _, c := range res.Captures {
		got[c.Key] = c.Type
	}
	assert.Equal(t, map[string]string{"n": TypeInt, "id": TypeString, "price": TypeFloat}, got)
}

// ExpectedStart resolves an answer's headers as Expected does, its captures
// first and its uses at each key's value after them, on a copy of the run: a
// use of a key a cookie of the same answer captures takes the captured value,
// and the run moves no value. A use of a key the body captures is the app's,
// the body never having come; a header the answer lacks keeps its expected
// value. Only the headers that hold a placeholder are returned.
func TestExpectedStart_AHeaderIsResolvedAsTheMainPathResolvesIt(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"id": "REC", "sid": "S1"}, nil)
	exp := models.HTTPResp{
		StatusCode: 201,
		Header: map[string]string{
			"Content-Type": "application/json",
			"Location":     "/orders/{{string .id}}",
			"Set-Cookie":   "sid={{capture string .sid}}; Path=/",
			"X-Sid":        "{{string .sid}}",
			"X-Order":      "{{string .id}}",
		},
		Body: `{"id":"{{capture string .id}}"}`,
	}
	answer := models.HTTPResp{StatusCode: 201, Header: map[string]string{
		"Content-Type": "application/json",
		"Location":     "/orders/NEW",
		"Set-Cookie":   "sid=S2; Path=/",
		"X-Sid":        "S1",
	}}
	got := r.ExpectedStart(exp, answer, ExpectOptions{})
	assert.Equal(t, map[string]string{
		"Location":   "/orders/NEW",
		"Set-Cookie": "sid=S2; Path=/",
		"X-Sid":      "S2",
		"X-Order":    "REC",
	}, got)
	assert.Equal(t, map[string]string{"id": "REC", "sid": "S1"}, r.Values(), "the run moved")
}

// A run that defines only secrets resolves a header that holds one.
func TestExpectedStart_ASecretOnlyRunResolvesItsHeaders(t *testing.T) {
	r := NewRun(nil, nil, map[string]interface{}{"key": "k-123"})
	exp := models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Key": "{{.secret.key}}", "X-Plain": "p"}}
	got := r.ExpectedStart(exp, models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Key": "k-123"}}, ExpectOptions{})
	assert.Equal(t, map[string]string{"X-Key": "k-123"}, got)
	assert.Nil(t, NewRun(nil, nil, nil).ExpectedStart(exp, models.HTTPResp{}, ExpectOptions{}), "a set with no template resolved a header")
}
