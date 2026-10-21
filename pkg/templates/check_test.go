package templates

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

func httpCase(name string, req models.HTTPReq, resp models.HTTPResp) *models.TestCase {
	return &models.TestCase{Name: name, Kind: models.HTTP, HTTPReq: req, HTTPResp: resp}
}

// Rule 9: a placeholder a test case cannot run with is refused when the set
// loads, with the edit to make, written with the placeholder's own key (never
// a stand-in such as {{string .KEY}}).
func TestCheck_RefusesWhatCannotRun(t *testing.T) {
	cases := []struct {
		name string
		tc   *models.TestCase
		opts CheckOptions
		why  string
		edit string
	}{
		{"a capture in a request URL", httpCase("t", models.HTTPReq{URL: "http://h/{{capture string .id}}"}, models.HTTPResp{}), CheckOptions{}, whyCaptureInRequest, "write {{string .id}} here"},
		{"a capture in a request header", httpCase("t", models.HTTPReq{Header: map[string]string{"X": "{{capture string .id}}"}}, models.HTTPResp{}), CheckOptions{}, whyCaptureInRequest, "write {{string .id}} here"},
		{"a capture in a request body", httpCase("t", models.HTTPReq{Body: `{"a":"{{capture string .id}}"}`}, models.HTTPResp{}), CheckOptions{}, whyCaptureInRequest, "write {{string .id}} here"},
		{"a capture in a form value", httpCase("t", models.HTTPReq{Form: []models.FormData{{Key: "k", Values: []string{"{{capture string .id}}"}}}}, models.HTTPResp{}), CheckOptions{}, whyCaptureInRequest, "write {{string .id}} here"},
		{"a capture with no type", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture .id}}"}`}), CheckOptions{}, whyCaptureNoType, "write {{capture string .id}}, {{capture int .id}} or {{capture float .id}}"},
		{"a capture under .secret", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"k":"{{capture string .secret.k}}"}`}), CheckOptions{}, whyCaptureSecret, "write {{string .secret.k}} to assert it"},
		{"a capture in a gRPC case", &models.TestCase{Name: "t", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{Body: models.GrpcLengthPrefixedMessage{DecodedData: `1: "{{capture string .id}}"`}}}, CheckOptions{}, whyCaptureGRPC, "write {{string .id}} here, and capture the value in an earlier HTTP test case"},
		{"a capture in a streamed response", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}), CheckOptions{Streaming: func(*models.TestCase) bool { return true }}, whyCaptureStream, "assert the value with {{string .id}}, or capture it in an earlier, non-streamed test case"},
		{"one key captured twice in one response", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "/u/{{capture string .id}}"}, Body: `{"id":"{{capture string .id}}"}`}), CheckOptions{}, whyCaptureTwice, "keep the first capture of id and write {{string .id}} here to assert it"},
		{"one key captured by two cookies", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "a={{capture string .id}}; Path=/,b={{capture string .id}}"}}), CheckOptions{}, whyCaptureTwice, "keep the first capture of id and write {{string .id}} here to assert it"},
		{"a capture in an array under ignoreOrdering", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"ids":["{{capture string .id}}"]}`}), CheckOptions{IgnoreOrdering: true}, whyCaptureArray, "turn ignoreOrdering off for this set, or capture the value outside the array"},
		{"a capture outside a JSON string", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"n":{{capture int .id}}}`}), CheckOptions{}, whyCaptureNotLeaf, `write "{{capture int .id}}" (in quotes) where the value is a whole JSON value, or its recorded value "1" here`},
		{"a capture in a body that is not JSON", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `token={{capture string .id}}`}), CheckOptions{}, whyCaptureNotLeaf, `or its recorded value "1" here`},
		{"a capture beside another placeholder", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"a":"{{string .x}}-{{capture string .id}}"}`}), CheckOptions{}, whyCaptureNotAlone, `write {{string .x}} as "2" here, or capture the value where it stands alone`},
		{"a header capture beside a use of a key the response captures", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "/{{string .x}}/{{capture string .id}}"}, Body: `{"x":"{{capture string .x}}"}`}), CheckOptions{}, whyCaptureBesideOwnUse, `write {{string .x}} as "2" here`},
		{"a use in a cookie a later one of its identity replaces", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{string .id}}; Path=/,sid=S2; Path=/"}}), CheckOptions{}, whyCookieShadowed, "move the placeholder to the last cookie of that name"},
		{"a capture in a cookie's attribute", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=S1; Domain={{capture string .id}}"}}), CheckOptions{}, whyCaptureNotCookie, "sid={{capture string .id}}; Path=/"},
		{"a capture in part of a cookie's value", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=s:{{capture string .id}}; Path=/"}}), CheckOptions{}, whyCaptureNotCookie, "sid={{capture string .id}}; Path=/"},
		{"a capture in a cookie a later one of its identity replaces", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{capture string .id}}; Path=/,sid=S2; Path=/"}}), CheckOptions{}, whyCookieShadowed, "move the placeholder to the last cookie of that name"},
		{"a capture in an object key", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"{{capture string .id}}":1}`}), CheckOptions{}, whyCaptureInKey, "write {{string .id}} in the key, or capture the value it names"},
		{"a capture on a value a repeated key replaces", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}","id":"x"}`}), CheckOptions{}, whyShadowedKey, "remove the earlier occurrence"},
		{"a use on a value a repeated parent key replaces", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"a":{"id":"{{string .id}}"},"a":{}}`}), CheckOptions{}, whyShadowedKey, "remove the earlier occurrence"},
		{"a use in an object key a repeated key replaces", httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"m":{"{{string .id}}":1},"m":{}}`}), CheckOptions{}, whyShadowedKey, "remove the earlier occurrence"},
		{"a use in a frame's value a repeated key replaces", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Content-Type": "application/x-ndjson"}, Body: `{"u":"{{string .id}}","u":"x"}`}), CheckOptions{Streaming: func(*models.TestCase) bool { return true }}, whyShadowedKey, "remove the earlier occurrence"},
		{"a use in a multipart streamed body", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Content-Type": "multipart/mixed; boundary=XYZ"}, Body: "--XYZ\r\n\r\n{\"user\":\"{{string .id}}\"}\r\n--XYZ--"}), CheckOptions{Streaming: func(*models.TestCase) bool { return true }}, whyStreamNotFramed, `write its recorded value "1" here`},
		{"a capture in a multipart streamed body", httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Content-Type": "multipart/mixed; boundary=XYZ"}, Body: "--XYZ\r\n\r\n{\"user\":\"{{capture string .id}}\"}\r\n--XYZ--"}), CheckOptions{Streaming: func(*models.TestCase) bool { return true }}, whyCaptureStream, "assert the value with {{string .id}}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewRun(nil, map[string]interface{}{"id": "1", "x": "2"}, map[string]interface{}{"k": "s"})
			refused := r.Check([]*models.TestCase{c.tc}, c.opts).Refused
			require.Len(t, refused["t"], 1, "%v", refused)
			assert.True(t, strings.HasPrefix(refused["t"][0].Why, c.why), "%q\nwant it to start %q", refused["t"][0].Why, c.why)
			assert.Contains(t, refused["t"][0].Why, c.edit, "the edit to make, with the real key")
			assert.NotContains(t, refused["t"][0].Why, ".KEY", "the placeholder's own key, not a stand-in")
			assert.Contains(t, refused["t"][0].Error(), "t: ")
		})
	}
}

func TestCheck_AcceptsWhatCanRun(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1", "sid": "2", "n": "3", "xsrf": "4"})
	cases := []*models.TestCase{
		httpCase("create", models.HTTPReq{Body: `{"name":"a"}`}, models.HTTPResp{
			// One capture per cookie, two cookies in one Set-Cookie value.
			Header: map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/; Expires=Thu, 23 Oct 2026 10:00:00 GMT,XSRF-TOKEN={{capture string .xsrf}}; Path=/", "Location": "/u/{{string .id}}"},
			Body:   `{"id":"{{capture string .id}}","n":"{{capture int .n}}","ids":["{{capture string .x}}"],"href":"/u/{{string .id}}"}`,
		}),
		// The same key captured again in a later response: a token refresh.
		httpCase("refresh", models.HTTPReq{URL: "http://h/{{string .id}}", Header: map[string]string{"Cookie": "sid={{string .sid}}"}}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}),
	}
	got := r.Check(cases, CheckOptions{})
	assert.Empty(t, got.Refused)
	require.Len(t, got.Undefined, 1)
	assert.Equal(t, Undefined{Key: "x", TestCase: "create", Where: "response body.ids[0]", Placeholder: "{{capture string .x}}"}, got.Undefined[0])
	assert.Empty(t, got.Uncaptured, "every key a request sends was captured by an earlier test case")
	var where []string
	for _, p := range got.Places {
		where = append(where, p.TestCase+" "+p.Where+" "+p.Placeholder.Text)
	}
	assert.Equal(t, []string{
		"create response body.id {{capture string .id}}",
		"create response body.n {{capture int .n}}",
		"create response body.href {{string .id}}",
		"create response header Location {{string .id}}",
		"create response header Set-Cookie.sid {{capture string .sid}}",
		"create response header Set-Cookie.XSRF-TOKEN {{capture string .xsrf}}",
		"refresh response body.id {{capture string .id}}",
	}, where, "each variable place, for the replay to tell when noise covers one")
}

// Rule 9 / rule 6: only a placeholder whose key the set defines can be
// refused. Any other is text: a set without templates is refused nothing,
// whatever its traffic holds, and an undefined capture is named, not refused.
func TestCheck_RefusesOnlyPlaceholdersOfDefinedKeys(t *testing.T) {
	traffic := []*models.TestCase{
		httpCase("t", models.HTTPReq{URL: "http://h/{{capture string .id}}", Body: `{"yaml":"body: {{capture string .id}}"}`},
			models.HTTPResp{Body: `{"stored":"{{capture .id}} {{capture string .id}}","n":{{capture int .n}}}`, Header: map[string]string{"Set-Cookie": "a=x; Domain={{capture string .id}}"}}),
	}
	got := newRun(t, nil).Check(traffic, CheckOptions{IgnoreOrdering: true})
	assert.Empty(t, got.Refused, "a set with no templates is refused nothing")

	got = newRun(t, map[string]interface{}{"other": "1"}).Check(traffic, CheckOptions{})
	assert.Empty(t, got.Refused, "an undefined key is text, never refused")
	require.NotEmpty(t, got.Undefined)
	assert.Equal(t, "id", got.Undefined[0].Key, "and it is named")
}

// A map or a list in config.yaml refuses only the test cases whose
// placeholders name it, with the key named; the others run.
func TestCheck_RefusesTheUseOfAKeyWithAMapOrListValue(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"user": map[string]interface{}{"id": 1}, "id": "1"}, nil)
	got := r.Check([]*models.TestCase{
		httpCase("uses-it", models.HTTPReq{URL: "http://h/{{string .user}}"}, models.HTTPResp{}),
		httpCase("does-not", models.HTTPReq{URL: "http://h/{{string .id}}"}, models.HTTPResp{}),
	}, CheckOptions{})
	require.Len(t, got.Refused, 1)
	require.Len(t, got.Refused["uses-it"], 1)
	assert.Contains(t, got.Refused["uses-it"][0].Why, `"user"`)
	assert.Contains(t, got.Refused["uses-it"][0].Why, "map")
}

// Rule 6's warning: each undefined key is named once, at its first use; in a
// set with no templates, dot-only text such as {{ .Name }} is the app's. A
// key outside the grammar (dotted, with a '-') is named as such.
func TestCheck_NamesEachUndefinedKeyOnce(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1"})
	got := r.Check([]*models.TestCase{
		httpCase("a", models.HTTPReq{URL: "http://h/{{string .idd}}"}, models.HTTPResp{Body: `{"x":"{{string .idd}}"}`}),
		httpCase("b", models.HTTPReq{Body: `{"tpl":"{{ .Name }}","u":"{{string .user.id}}"}`}, models.HTTPResp{}),
	}, CheckOptions{})
	require.Len(t, got.Undefined, 3)
	assert.Equal(t, "idd", got.Undefined[0].Key)
	assert.Equal(t, "request url", got.Undefined[0].Where)
	assert.Equal(t, Undefined{Key: "user.id", TestCase: "b", Where: "request body", Placeholder: "{{string .user.id}}", OutsideGrammar: true}, got.Undefined[1])
	assert.Equal(t, "Name", got.Undefined[2].Key)

	got = newRun(t, nil).Check([]*models.TestCase{
		httpCase("b", models.HTTPReq{Body: `{"tpl":"{{ .Name }}","x":"{{ .Subscriber.Name }}"}`}, models.HTTPResp{Body: `{"id":"{{string .id}}"}`}),
	}, CheckOptions{})
	require.Len(t, got.Undefined, 1, "with no templates only a typed placeholder is named")
	assert.Equal(t, "id", got.Undefined[0].Key)
}

// A key a request sends that no earlier test case captures is named: every
// run sends config.yaml's value there (what a set templatized by an older
// keploy, which wrote a use where the value is produced, now does).
func TestCheck_NamesAKeyARequestSendsThatNothingCapturedBefore(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_REC", "host": "h", "tok": "T0"})
	got := r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{URL: "http://{{string .host}}/users", Header: map[string]string{"Authorization": "Bearer {{string .tok}}"}}, models.HTTPResp{Body: `{"id":"{{string .id}}","tok":"{{capture string .tok}}"}`}),
		httpCase("read", models.HTTPReq{URL: "http://h/users/{{string .id}}", Header: map[string]string{"Authorization": "Bearer {{string .tok}}"}}, models.HTTPResp{}),
	}, CheckOptions{})
	var keys []string
	for _, u := range got.Uncaptured {
		keys = append(keys, u.TestCase+":"+u.Key)
	}
	assert.Equal(t, []string{"create:host", "create:tok", "read:id"}, keys, "tok is captured by create before read sends it")
}

// A clear-then-set login captures in the cookie that stays, and cookies of one
// name and two Paths are two cookies: neither is refused.
func TestCheck_ACookieIsOneByItsNameDomainAndPath(t *testing.T) {
	r := newRun(t, map[string]interface{}{"sid": "1", "sidAdmin": "2"})
	cases := []*models.TestCase{
		httpCase("clear-then-set", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=; Path=/; Max-Age=0,sid={{capture string .sid}}; Path=/"}}),
		httpCase("two-paths", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/,sid={{capture string .sidAdmin}}; Path=/admin"}}),
	}
	assert.Empty(t, r.Check(cases, CheckOptions{}).Refused)
}

// An object key's placeholder is read like any other: an undefined key is
// named, and a use is a variable place.
func TestCheck_ReadsPlaceholdersInObjectKeys(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1"})
	res := r.Check([]*models.TestCase{
		httpCase("t", models.HTTPReq{Body: `{"{{string .idd}}":true}`}, models.HTTPResp{Body: `{"{{string .id}}":{"n":1},"{{string .other}}":2}`}),
	}, CheckOptions{})
	assert.Empty(t, res.Refused)
	var undefined []string
	for _, u := range res.Undefined {
		undefined = append(undefined, u.Key+" at "+u.Where)
	}
	assert.Equal(t, []string{"idd at request body", "other at response body key {{string .other}}"}, undefined)
	require.Len(t, res.Places, 1)
	assert.Equal(t, "response body key {{string .id}}", res.Places[0].Where)
	assert.Equal(t, []string{"{{string .id}}"}, res.Places[0].KeyPath())
}

// A gRPC response's uses are variable places, so the noise warning can name an
// entry over one.
func TestCheck_RecordsTheVariablePlacesOfAGRPCResponse(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1"})
	tc := &models.TestCase{Name: "g", Kind: models.GRPC_EXPORT,
		GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-id": "{{string .id}}"}}},
		GrpcResp: models.GrpcResp{
			Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-user": "{{string .id}}"}},
			Body:    models.GrpcLengthPrefixedMessage{DecodedData: `1: "{{string .id}}"`},
		}}
	places := r.Check([]*models.TestCase{tc}, CheckOptions{Streaming: func(*models.TestCase) bool { return true }}).Places
	require.Len(t, places, 2)
	assert.Equal(t, Place{TestCase: "g", Where: "response header x-user", Placeholder: places[0].Placeholder, Header: "x-user", GRPC: true}, places[0])
	assert.Equal(t, "response body", places[1].Where)
	assert.True(t, places[1].GRPC)
}

// A NESTED CREATE: a header capture may sit beside uses of keys its response
// does not capture (the text around it is then known before the answer is
// read), never beside a use of a key the response captures, and a header
// value holds one capture.
func TestCheck_AHeaderCaptureMaySitBesideUsesOfOtherKeys(t *testing.T) {
	r := newRun(t, map[string]interface{}{"org": "o", "users": "u", "x": "1", "id": "2"})
	got := r.Check([]*models.TestCase{httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "/orgs/{{string .org}}/users/{{capture string .users}}"}})}, CheckOptions{})
	assert.Empty(t, got.Refused)
	assert.Len(t, got.Places, 2)

	got = r.Check([]*models.TestCase{httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "/{{capture string .x}}/{{capture string .id}}"}})}, CheckOptions{})
	require.Len(t, got.Refused["t"], 2)
	for i, f := range got.Refused["t"] {
		assert.True(t, strings.HasPrefix(f.Why, whyCaptureTwiceInValue), f.Why)
		// Each refusal names its own capture at its recorded value.
		assert.Contains(t, f.Why, []string{`{{capture string .x}} as "1"`, `{{capture string .id}} as "2"`}[i])
	}
}

// A COOKIE ANOTHER OF ITS IDENTITY REPLACES holds no placeholder, a use
// included, and the identity is read with each use at its recorded value, as
// a run reads it at its own.
func TestCheck_ACookieAnotherOfItsIdentityReplacesHoldsNoPlaceholder(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1", "x": "2"})
	got := r.Check([]*models.TestCase{httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{capture string .id}}; Path=/{{string .x}},sid=stale; Path=/2"}})}, CheckOptions{})
	var refused []string
	for _, f := range got.Refused["t"] {
		refused = append(refused, f.Placeholder)
		assert.Equal(t, whyCookieShadowed, f.Why)
	}
	assert.Equal(t, []string{"{{capture string .id}}", "{{string .x}}"}, refused)
}

// A SET TEMPLATIZED BY AN OLDER KEPLOY wrote a use where the value is issued.
// The warning for the key a later request sends names that place and the
// exact edit: a capture there, of the use's type, in quotes where a JSON
// value is written bare.
func TestCheck_NamesTheEditThatMakesAnOlderProducerCapture(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1", "n": "7", "sid": "S0"})
	got := r.Check([]*models.TestCase{
		httpCase("test-1", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{string .sid}}; Path=/"}, Body: `{"id":"{{string .id}}","n":{{int .n}}}`}),
		// A list that holds the id after it was issued: not where it is issued.
		httpCase("list", models.HTTPReq{URL: "http://h/users"}, models.HTTPResp{Body: `{"ids":"{{string .id}}"}`}),
		httpCase("test-2", models.HTTPReq{URL: "http://h/users/{{string .id}}", Header: map[string]string{"Cookie": "sid={{string .sid}}"}, Body: `{"n":"{{int .n}}"}`}, models.HTTPResp{}),
	}, CheckOptions{})
	fixes := map[string]string{}
	for _, u := range got.Uncaptured {
		fixes[u.Key] = u.Fix
	}
	const constant = "; if it is a constant or what the request sent: write "
	assert.Equal(t, map[string]string{
		"id":  "if the app issues this value anew each run: change {{string .id}} at test-1/response body.id to {{capture string .id}}" + constant + `id's recorded value in config.yaml's template in place of every placeholder of id`,
		"n":   `if the app issues this value anew each run: change {{int .n}} at test-1/response body.n to "{{capture int .n}}"` + constant + `n's recorded value in config.yaml's template in place of every placeholder of n`,
		"sid": "if the app issues this value anew each run: change {{string .sid}} at test-1/response header Set-Cookie.sid to {{capture string .sid}}" + constant + `sid's recorded value in config.yaml's template in place of every placeholder of sid`,
	}, fixes)
}

// A streamed response's places are its headers' and its frames': the noise
// warning can name an entry over either. Its flat body, which the stream
// comparison does not read, has none, and a frame never captures.
func TestCheck_RecordsTheVariablePlacesOfAStreamedResponse(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1"})
	tc := httpCase("s", models.HTTPReq{}, models.HTTPResp{
		Header: map[string]string{"X-User": "{{string .id}}"},
		Body:   "flat {{string .id}}",
		StreamBody: []models.HTTPStreamChunk{
			{Data: []models.HTTPStreamDataField{{Key: "event", Value: "user"}, {Key: "data", Value: `{"user":"{{string .id}}"}`}}},
			{Data: []models.HTTPStreamDataField{{Key: "raw", Value: "id={{capture string .id}}"}}},
			{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"next":"{{capture string .id}}"}`}}},
		},
	})
	res := r.Check([]*models.TestCase{tc}, CheckOptions{Streaming: func(*models.TestCase) bool { return true }})
	require.Len(t, res.Refused["s"], 2)
	for _, f := range res.Refused["s"] {
		assert.True(t, strings.HasPrefix(f.Why, whyCaptureStream), f.Why)
		assert.Contains(t, f.Why, "assert the value with {{string .id}}")
	}
	assert.Equal(t, "response body frame 1", res.Refused["s"][0].Where)
	assert.Equal(t, "response body frame 2 data.next", res.Refused["s"][1].Where)
	require.Len(t, res.Places, 2)
	assert.Equal(t, "response body frame 0 data.user", res.Places[0].Where)
	assert.True(t, res.Places[0].Stream)
	assert.Equal(t, []string{"user"}, res.Places[0].KeyPath())
	assert.Equal(t, Place{TestCase: "s", Where: "response header X-User", Placeholder: res.Places[1].Placeholder, Header: "X-User"}, res.Places[1])
}

// Placeholder-shaped text whose key is not a key of the grammar is named when
// the set loads, as such, also when config.yaml has that key.
func TestCheck_NamesAKeyOutsideTheGrammar(t *testing.T) {
	r := newRun(t, map[string]interface{}{"user-id": "usr_1", "id": "1"})
	got := r.Check([]*models.TestCase{httpCase("t", models.HTTPReq{URL: "http://h/users/{{string .user-id}}"}, models.HTTPResp{Body: `{"u":"{{capture string .user-id}}"}`})}, CheckOptions{})
	assert.Empty(t, got.Refused, "text is never refused")
	require.Len(t, got.Undefined, 1, "named once")
	assert.Equal(t, Undefined{Key: "user-id", TestCase: "t", Where: "request url", Placeholder: "{{string .user-id}}", OutsideGrammar: true}, got.Undefined[0])
}

// A FLAT STREAMED BODY, which a recording kept unsplit, is read frame by
// frame as the run reads it (StreamFrames): its uses are places of their
// frames, so a noise entry over one is named.
func TestCheck_AFlatStreamedBodyHasPlacesInItsFrames(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "1"})
	tc := httpCase("s", models.HTTPReq{}, models.HTTPResp{
		Header: map[string]string{"Content-Type": "application/x-ndjson"},
		Body:   `{"n":1}` + "\n" + `{"user":"{{string .id}}"}`,
	})
	res := r.Check([]*models.TestCase{tc}, CheckOptions{Streaming: func(*models.TestCase) bool { return true }})
	require.Empty(t, res.Refused)
	require.Len(t, res.Places, 1)
	assert.Equal(t, "response body frame 1.user", res.Places[0].Where)
	assert.True(t, res.Places[0].Stream)
}

// A SECRET IS NOT A VARIABLE (keploy sanitize's redaction of a literal): it
// is no place a run asserts on its own, so no noise warning names it, and a
// shape a variable is refused in (a replaced cookie, a replaced JSON value,
// a multipart stream) holds a secret as the literal it was. A set sanitized
// but never templatized is refused nothing.
func TestCheck_ASecretIsNotAVariable(t *testing.T) {
	r := NewRun(nil, nil, map[string]interface{}{"Jwt": "eyJ.rec.sig"})
	stream := func(*models.TestCase) bool { return true }
	cases := []*models.TestCase{
		httpCase("login", models.HTTPReq{Header: map[string]string{"Authorization": "Bearer {{string .secret.Jwt }}"}}, models.HTTPResp{
			Header: map[string]string{"X-Auth-Token": "{{string .secret.Jwt }}", "Set-Cookie": "sid={{string .secret.Jwt}}; Path=/,sid=fresh; Path=/"},
			Body:   `{"access_token":"{{string .secret.Jwt }}","access_token":"x","m":{"{{string .secret.Jwt}}":1},"k":{"{{string .secret.Jwt}}":1},"k":{}}`,
		}),
	}
	res := r.Check(cases, CheckOptions{})
	assert.Empty(t, res.Refused)
	assert.Empty(t, res.Places)
	assert.Empty(t, res.Uncaptured)
	multipart := httpCase("m", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Content-Type": "multipart/mixed; boundary=XYZ"}, Body: `--XYZ {"t":"{{string .secret.Jwt}}"} --XYZ--`})
	res = r.Check([]*models.TestCase{multipart}, CheckOptions{Streaming: stream})
	assert.Empty(t, res.Refused)
	assert.Empty(t, res.Places)
}

// A CAPTURE IN A URL-VALUED HEADER whose recorded value holds a delimiter of
// the part it stands in (a '/' in a path segment; a '&', '=' or '#' in a
// query value) is refused: no answer's part can hold it.
func TestCheck_RefusesAURLCaptureOfAValueItsPartCannotHold(t *testing.T) {
	for _, c := range []struct {
		name, location, value string
		refused               bool
	}{
		{"a '/' in a path segment", "/files/{{capture string .f}}", "dir/f", true},
		{"a '&' in a query value", "/cb?code={{capture string .f}}", "x&y", true},
		{"a '=' in a query value", "/cb?code={{capture string .f}}&s=1", "x=y", true},
		{"a '&' in a path segment", "/files/{{capture string .f}}", "x&y", false},
		{"a '/' in a query value", "/cb?next={{capture string .f}}", "a/b", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRun(t, map[string]interface{}{"f": c.value})
			res := r.Check([]*models.TestCase{httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": c.location}})}, CheckOptions{})
			if !c.refused {
				assert.Empty(t, res.Refused)
				return
			}
			require.Len(t, res.Refused["t"], 1)
			assert.Contains(t, res.Refused["t"][0].Why, "the URL is read by its parts")
		})
	}
	r := newRun(t, map[string]interface{}{"f": "dir/f"})
	assert.Empty(t, r.Check([]*models.TestCase{httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"X-File": "{{capture string .f}}"}})}, CheckOptions{}).Refused, "a header that is not a URL is text")
}

// THE NEXT STEP OF A TEMPLATE FAILURE names both things it can mean: the app
// issues the value anew each run (the place that issues it must capture it,
// named with its edit), or the value changed on purpose (edit the expected
// value, or re-record). It never says to run keploy normalize.
func TestCheckResult_NextStepNamesBothCases(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1", "n": "7", "tok": "T0"})
	res := r.Check([]*models.TestCase{
		httpCase("test-1", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{string .id}}","n":{{int .n}}}`}),
		httpCase("test-2", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{Body: `{"id":"{{string .id}}","tok":"{{capture string .tok}}"}`}),
		httpCase("test-3", models.HTTPReq{}, models.HTTPResp{Body: `{"tok":"{{string .tok}}","n":"{{int .n}}"}`}),
	}, CheckOptions{})
	changed := "if the value changed on purpose, edit the expected value at "
	for _, c := range []struct {
		name, testCase string
		f              Failure
		canCapture     bool
		want           []string
	}{
		{"a use nothing captures, an older producer before it", "test-2", Failure{Where: "body.id", Placeholder: "{{string .id}}"}, true,
			[]string{"make the producer a capture: change {{string .id}} at test-1/response body.id to {{capture string .id}}", changed + "test-2/response body.id or re-record the test case"}},
		{"the producer itself", "test-1", Failure{Where: "body.n", Placeholder: "{{int .n}}"}, true,
			[]string{`change {{int .n}} at test-1/response body.n to "{{capture int .n}}"`, changed + "test-1/response body.n"}},
		{"a use of a key an earlier case captured", "test-3", Failure{Where: "body.tok", Placeholder: "{{string .tok}}"}, true,
			[]string{"this run's value of tok is what test-2/response body.tok captured",
				"capture it under a key of its own: change {{string .tok}} at test-3/response body.tok to {{capture string .tok2}}, give tok2 tok's recorded value in config.yaml's template",
				"if this place replaces tok from then on (a refreshed session cookie), change {{string .tok}} there to {{capture string .tok}} instead, which points every later use of tok at this answer",
				changed + "test-3/response body.tok"}},
		{"a use in a case that cannot capture", "test-3", Failure{Where: "body.tok", Placeholder: "{{string .tok}}"}, false,
			[]string{"this run's value of tok is what test-2/response body.tok captured", changed}},
		{"a use of a key its own response captures", "test-2", Failure{Where: "body.id2", Placeholder: "{{string .tok}}"}, true,
			[]string{"this run's value of tok is what test-2/response body.tok captured from this same answer", changed + "test-2/response body.id2"}},
		{"a capture that found nothing", "test-2", Failure{Where: "body.tok", Placeholder: "{{capture string .tok}}"}, true,
			[]string{"the answer has no value of the capture's type there", changed + "test-2/response body.tok"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := res.NextStep(c.testCase, c.f, c.canCapture)
			for _, w := range c.want {
				assert.Contains(t, got, w)
			}
			assert.NotContains(t, got, "normalize")
			if !c.canCapture {
				assert.NotContains(t, got, "{{capture")
			}
		})
	}
	same := res.NextStep("test-2", Failure{Where: "body.id2", Placeholder: "{{string .tok}}"}, true)
	assert.NotContains(t, same, "{{capture", "a second capture of a key its own response captures would contradict the first")
	assert.NotContains(t, same, "nothing captures")
}

// A VARIABLE IN A NAME OR A gRPC TRAILER is refused (rule 9): a header's or a
// form field's name is matched as written, so a variable there would be
// neither sent at its value nor asserted, and the gRPC comparison reads only
// grpc-status of the trailers. The refusal gives the name to write, at its
// recorded value. A secret there is the literal the recording held: a set
// keploy sanitize wrote is refused nothing.
func TestCheck_RefusesAVariableInANameOrATrailer(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"tenant": "acme", "field": "email", "id": "7"}, map[string]interface{}{"K": "k1"})
	for _, c := range []struct {
		name, where, edit string
		tc                *models.TestCase
	}{
		{"a request header's name", "request header name X-{{string .tenant}}-Key", `write the name as recorded, "X-acme-Key"`,
			httpCase("t", models.HTTPReq{Header: map[string]string{"X-{{string .tenant}}-Key": "1"}}, models.HTTPResp{})},
		{"a form field's name", "request form field name {{string .field}}", `write the name as recorded, "email"`,
			httpCase("t", models.HTTPReq{Form: []models.FormData{{Key: "{{string .field}}", Values: []string{"a"}}}}, models.HTTPResp{})},
		{"a response header's name", "response header name X-{{string .id}}", `write the name as recorded, "X-7"`,
			httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"X-{{string .id}}": "1"}})},
		{"a capture in a response header's name", "response header name X-{{capture string .id}}", `write the name as recorded, "X-7"`,
			httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"X-{{capture string .id}}": "1"}})},
		{"a gRPC request header's name", "request header name x-{{string .tenant}}", `write the name as recorded, "x-acme"`,
			&models.TestCase{Name: "t", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-{{string .tenant}}": "1"}}}}},
		{"a gRPC response header's name", "response header name x-{{string .id}}", `write the name as recorded, "x-7"`,
			&models.TestCase{Name: "t", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-{{string .id}}": "1"}}}}},
		{"a gRPC response pseudo header's name", "response pseudo header name :x-{{string .id}}", `write the name as recorded, ":x-7"`,
			&models.TestCase{Name: "t", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":x-{{string .id}}": "200"}}}}},
		{"a gRPC trailer's value", "response trailer x-user", `write its recorded value "7" here`,
			&models.TestCase{Name: "t", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-user": "{{string .id}}"}}}}},
		{"a gRPC trailer's name", "response trailer name x-{{string .id}}", `write the name as recorded, "x-7"`,
			&models.TestCase{Name: "t", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{Trailers: models.GrpcHeaders{PseudoHeaders: map[string]string{"x-{{string .id}}": "1"}}}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			refused := r.Check([]*models.TestCase{c.tc}, CheckOptions{}).Refused["t"]
			require.Len(t, refused, 1, "%v", refused)
			assert.Equal(t, c.where, refused[0].Where)
			assert.Contains(t, refused[0].Why, c.edit)
		})
	}
	trailer := r.Check([]*models.TestCase{{Name: "t", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-user": "{{string .id}}"}}}}}, CheckOptions{}).Refused["t"]
	require.Len(t, trailer, 1)
	assert.True(t, strings.HasPrefix(trailer[0].Why, whyInTrailer), trailer[0].Why)

	sanitized := []*models.TestCase{
		httpCase("s", models.HTTPReq{Header: map[string]string{"X-{{string .secret.K}}": "1"}, Form: []models.FormData{{Key: "{{string .secret.K}}", Values: []string{"a"}}}},
			models.HTTPResp{Header: map[string]string{"X-{{string .secret.K}}": "1"}}),
		{Name: "g", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{Trailers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-{{string .secret.K}}": "{{string .secret.K}}"}}}},
	}
	assert.Empty(t, r.Check(sanitized, CheckOptions{}).Refused, "a secret in a name or a trailer is the recorded literal")
	untemplated := httpCase("t", models.HTTPReq{Header: map[string]string{"X-{{string .tenant}}-Key": "1"}}, models.HTTPResp{Header: map[string]string{"X-{{string .id}}": "1"}})
	assert.Empty(t, NewRun(nil, nil, nil).Check([]*models.TestCase{untemplated}, CheckOptions{}).Refused, "a set with no templates is refused nothing")
}

// A READ-BACK THAT FAILS (PL2): the test case's own request sent the key, so
// its response holding it says the app answers what it was asked for. When
// the answer holds another value, a capture there would accept whatever the
// app answers and send it on, so the next step never offers one: it says the
// app answered a different value than the one captured at the producer, and
// names both places to check. A use whose request did not send the key keeps
// the capture edit.
func TestCheckResult_NextStepOfAReadBackNeverOffersACapture(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "usr_1", "tok": "T0"})
	res := r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}),
		httpCase("get", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{Body: `{"id":"{{string .id}}"}`}),
		httpCase("list", models.HTTPReq{URL: "http://h/users"}, models.HTTPResp{Body: `{"first":"{{string .id}}"}`}),
		{Name: "g", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Body: models.GrpcLengthPrefixedMessage{DecodedData: `1: "{{string .tok}}"`}}},
	}, CheckOptions{})
	assert.Equal(t, map[string]map[string]bool{"get": {"id": true}, "g": {"tok": true}}, res.Sends, "the keys each request sends")

	readBack := res.NextStep("get", Failure{Where: "body.id", Placeholder: "{{string .id}}"}, true)
	assert.NotContains(t, readBack, "{{capture", "a capture on a read-back would accept any id and send it on")
	assert.Contains(t, readBack, "this test case's own request sent it")
	assert.Contains(t, readBack, "the app answered a different value than the one captured at create/response body.id")
	assert.Contains(t, readBack, "check the read-back at get/response body.id, or the producer at create/response body.id")
	assert.Contains(t, readBack, "edit the expected value at get/response body.id or re-record the test case")

	other := res.NextStep("list", Failure{Where: "body.first", Placeholder: "{{string .id}}"}, true)
	assert.Contains(t, other, "change {{string .id}} at list/response body.first to {{capture string .id2}}", "the request did not send it: the capture edit stays")

	// Every place a request sends a key makes its test case a read-back of
	// it: a header, a form value, a url_params value, the body. A test case
	// that sends another key only is no read-back of this one.
	for _, c := range []struct {
		name     string
		req      models.HTTPReq
		readBack bool
	}{
		{"a header", models.HTTPReq{URL: "http://h/me", Header: map[string]string{"X-User": "{{string .id}}"}}, true},
		{"a form value", models.HTTPReq{URL: "http://h/me", Form: []models.FormData{{Key: "user", Values: []string{"{{string .id}}"}}}}, true},
		{"a url_params value", models.HTTPReq{URL: "http://h/me", URLParams: map[string]string{"user": "{{string .id}}"}}, true},
		{"the body", models.HTTPReq{URL: "http://h/me", Body: `{"user":"{{string .id}}"}`}, true},
		{"another key only", models.HTTPReq{URL: "http://h/me", Header: map[string]string{"X-Tok": "{{string .tok}}"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := r.Check([]*models.TestCase{
				httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}),
				httpCase("read", c.req, models.HTTPResp{Body: `{"id":"{{string .id}}"}`}),
			}, CheckOptions{})
			next := res.NextStep("read", Failure{Where: "body.id", Placeholder: "{{string .id}}"}, true)
			if c.readBack {
				assert.True(t, res.Sends["read"]["id"], "the request sends id: %v", res.Sends)
				assert.NotContains(t, next, "{{capture", "a read-back is never offered a capture")
				assert.Contains(t, next, "this test case's own request sent it")
				return
			}
			assert.False(t, res.Sends["read"]["id"], "%v", res.Sends)
			assert.Contains(t, next, "change {{string .id}} at read/response body.id to {{capture string .id2}}", "the request sent another key only: the capture edit stays")
		})
	}
}

// A WHOLE LOCATION AN OLDER TEMPLATIZE WROTE ({{string .location}} with
// location "/jobs/j_REC1") is read by its parts, and a capture there would be
// refused: its recorded value holds a '/' of its path. Its advice gives only
// the edit that writes the value as recorded, and says to capture the value
// where a response gives it whole, if one does; a part that holds no delimiter keeps both
// edits.
func TestCheck_AdvisesNoCaptureWhereACaptureWouldBeRefused(t *testing.T) {
	r := newRun(t, map[string]interface{}{"location": "/jobs/j_REC1", "id": "j_REC1"})
	res := r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "{{string .location}}", "Content-Location": "/jobs/{{string .id}}"}}),
		httpCase("read", models.HTTPReq{URL: "http://h{{string .location}}?id={{string .id}}"}, models.HTTPResp{}),
	}, CheckOptions{})
	fixes := map[string]string{}
	for _, u := range res.Uncaptured {
		fixes[u.Key] = u.Fix
	}
	require.Contains(t, fixes, "location")
	assert.NotContains(t, fixes["location"], "{{capture", "the capture would be refused")
	assert.Contains(t, fixes["location"], `write location's recorded value in config.yaml's template in place of every placeholder of location`)
	assert.Contains(t, fixes["location"], "if a response also gives the value whole (a JSON value), capture it there")
	assert.Contains(t, fixes["id"], "change {{string .id}} at create/response header Content-Location to {{capture string .id}}", "a part with no delimiter can capture")

	// Following the edit it does not give would be refused.
	edited := r.Check([]*models.TestCase{httpCase("create", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "{{capture string .location}}"}})}, CheckOptions{})
	require.Len(t, edited.Refused["create"], 1)

	next := res.NextStep("create", Failure{Where: "header Location", Placeholder: "{{string .location}}"}, true)
	assert.NotContains(t, next, "{{capture")
	assert.Contains(t, next, "if a response also gives the value whole (a JSON value), capture it there")
	assert.Contains(t, next, `location's recorded value in config.yaml's template holds a delimiter of its part`)
}

// A NEXT STEP NAMES A RECORDED VALUE BY ITS KEY, never by the value: keploy
// test's WARN lines say each next step whole, and a recorded value is one the
// application sent (an id, an email, a URL of its own), which a line carries
// only as KEPLOY_LOG_CONTENT allows. Every next step that offers to write a
// value as recorded, or says why a capture cannot stand at it, points at
// config.yaml's template instead. A refusal's why, which the report keeps
// and a line logs by its length, still quotes it.
func TestCheck_ANextStepNamesARecordedValueByItsKey(t *testing.T) {
	const canary = "CANARY_8f3a"
	r := newRun(t, map[string]interface{}{
		"id": "u_" + canary, "location": "/jobs/" + canary, "loc": "/jobs/" + canary + "2", "q": "q_" + canary, "tok": "t_" + canary, "tok2": "taken",
	})
	res := r.Check([]*models.TestCase{
		// A producer an older templatize wrote, where a capture can stand,
		// and a whole Location, where none can.
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{string .id}}"}`, Header: map[string]string{"Location": "{{string .location}}"}}),
		// The one capture of loc is refused.
		httpCase("move", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "{{capture string .loc}}"}}),
		httpCase("login", models.HTTPReq{}, models.HTTPResp{Body: `{"tok":"{{capture string .tok}}"}`}),
		httpCase("read", models.HTTPReq{URL: "http://h{{string .location}}/{{string .id}}?q={{string .q}}", Body: `{"to":"{{string .loc}}"}`}, models.HTTPResp{Body: `{"tok":"{{string .tok}}"}`}),
	}, CheckOptions{})
	steps := map[string]string{}
	for _, u := range res.Uncaptured {
		steps["uncaptured "+u.Key] = u.UncapturedNextStep()
	}
	steps["delimiter"] = res.NextStep("create", Failure{Where: "header Location", Placeholder: "{{string .location}}"}, true)
	steps["own key"] = res.NextStep("read", Failure{Where: "body.tok", Placeholder: "{{string .tok}}"}, true)
	for _, k := range []string{"uncaptured id", "uncaptured location", "uncaptured loc", "uncaptured q"} {
		require.Contains(t, steps, k)
	}
	for k, next := range steps {
		assert.NotContains(t, next, canary, "the next step %s quotes a recorded value", k)
	}
	assert.Contains(t, steps["uncaptured id"], "write id's recorded value in config.yaml's template in place of every placeholder of id", "a producer's two edits")
	assert.Contains(t, steps["uncaptured location"], "write location's recorded value in config.yaml's template in place of every placeholder of location", "a producer where no capture stands")
	assert.Contains(t, steps["uncaptured loc"], "write loc's recorded value in config.yaml's template in place of every placeholder of loc", "a refused capture")
	assert.Contains(t, steps["uncaptured q"], "write q's recorded value in config.yaml's template in place of every placeholder of q", "no producer")
	assert.Contains(t, steps["delimiter"], "location's recorded value in config.yaml's template holds a delimiter of its part")
	assert.Contains(t, steps["own key"], "give tok3 tok's recorded value in config.yaml's template")

	// The refusal's why, for the report, quotes the value as before.
	require.Len(t, res.Refused["move"], 1)
	assert.Contains(t, res.Refused["move"][0].Why, `the recorded value "/jobs/`+canary+`2" holds "/"`)
}

// AN UNTYPED BARE PRODUCER an older templatize wrote ({"n":{{.n}}}, a number)
// is advised a capture of the JSON type it was recorded as, in quotes:
// "{{capture int .n}}", never a string capture that no JSON number satisfies.
func TestCheck_AnUntypedProducerIsAdvisedACaptureOfItsRecordedType(t *testing.T) {
	r := newRun(t, map[string]interface{}{"n": "7654321", "f": "2.5", "s": "abc"})
	res := r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"n":{{.n}},"f":{{.f}},"s":{{.s}}}`}),
		httpCase("read", models.HTTPReq{URL: "http://h/{{.n}}/{{.f}}/{{.s}}"}, models.HTTPResp{}),
	}, CheckOptions{})
	fixes := map[string]string{}
	for _, u := range res.Uncaptured {
		fixes[u.Key] = u.Fix
	}
	assert.Contains(t, fixes["n"], `to "{{capture int .n}}"`)
	assert.Contains(t, fixes["f"], `to "{{capture float .f}}"`)
	assert.Contains(t, fixes["s"], `to "{{capture string .s}}"`)
	next := res.NextStep("read", Failure{Where: "body.x", Placeholder: "{{.n}}"}, true)
	assert.Contains(t, next, `change {{.n}} at create/response body.n to "{{capture int .n}}"`)
}

// A NEXT STEP OFFERS A CAPTURE ONLY WHERE ONE CAN STAND (R7f, R7u, R7v). A use
// that fails where an earlier capture gave its value is offered the capture
// edit only at a place a capture would replace and the set check accepts: not
// inside an array under ignoreOrdering, in an object key, in part of a cookie,
// beside another placeholder of its JSON value, beside a capture or a use of
// a key its response captures in a header value. Where it is offered, a use
// written outside a JSON string gets the capture in quotes, and the edit
// applied passes the set check.
func TestCheckResult_NextStepOffersACaptureOnlyWhereOneCanStand(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "u_REC1", "n": "7654321", "own": "o_REC1"})
	create := httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}","n":"{{capture int .n}}"}`})
	for _, c := range []struct {
		name, where, placeholder string
		resp                     models.HTTPResp
		ignoreOrdering           bool
		capture                  string // the capture offered, "" for none
		why                      string
	}{
		{"R7f a use in an array under ignoreOrdering", "body.items[0].id", "{{string .id}}",
			models.HTTPResp{Body: `{"items":[{"id":"{{string .id}}"},{"id":"x"}]}`}, true, "", "inside an array and test.ignoreOrdering is on"},
		{"the same with ignoreOrdering off", "body.items[0].id", "{{string .id}}",
			models.HTTPResp{Body: `{"items":[{"id":"{{string .id}}"},{"id":"x"}]}`}, false, "{{capture string .id}}", ""},
		{"R7u a bare use", "body.last", "{{int .n}}",
			models.HTTPResp{Body: `{"last":{{int .n}}}`}, false, `"{{capture int .n}}"`, ""},
		{"R7v an object key", "body key roles.{{string .id}}", "{{string .id}}",
			models.HTTPResp{Body: `{"roles":{"{{string .id}}":"admin"}}`}, false, "", "an object key"},
		// A failure names a key by this run's value of it; the next step
		// names the place as written, where the edit goes.
		{"R7v an object key named by the run's value", "body key roles.u_LIVE2", "{{string .id}}",
			models.HTTPResp{Body: `{"roles":{"{{string .id}}":"admin"}}`}, false, "", "an object key"},
		{"part of a cookie", "header Set-Cookie.sid", "{{string .id}}",
			models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=v-{{string .id}}; Path=/"}}, false, "", "part of a cookie's value"},
		{"beside another placeholder of its JSON value", "body.pair", "{{string .id}}",
			models.HTTPResp{Body: `{"pair":"{{string .id}}:{{string .n}}"}`}, false, "", "holds other placeholders"},
		{"beside a capture in a header value", "header X-Pair", "{{string .id}}",
			models.HTTPResp{Header: map[string]string{"X-Pair": "{{string .id}}:{{capture string .own}}"}}, false, "", "holds a capture already"},
		{"beside a use of a key its response captures", "header X-Pair", "{{string .id}}",
			models.HTTPResp{Header: map[string]string{"X-Pair": "{{string .id}}:{{string .own}}"}, Body: `{"own":"{{capture string .own}}"}`}, false, "", "asserts own, which this response captures"},
		{"a whole header value", "header X-Id", "{{string .id}}",
			models.HTTPResp{Header: map[string]string{"X-Id": "{{string .id}}"}}, false, "{{capture string .id}}", ""},
		{"a header value beside a use of a key its response does not capture", "header X-Pair", "{{string .id}}",
			models.HTTPResp{Header: map[string]string{"X-Pair": "{{string .id}}:{{string .n}}"}}, false, "{{capture string .id}} (that header value holds other placeholders and one capture: change at most one of them)", ""},
		{"a body that is not JSON", "body", "{{string .id}}",
			models.HTTPResp{Body: "id={{string .id}}"}, false, "", "the body is not JSON"},
	} {
		t.Run(c.name, func(t *testing.T) {
			later := httpCase("later", models.HTTPReq{}, c.resp)
			res := r.Check([]*models.TestCase{create, later}, CheckOptions{IgnoreOrdering: c.ignoreOrdering})
			require.Empty(t, res.Refused)
			next := res.NextStep("later", Failure{Where: c.where, Placeholder: c.placeholder}, true)
			assert.Contains(t, next, "is what create/response body.")
			if c.capture == "" {
				assert.NotContains(t, next, "{{capture", "a capture there would be refused")
				written := strings.Replace(c.where, "u_LIVE2", "{{string .id}}", 1)
				assert.Contains(t, next, "a capture cannot stand at later/response "+written)
				assert.Contains(t, next, "edit the expected value at later/response "+written)
				assert.Contains(t, next, c.why)
				return
			}
			// Two edits: a capture under a key of its own (id2), and the
			// same key's capture for a place that replaces it from then on.
			fresh := strings.NewReplacer(".id}}", ".id2}}", ".n}}", ".n2}}").Replace(c.capture)
			same := strings.SplitN(c.capture, " (", 2)[0]
			assert.Contains(t, next, "change "+c.placeholder+" at later/response "+c.where+" to "+fresh)
			assert.Contains(t, next, "change "+c.placeholder+" there to "+same+" instead, which points every later use of")
			// The first edit applied, its key given its recorded value: the
			// set check refuses nothing.
			r := newRun(t, map[string]interface{}{"id": "u_REC1", "n": "7654321", "own": "o_REC1", "id2": "u_REC1", "n2": "7654321"})
			capture := strings.SplitN(fresh, " (", 2)[0]
			edited := c.resp
			edited.Header = map[string]string{}
			for k, v := range c.resp.Header {
				edited.Header[k] = strings.Replace(v, c.placeholder, strings.Trim(capture, `"`), 1)
			}
			old, repl := c.placeholder, strings.Trim(capture, `"`)
			if strings.HasPrefix(capture, `"`) {
				repl = capture
			}
			edited.Body = strings.Replace(c.resp.Body, old, repl, 1)
			assert.Empty(t, r.Check([]*models.TestCase{create, httpCase("later", models.HTTPReq{}, edited)}, CheckOptions{IgnoreOrdering: c.ignoreOrdering}).Refused, "the advised edit is accepted: %s", edited.Body)
		})
	}
}

// NEXTSTEP NAMES THE FAILING PLACE OF TWO THAT HOLD THE SAME PLACEHOLDER
// (R8T-10): each gets the edit its own place takes, a capture where one can
// stand and the reason where none can.
func TestCheckResult_NextStepNamesTheFailingPlaceOfTwo(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "u_REC1"})
	res := r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}),
		httpCase("stats", models.HTTPReq{URL: "http://h/stats"}, models.HTTPResp{Body: `{"last":"{{string .id}}","items":[{"id":"{{string .id}}"}]}`}),
	}, CheckOptions{IgnoreOrdering: true})
	require.Empty(t, res.Refused)
	last := res.NextStep("stats", Failure{Where: "body.last", Placeholder: "{{string .id}}"}, true)
	assert.Contains(t, last, "change {{string .id}} at stats/response body.last to {{capture string .id2}}")
	inArray := res.NextStep("stats", Failure{Where: "body.items[0].id", Placeholder: "{{string .id}}"}, true)
	assert.Contains(t, inArray, "a capture cannot stand at stats/response body.items[0].id: it is inside an array")
	assert.NotContains(t, inArray, "{{capture")
}

// A VALUE OF ITS OWN TAKES A KEY OF ITS OWN (R8f). A later response, not a
// read-back, that answers another value than a key's capture is advised two
// edits: a capture under a new key, for a place that issues a value of its
// own, and the same key's capture only for a place that replaces the value
// from then on (a refreshed session cookie), which re-points every later use.
// The first, applied, keeps the read-back after it sending and asserting the
// id its producer issued; the same key's capture would have sent this place's.
func TestCheckResult_NextStepCapturesAValueOfItsOwnUnderANewKey(t *testing.T) {
	create := httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`})
	read := httpCase("read", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{Body: `{"id":"{{string .id}}"}`})
	stats := func(body string) *models.TestCase {
		return httpCase("stats", models.HTTPReq{URL: "http://h/stats"}, models.HTTPResp{Body: body})
	}
	r := newRun(t, map[string]interface{}{"id": "u_REC1", "id2": "taken"})
	res := r.Check([]*models.TestCase{create, stats(`{"last":"{{string .id}}"}`), read}, CheckOptions{})
	next := res.NextStep("stats", Failure{Where: "body.last", Placeholder: "{{string .id}}"}, true)
	assert.Contains(t, next, `capture it under a key of its own: change {{string .id}} at stats/response body.last to {{capture string .id3}}, give id3 id's recorded value in config.yaml's template, and write {{string .id3}} where later requests send this place's value`, "id2 is taken")
	// A key config.yaml defines with a map or a list value is taken too
	// (TR9-11): the new key is never one config.yaml holds.
	mapped := newRun(t, map[string]interface{}{"id": "u_REC1", "id2": map[string]interface{}{"a": 1}})
	res = mapped.Check([]*models.TestCase{create, stats(`{"last":"{{string .id}}"}`), read}, CheckOptions{})
	next = res.NextStep("stats", Failure{Where: "body.last", Placeholder: "{{string .id}}"}, true)
	assert.Contains(t, next, `to {{capture string .id3}}, give id3 id's recorded value in config.yaml's template`, "id2 is defined, as a map")
	assert.Contains(t, next, "if this place replaces id from then on (a refreshed session cookie), change {{string .id}} there to {{capture string .id}} instead, which points every later use of id at this answer")

	sendsRead := func(capture string) string {
		run := newRun(t, map[string]interface{}{"id": "u_REC1", "id2": "taken", "id3": "u_REC1"})
		cases := []*models.TestCase{create, stats(`{"last":"` + capture + `"}`), read}
		require.Empty(t, run.Check(cases, CheckOptions{}).Refused)
		assert.Empty(t, run.Expected(create.HTTPResp, models.HTTPResp{Body: `{"id":"u_LIVE2"}`}, ExpectOptions{}).Failures)
		assert.Empty(t, run.Expected(cases[1].HTTPResp, models.HTTPResp{Body: `{"last":"u_OTHER"}`}, ExpectOptions{}).Failures)
		return run.Request(read.HTTPReq).URL
	}
	assert.Equal(t, "http://h/users/u_LIVE2", sendsRead("{{capture string .id3}}"), "the read-back still reads the created id")
	assert.Equal(t, "http://h/users/u_OTHER", sendsRead("{{capture string .id}}"), "the same key's capture re-points it")
}

// THE ADVICE NAMES THE PRODUCER keploy templatize WOULD (R8n, R8T-07). In a
// set an older keploy templatized, a redirect's Location and the Set-Cookie
// of its response both hold a use of the session id: templatize passes over
// a URL-valued header outside a 201's Location for a later place of the same
// response, so the advice names the cookie, never a capture inside the
// redirect, whatever the letter case of the Location's name. In a 201, whose
// Location may capture, the Location comes first by name whatever its case.
func TestCheck_TheAdviceNamesTheProducerTemplatizeWould(t *testing.T) {
	fixOf := func(t *testing.T, res CheckResult, key string) string {
		t.Helper()
		for _, u := range res.Uncaptured {
			if u.Key == key {
				return u.UncapturedNextStep()
			}
		}
		t.Fatalf("%s is not uncaptured: %+v", key, res.Uncaptured)
		return ""
	}
	r := newRun(t, map[string]interface{}{"sid": "s_REC1", "id": "u_REC1"})
	for _, loc := range []string{"Location", "location", "LOCATION"} {
		res := r.Check([]*models.TestCase{
			httpCase("login", models.HTTPReq{}, models.HTTPResp{StatusCode: 302, Header: map[string]string{loc: "/home?sid={{string .sid}}", "Set-Cookie": "sid={{string .sid}}; Path=/"}}),
			httpCase("me", models.HTTPReq{Header: map[string]string{"Cookie": "sid={{string .sid}}"}}, models.HTTPResp{Body: `{"sid":"{{string .sid}}"}`}),
		}, CheckOptions{})
		fix := fixOf(t, res, "sid")
		assert.Contains(t, fix, "change {{string .sid}} at login/response header Set-Cookie.sid to {{capture string .sid}}", loc)
		assert.NotContains(t, fix, "header "+loc+" to {{capture", loc)
		var redirect *Place
		for i := range res.Places {
			if res.Places[i].Where == "response header "+loc {
				redirect = &res.Places[i]
			}
		}
		require.NotNil(t, redirect, loc)
		assert.True(t, redirect.Producer && redirect.PassedOver, "%s: %+v", loc, redirect)
		next := res.NextStep("me", Failure{Where: "body.sid", Placeholder: "{{string .sid}}"}, true)
		assert.Contains(t, next, "make the producer a capture: change {{string .sid}} at login/response header Set-Cookie.sid to {{capture string .sid}}", loc)
	}
	// Only a part of a URL-valued header outside a 201's Location is passed
	// over, and a later place that is passed over too does not replace it.
	res := r.Check([]*models.TestCase{
		httpCase("move", models.HTTPReq{}, models.HTTPResp{StatusCode: 302, Header: map[string]string{"Content-Location": "/s/{{string .sid}}", "Location": "/home?sid={{string .sid}}"}}),
		httpCase("me", models.HTTPReq{Header: map[string]string{"Cookie": "sid={{string .sid}}"}}, models.HTTPResp{}),
	}, CheckOptions{})
	assert.Contains(t, fixOf(t, res, "sid"), "change {{string .sid}} at move/response header Content-Location to {{capture string .sid}}")

	// The producer is in the first response holding the key, as templatize
	// reads it: a later response's place does not replace it.
	res = r.Check([]*models.TestCase{
		httpCase("login", models.HTTPReq{}, models.HTTPResp{StatusCode: 302, Header: map[string]string{"Location": "/home?sid={{string .sid}}"}}),
		httpCase("session", models.HTTPReq{}, models.HTTPResp{Body: `{"sid":"{{string .sid}}"}`}),
		httpCase("me", models.HTTPReq{Header: map[string]string{"Cookie": "sid={{string .sid}}"}}, models.HTTPResp{}),
	}, CheckOptions{})
	assert.Contains(t, fixOf(t, res, "sid"), "at login/response header Location to {{capture string .sid}} (a capture in that header checks only that a value stands there, so any target passes there, a regression included)")
	// The next step that offers a capture in a redirect, the only place of
	// its response holding the key, says what it gives up.
	next := res.NextStep("session", Failure{Where: "body.sid", Placeholder: "{{string .sid}}"}, true)
	assert.Contains(t, next, "make the producer a capture: change {{string .sid}} at login/response header Location to {{capture string .sid}} (a capture in that header checks only that a value stands there, so any target passes there, a regression included)")
	// So does the two-edit advice for a redirect that answers another value
	// than a key's capture (P10, Q10): a capture there accepts any target.
	res = r.Check([]*models.TestCase{
		httpCase("login", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/"}}),
		httpCase("hop", models.HTTPReq{}, models.HTTPResp{StatusCode: 302, Header: map[string]string{"Location": "/home?sid={{string .sid}}", "Set-Cookie": "sid={{string .sid}}; Path=/"}}),
	}, CheckOptions{})
	next = res.NextStep("hop", Failure{Where: "header Location", Placeholder: "{{string .sid}}"}, true)
	assert.Contains(t, next, "change {{string .sid}} at hop/response header Location to {{capture string .sid2}} (a capture in that header checks only that a value stands there, so any target passes there, a regression included), give sid2 sid's recorded value in config.yaml's template")
	next = res.NextStep("hop", Failure{Where: "header Set-Cookie.sid", Placeholder: "{{string .sid}}"}, true)
	assert.NotContains(t, next, "any target passes", "a cookie is no target")

	for _, loc := range []string{"Location", "location"} {
		res := r.Check([]*models.TestCase{
			httpCase("signup", models.HTTPReq{}, models.HTTPResp{StatusCode: 201, Header: map[string]string{loc: "/users/{{string .id}}", "Set-Cookie": "id={{string .id}}; Path=/"}}),
			httpCase("read", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{}),
		}, CheckOptions{})
		assert.Contains(t, fixOf(t, res, "id"), "change {{string .id}} at signup/response header "+loc+" to {{capture string .id}}", loc)
	}

	// Only a 201's Location may capture: a 200's or a 202's is passed over
	// like a redirect's (TR9-06).
	for _, status := range []int{200, 202} {
		res := r.Check([]*models.TestCase{
			httpCase("login", models.HTTPReq{}, models.HTTPResp{StatusCode: status, Header: map[string]string{"Location": "/home?sid={{string .sid}}", "Set-Cookie": "sid={{string .sid}}; Path=/"}}),
			httpCase("me", models.HTTPReq{Header: map[string]string{"Cookie": "sid={{string .sid}}"}}, models.HTTPResp{Body: `{"sid":"{{string .sid}}"}`}),
		}, CheckOptions{})
		assert.Contains(t, fixOf(t, res, "sid"), "change {{string .sid}} at login/response header Set-Cookie.sid to {{capture string .sid}}", "%d", status)
		next := res.NextStep("me", Failure{Where: "body.sid", Placeholder: "{{string .sid}}"}, true)
		assert.Contains(t, next, "change {{string .sid}} at login/response header Set-Cookie.sid to {{capture string .sid}}", "%d", status)
	}

	// A place where a capture would be refused (inside an array under
	// ignoreOrdering) is passed over too, for a later place of its response
	// that can capture, in the set-load advice and in the next step alike
	// (P7: TR9-07, TR9-08).
	res = r.Check([]*models.TestCase{
		httpCase("list", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "id={{string .id}}; Path=/"}, Body: `{"items":[{"id":"{{string .id}}"}]}`}),
		httpCase("read", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{Body: `{"id":"{{string .id}}"}`}),
	}, CheckOptions{IgnoreOrdering: true})
	fix := fixOf(t, res, "id")
	assert.Contains(t, fix, "change {{string .id}} at list/response header Set-Cookie.id to {{capture string .id}}")
	assert.NotContains(t, fix, "a capture cannot stand")
	next = res.NextStep("read", Failure{Where: "body.id", Placeholder: "{{string .id}}"}, true)
	assert.Contains(t, next, "make the producer a capture: change {{string .id}} at list/response header Set-Cookie.id to {{capture string .id}}")
}

// A USE IN A COOKIE'S ATTRIBUTES IS NOT PART OF ITS VALUE: the reason a
// capture cannot stand there says where it is.
func TestCheck_ACookieAttributeUseIsNotPartOfItsValue(t *testing.T) {
	r := newRun(t, map[string]interface{}{"id": "u_REC1"})
	res := r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}),
		httpCase("later", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=v-{{string .id}}; Path=/u/{{string .id}}"}}),
	}, CheckOptions{})
	var whys []string
	for _, pl := range res.Places {
		if pl.TestCase == "later" {
			whys = append(whys, pl.NoCaptureWhy)
		}
	}
	assert.Equal(t, []string{noCaptureCookiePart, noCaptureCookieAttribute}, whys)

	// A use in a cookie's name is outside its value too (TR9-13), and no
	// place: it is refused (TestCheck_RefusesAVariableInACookiesNameOrFlag).
	res = r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"id":"{{capture string .id}}"}`}),
		httpCase("later", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "u_{{string .id}}=1; Path=/"}}),
	}, CheckOptions{})
	for _, pl := range res.Places {
		assert.NotEqual(t, "later", pl.TestCase, "a use in a cookie's name is no place: %+v", pl)
	}
	require.Len(t, res.Refused["later"], 1)
	assert.True(t, strings.HasPrefix(res.Refused["later"][0].Why, whyInCookieName), res.Refused["later"][0].Why)
}

// A VARIABLE IN A COOKIE'S NAME, AN ATTRIBUTE'S NAME OR A FLAG IS REFUSED. A
// run reads a cookie's value and its attributes' values (the answer's cookie
// of the same identity, then each attribute that has a value), so a use in
// the cookie's name, in an attribute's name or as an attribute with no value
// (HttpOnly) was never asserted: under a noise entry over Set-Cookie any
// answer passed while keploy test said the variable was asserted there, and
// without one a cookie's name failed the answer the recording held. The set
// check refuses it with the edit. So too a use whose recorded value, written
// at its place, splits the value into other cookies or attributes (a ';', a
// line break, a ',' that starts a name=): its place is not the one a run
// reads. So too a use in an attribute a later one of its name overrides (a
// run reads the last, as a user agent keeps it), and a use whose recorded
// value has a space at the edge of its part (a run reads the part without
// it); placeholders side by side are read together, each at its recorded
// value. A value's '=' and an Expires date's ',' split nothing; a secret is
// the literal it redacts; a key the set does not define is text, named.
func TestCheck_RefusesAVariableInACookiesNameOrFlag(t *testing.T) {
	template := map[string]interface{}{"flag": "HttpOnly", "an": "SameSite", "pn": "Path", "n": "sid", "id": "u_REC1", "c": "sid=abc", "semi": "a;b", "nl": "a\nb", "b64": "dG9r==", "exp": "Thu, 01 Jan 2026 00:00:00 GMT", "v": "abc",
		"p": "zz", "ss": "Strict", "sp": " abc", "tsp": "/p ", "e": "", "cm": "a,", "eq": "b=1", "t": " "}
	secrets := map[string]interface{}{"k": "HttpOnly"}
	for _, c := range []struct {
		name, value string
		refused     []string
		why, edit   string
	}{
		{"a flag (F1-F4)", "sid=abc; {{string .flag}}", []string{"{{string .flag}}"}, whyInCookieName, `write its recorded value "HttpOnly" here`},
		{"an attribute's name (G4, G5)", "sid=abc; {{string .an}}=Lax", []string{"{{string .an}}"}, whyInCookieName, `write its recorded value "SameSite" here`},
		{"Path's name (G1-G3)", "sid=abc; {{string .pn}}=/x", []string{"{{string .pn}}"}, whyInCookieName, `write its recorded value "Path" here`},
		{"the cookie's name (H1-H3)", "{{string .n}}=abc; Path=/", []string{"{{string .n}}"}, whyInCookieName, `write its recorded value "sid" here`},
		{"part of the cookie's name", "u_{{string .id}}=1; Path=/", []string{"{{string .id}}"}, whyInCookieName, `write its recorded value "u_REC1" here`},
		{"a flag of the second cookie", "a=1; Path=/,sid=abc; {{string .flag}}", []string{"{{string .flag}}"}, whyInCookieName, `write its recorded value "HttpOnly" here`},
		{"a value that starts a cookie after a comma (SC15, SC16)", "a=1; Path=/, {{string .c}}", []string{"{{string .c}}"}, whyCookieSplit, `write its recorded value "sid=abc" here`},
		{"a cookie value holding a ';'", "sid={{string .semi}}; Path=/", []string{"{{string .semi}}"}, whyCookieSplit, `write its recorded value "a;b" here`},
		{"an attribute value holding a line break", "sid=abc; Path=/{{string .nl}}", []string{"{{string .nl}}"}, whyCookieSplit, `write its recorded value "a\nb" here`},
		{"a cookie value holding '=': read as a cookie's", "sid={{string .b64}}; Path=/", nil, "", ""},
		{"an Expires date's comma: read as an attribute's", "sid=abc; Expires={{string .exp}}", nil, "", ""},
		{"a cookie's value and an attribute's value: read as a cookie's", "sid={{string .v}}; Path=/{{string .v}}", nil, "", ""},
		{"a secret as a flag: the literal it redacts", "sid=abc; {{string .secret.k}}", nil, "", ""},
		// An attribute a later one of its name overrides: a run reads the last
		// (round-13 verdicts review, RA1 to RA4).
		{"an attribute a later one of its name overrides (RA1)", "sid=abc; X={{string .p}}; X=lit", []string{"{{string .p}}"}, "a later X attribute of this cookie overrides this one", `move the placeholder to the last X attribute of this cookie, or write its recorded value "zz" here`},
		{"an attribute a later one overrides in another letter case (RA2)", "sid=abc; SameSite={{string .ss}}; samesite=Lax", []string{"{{string .ss}}"}, "a later SameSite attribute of this cookie overrides this one", `write its recorded value "Strict" here`},
		{"the only attribute of its name: read as an attribute's (RA3)", "sid=abc; X={{string .p}}", nil, "", ""},
		{"the last attribute of its name: read as an attribute's (RA4)", "sid=abc; X=lit; X={{string .p}}", nil, "", ""},
		// A recorded value with a space at the edge of its part: a run reads
		// the part without it (round-13 verdicts review, S1).
		{"a cookie value with a leading space (S1)", "sid={{string .sp}}; Path=/", []string{"{{string .sp}}"}, whyCookieSpace, `write its recorded value " abc" here`},
		{"an attribute value with a trailing space", "sid=abc; Path={{string .tsp}}", []string{"{{string .tsp}}"}, whyCookieSpace, `write its recorded value "/p " here`},
		{"a space inside the value: read as a cookie's", "sid=a{{string .sp}}; Path=/", nil, "", ""},
		{"an empty recorded value: read as a cookie's (S5)", "sid={{string .e}}; Path=/", nil, "", ""},
		// Placeholders side by side are read together (round-14 verdicts
		// review's fuzz: 21 of 8,614 accepted templates failed their own
		// recorded answer): neither of these splits the value alone.
		{"two placeholders that split the value together", "sid=x{{string .cm}}{{string .eq}}; Path=/", []string{"{{string .cm}}", "{{string .eq}}"}, whyCookieSplit, `write its recorded value "`},
		{"a space and an empty value at the part's edge together", "sid=x{{string .t}}{{string .e}}", []string{"{{string .t}}", "{{string .e}}"}, whyCookieSpace, `write its recorded value "`},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := NewRun(nil, template, secrets).Check([]*models.TestCase{
				httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": c.value}}),
			}, CheckOptions{})
			var got []string
			for _, f := range res.Refused["t"] {
				got = append(got, f.Placeholder)
				assert.True(t, strings.HasPrefix(f.Where, "response header Set-Cookie."), f.Where)
				assert.True(t, strings.HasPrefix(f.Why, c.why), f.Why)
				assert.Contains(t, f.Why, c.edit)
			}
			assert.Equal(t, c.refused, got)
			for _, pl := range res.Places {
				for _, r := range c.refused {
					assert.NotEqual(t, r, pl.Placeholder.Text, "a refused use is no place a run asserts: %+v", pl)
				}
			}
		})
	}
	// A key the set does not define is text: named, never refused.
	res := NewRun(nil, template, nil).Check([]*models.TestCase{
		httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "sid=abc; {{string .other}}"}}),
	}, CheckOptions{})
	assert.Empty(t, res.Refused)
	require.Len(t, res.Undefined, 1)
	assert.Equal(t, "other", res.Undefined[0].Key)
}

// THE ADVICE FOR A KEY NOTHING CAPTURES NEVER NAMES A REFUSED EDIT, AND ALWAYS
// OFFERS THE ONE THAT WRITES THE VALUE AS RECORDED (R7g, T20b, R7s, R7q).
func TestCheck_TheAdviceForAnUncapturedKey(t *testing.T) {
	fixOf := func(t *testing.T, res CheckResult, key string) string {
		t.Helper()
		for _, u := range res.Uncaptured {
			if u.Key == key {
				return u.UncapturedNextStep()
			}
		}
		t.Fatalf("%s is not uncaptured: %+v", key, res.Uncaptured)
		return ""
	}
	r := newRun(t, map[string]interface{}{"id": "u_REC1", "location": "/jobs/j_REC1", "a": "a_REC1", "b": "b_REC1"})

	// R7g: the older producer sits in an array under ignoreOrdering, where a
	// capture is refused.
	res := r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Body: `{"items":[{"id":"{{string .id}}"}]}`}),
		httpCase("read", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{}),
	}, CheckOptions{IgnoreOrdering: true})
	fix := fixOf(t, res, "id")
	assert.NotContains(t, fix, "{{capture")
	assert.Contains(t, fix, "a capture cannot stand at create/response body.items[0].id (it is inside an array and test.ignoreOrdering is on")
	assert.Contains(t, fix, `write id's recorded value in config.yaml's template in place of every placeholder of id`)

	// T20b: the one capture of the key is refused.
	res = r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Location": "{{capture string .location}}"}}),
		httpCase("poll", models.HTTPReq{Body: `{"url":"{{string .location}}"}`}, models.HTTPResp{}),
	}, CheckOptions{})
	require.Len(t, res.Refused["create"], 1)
	fix = fixOf(t, res, "location")
	assert.NotContains(t, fix, "mark that place {{capture", "the capture it would advise is the one refused")
	assert.Contains(t, fix, "the capture {{capture string .location}} at create/response header Location is refused when the set loads")
	assert.Contains(t, fix, `write location's recorded value in config.yaml's template in place of every placeholder of location`)

	// R7s: no earlier response holds the key (the client chose it): the
	// literal edit is offered beside the capture.
	res = r.Check([]*models.TestCase{httpCase("read", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{})}, CheckOptions{})
	fix = fixOf(t, res, "id")
	assert.Contains(t, fix, "mark that place {{capture string .id}}")
	assert.Contains(t, fix, `if it is a constant or what the request sent, write id's recorded value in config.yaml's template in place of every placeholder of id`)

	// R7q: one header value holds two uses; it holds one capture, so the
	// advice says at most one of them becomes one.
	res = r.Check([]*models.TestCase{
		httpCase("pair", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"X-Pair": "{{string .a}}:{{string .b}}"}}),
		httpCase("read", models.HTTPReq{URL: "http://h/x/{{string .a}}/{{string .b}}"}, models.HTTPResp{}),
	}, CheckOptions{})
	for _, k := range []string{"a", "b"} {
		fix = fixOf(t, res, k)
		assert.Contains(t, fix, "to {{capture string ."+k+"}} (that header value holds other placeholders and one capture: change at most one of them)")
	}
	both := r.Check([]*models.TestCase{httpCase("pair", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"X-Pair": "{{capture string .a}}:{{capture string .b}}"}})}, CheckOptions{})
	assert.NotEmpty(t, both.Refused["pair"], "both captures are refused, which is why the advice says one")
	// The next step of a later use names that producer the same way.
	res = r.Check([]*models.TestCase{
		httpCase("pair", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"X-Pair": "{{string .a}}:{{string .b}}"}}),
		httpCase("later", models.HTTPReq{}, models.HTTPResp{Body: `{"a":"{{string .a}}"}`}),
	}, CheckOptions{})
	next := res.NextStep("later", Failure{Where: "body.a", Placeholder: "{{string .a}}"}, true)
	assert.Contains(t, next, "make the producer a capture: change {{string .a}} at pair/response header X-Pair to {{capture string .a}} (that header value holds other placeholders and one capture: change at most one of them)")

	// A capture refused in a request is no producer: the advice is where to
	// put one.
	res = r.Check([]*models.TestCase{
		httpCase("create", models.HTTPReq{Body: `{"id":"{{capture string .id}}"}`}, models.HTTPResp{}),
		httpCase("read", models.HTTPReq{URL: "http://h/users/{{string .id}}"}, models.HTTPResp{}),
	}, CheckOptions{})
	fix = fixOf(t, res, "id")
	assert.NotContains(t, fix, "is refused when the set loads")
	assert.Contains(t, fix, "mark that place {{capture string .id}}")
}

// TEMPLATE TEXT OUTSIDE THE GRAMMAR IS NAMED, AN EXPRESSION INCLUDED. A set
// an older keploy replayed may hold an expression it rendered ({{ .id | print
// }}): keploy test sends and compares it as written, and the set check names
// it once, as an expression, wherever it stands: in a request, in a response
// body that holds nothing else, in a streamed frame. One that holds a double
// quote, which the older keploy read as text too, is named alike. A secret's
// expression is no secret. In a set without templates untyped text is the
// app's own (a mail template) and is not named.
func TestCheck_NamesAnExpressionAsTextOutsideTheGrammar(t *testing.T) {
	streaming := CheckOptions{Streaming: func(tc *models.TestCase) bool { return tc.Name == "stream" }}
	frame := httpCase("stream", models.HTTPReq{}, models.HTTPResp{StreamBody: []models.HTTPStreamChunk{{Data: []models.HTTPStreamDataField{{Key: "data", Value: `{"p":"{{ .price | print }}"}`}}}}})
	for _, c := range []struct {
		name     string
		template map[string]interface{}
		tc       *models.TestCase
		want     []Undefined
	}{
		{"in a request URL", map[string]interface{}{"id": "u_1"},
			httpCase("t", models.HTTPReq{URL: "http://h/users/{{ .id | print }}"}, models.HTTPResp{}),
			[]Undefined{{Key: "id | print", TestCase: "t", Where: "request url", Placeholder: "{{ .id | print }}", OutsideGrammar: true, Expression: true}}},
		{"only in a response body", map[string]interface{}{"price": "9.5"},
			httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"p":"{{ .price | print }}"}`}),
			[]Undefined{{Key: "price | print", TestCase: "t", Where: "response body.p", Placeholder: "{{ .price | print }}", OutsideGrammar: true, Expression: true}}},
		{"only in a response body that is not JSON", map[string]interface{}{"price": "9.5"},
			httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `price: {{ .price | print }}`}),
			[]Undefined{{Key: "price | print", TestCase: "t", Where: "response body", Placeholder: "{{ .price | print }}", OutsideGrammar: true, Expression: true}}},
		{"only in a streamed frame", map[string]interface{}{"price": "9.5"}, frame,
			[]Undefined{{Key: "price | print", TestCase: "stream", Where: "response body frame 0 data.p", Placeholder: "{{ .price | print }}", OutsideGrammar: true, Expression: true}}},
		{"one that holds a double quote", map[string]interface{}{"price": "9.5"},
			httpCase("t", models.HTTPReq{Header: map[string]string{"X-Price": `{{ float .price | printf "%.2f" }}`}}, models.HTTPResp{}),
			[]Undefined{{Key: `price | printf "%.2f"`, TestCase: "t", Where: "request header X-Price", Placeholder: `{{ float .price | printf "%.2f" }}`, OutsideGrammar: true, Expression: true}}},
		{"a pipe written without spaces", map[string]interface{}{"id": "u_1"},
			httpCase("t", models.HTTPReq{URL: "http://h/users/{{.id|len}}"}, models.HTTPResp{}),
			[]Undefined{{Key: "id|len", TestCase: "t", Where: "request url", Placeholder: "{{.id|len}}", OutsideGrammar: true, Expression: true}}},
		{"a bare dot", map[string]interface{}{"id": "u_1"},
			httpCase("t", models.HTTPReq{Body: `{{#items}}{{.}}{{/items}}`}, models.HTTPResp{}),
			[]Undefined{{Key: "", TestCase: "t", Where: "request body", Placeholder: "{{.}}", OutsideGrammar: true, Expression: true}}},
		{"a secret's expression", map[string]interface{}{"id": "u_1"},
			httpCase("t", models.HTTPReq{URL: "http://h/{{ .secret.k | urlquery }}"}, models.HTTPResp{}),
			[]Undefined{{Key: "secret.k | urlquery", TestCase: "t", Where: "request url", Placeholder: "{{ .secret.k | urlquery }}", OutsideGrammar: true, Expression: true}}},
		{"named once, beside a key outside the grammar", map[string]interface{}{"id": "u_1"},
			httpCase("t", models.HTTPReq{URL: "http://h/{{ .id | print }}/{{.a.b}}", Body: `{{ .id | print }}`}, models.HTTPResp{}),
			[]Undefined{
				{Key: "id | print", TestCase: "t", Where: "request url", Placeholder: "{{ .id | print }}", OutsideGrammar: true, Expression: true},
				{Key: "a.b", TestCase: "t", Where: "request url", Placeholder: "{{.a.b}}", OutsideGrammar: true},
			}},
		{"a call with an argument (MX36)", map[string]interface{}{"id": "u_1"},
			httpCase("t", models.HTTPReq{URL: "http://h/users/{{.id 2}}"}, models.HTTPResp{}),
			[]Undefined{{Key: "id 2", TestCase: "t", Where: "request url", Placeholder: "{{.id 2}}", OutsideGrammar: true, Expression: true}}},
		{"in the text of a Set-Cookie value that is in no cookie (MX38)", map[string]interface{}{"id": "u_1"},
			httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "{{string .user-id}}; Path=/"}}),
			[]Undefined{{Key: "user-id", TestCase: "t", Where: "response header Set-Cookie", Placeholder: "{{string .user-id}}", OutsideGrammar: true}}},
		{"in an assertion (MX39)", map[string]interface{}{"id": "u_1"},
			func() *models.TestCase {
				tc := httpCase("t", models.HTTPReq{}, models.HTTPResp{})
				tc.Assertions = map[models.AssertionType]interface{}{models.HeaderEqual: map[string]interface{}{"X-User": "{{string .user-id}}"}}
				return tc
			}(),
			[]Undefined{{Key: "user-id", TestCase: "t", Where: "assertions header_equal.X-User", Placeholder: "{{string .user-id}}", OutsideGrammar: true}}},
		{"untyped, in a set without templates: the app's text", nil,
			httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"tpl":"{{ .Values.tag | quote }} {{.}}"}`}), nil},
		{"typed, in a set without templates", nil,
			httpCase("t", models.HTTPReq{}, models.HTTPResp{Body: `{"p":"{{ float .price | print }}"}`}),
			[]Undefined{{Key: "price | print", TestCase: "t", Where: "response body.p", Placeholder: "{{ float .price | print }}", OutsideGrammar: true, Expression: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := NewRun(nil, c.template, nil).Check([]*models.TestCase{c.tc}, streaming)
			assert.Equal(t, c.want, res.Undefined)
			assert.Empty(t, res.Refused, "text is never refused")
			assert.Empty(t, res.Places, "text is no variable place")
		})
	}
}

// A VARIABLE OF A Set-Cookie VALUE THAT STANDS IN NO COOKIE IS REFUSED. A run
// reads a Set-Cookie value cookie by cookie (name=value, then attributes), so
// a use or a capture in a part with no name= (the whole value one
// placeholder; a first part that is one) has no place it could be captured
// or asserted at: before, the set check recorded none for it, and under a
// noise entry over the header any answer passed. The refusal names both
// edits. A cookie's own variables are read as before, a secret is the literal
// it redacts, and a key the set does not define is text, named and never
// refused.
func TestCheck_RefusesAVariableOfASetCookieValueInNoCookie(t *testing.T) {
	template := map[string]interface{}{"c": "sid=abc", "sid": "s_1"}
	secrets := map[string]interface{}{"k": "s"}
	for _, c := range []struct {
		name, value string
		refused     []string
		edit        string
	}{
		{"the whole value one use", "{{string .c}}", []string{"{{string .c}}"}, `write it as a cookie's whole value (name={{string .c}}), or write its recorded value "sid=abc" here`},
		{"a first part that is one use", "{{string .sid}}; Path=/", []string{"{{string .sid}}"}, `write it as a cookie's whole value (name={{string .sid}}), or write its recorded value "s_1" here`},
		{"a capture there", "{{capture string .sid}}; Path=/", []string{"{{capture string .sid}}"}, `write it as a cookie's whole value (name={{capture string .sid}})`},
		{"a line with no name after a cookie's", "sid=abc; Path=/\n{{string .sid}}", []string{"{{string .sid}}"}, `name={{string .sid}}`},
		{"text before the first cookie", "; {{string .sid}}, a=1", []string{"{{string .sid}}"}, `name={{string .sid}}`},
		{"a cookie's value, its attribute and its name: read as a cookie's", "sid={{string .sid}}; Path=/{{string .c}}", nil, ""},
		{"a secret in no cookie: the literal it redacts", "{{string .secret.k}}; Path=/", nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := NewRun(nil, template, secrets).Check([]*models.TestCase{
				httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": c.value}}),
			}, CheckOptions{})
			var got []string
			for _, f := range res.Refused["t"] {
				got = append(got, f.Placeholder)
				assert.Equal(t, "response header Set-Cookie", f.Where)
				assert.True(t, strings.HasPrefix(f.Why, whyOutsideCookie), f.Why)
				assert.Contains(t, f.Why, c.edit)
			}
			assert.Equal(t, c.refused, got)
		})
	}
	// A key the set does not define is text: named, never refused.
	res := NewRun(nil, template, nil).Check([]*models.TestCase{
		httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"Set-Cookie": "{{string .other}}; Path=/"}}),
	}, CheckOptions{})
	assert.Empty(t, res.Refused)
	require.Len(t, res.Undefined, 1)
	assert.Equal(t, Undefined{Key: "other", TestCase: "t", Where: "response header Set-Cookie", Placeholder: "{{string .other}}"}, res.Undefined[0])
}

// A VARIABLE IN AN ASSERTION IS REFUSED. The comparison reads a test case's
// assertions as written, never at a key's value (an earlier keploy rendered
// the whole test case, assertions included), so header_equal: {X-User:
// "{{string .id}}"} passed only an app that answers the placeholder's text.
// The set check refuses it, wherever in the assertions it stands (a value, a
// name, a list, a noise entry), with the edit; a secret there and a key the
// set does not define are text, and a set without templates is refused
// nothing.
func TestCheck_RefusesAVariableInAnAssertion(t *testing.T) {
	template := map[string]interface{}{"id": "u_REC1"}
	withAssertions := func(a map[models.AssertionType]interface{}) *models.TestCase {
		tc := httpCase("t", models.HTTPReq{}, models.HTTPResp{Header: map[string]string{"X-User": "u_REC1"}})
		tc.Assertions = a
		return tc
	}
	for _, c := range []struct {
		name       string
		assertions map[models.AssertionType]interface{}
		where      string
		holder     string
	}{
		{"a header_equal value", map[models.AssertionType]interface{}{"header_equal": map[string]interface{}{"X-User": "{{string .id}}"}}, "assertions header_equal.X-User", "{{string .id}}"},
		{"a json_contains value, nested", map[models.AssertionType]interface{}{"json_contains": map[models.AssertionType]interface{}{"user": map[models.AssertionType]interface{}{"id": "{{string .id}}"}}}, "assertions json_contains.user.id", "{{string .id}}"},
		{"a name", map[models.AssertionType]interface{}{"header_equal": map[string]interface{}{"X-{{string .id}}": "1"}}, "assertions header_equal.X-{{string .id}}", "{{string .id}}"},
		{"an element of a list", map[models.AssertionType]interface{}{"header_exists": []interface{}{"X-A", "X-{{string .id}}"}}, "assertions header_exists[1]", "{{string .id}}"},
		{"a noise entry's pattern", map[models.AssertionType]interface{}{"noise": map[models.AssertionType]interface{}{"body.id": []interface{}{"^{{string .id}}$"}}}, "assertions noise.body.id[0]", "{{string .id}}"},
		{"a capture", map[models.AssertionType]interface{}{"json_equal": `{"id":"{{capture string .id}}"}`}, "assertions json_equal", "{{capture string .id}}"},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := NewRun(nil, template, nil).Check([]*models.TestCase{withAssertions(c.assertions)}, CheckOptions{})
			require.Len(t, res.Refused["t"], 1, "%v", res.Refused)
			f := res.Refused["t"][0]
			assert.Equal(t, c.where, f.Where)
			assert.Equal(t, c.holder, f.Placeholder)
			assert.Equal(t, whyInAssertion+`: write its recorded value "u_REC1" here`, f.Why)
			assert.Empty(t, res.Places, "an assertion holds no variable place")
		})
	}
	// A gRPC test case's assertions are read the same way: its noise entries
	// are stored there, and a pattern is matched as written.
	grpcCase := &models.TestCase{Name: "g", Kind: models.GRPC_EXPORT, Assertions: map[models.AssertionType]interface{}{"noise": map[models.AssertionType]interface{}{"body.id": []interface{}{"^{{string .id}}$"}}}}
	refused := NewRun(nil, template, nil).Check([]*models.TestCase{grpcCase}, CheckOptions{}).Refused
	require.Len(t, refused["g"], 1, "%v", refused)
	assert.Equal(t, "assertions noise.body.id[0]", refused["g"][0].Where)

	// So are a streamed test case's, read after the others (founder
	// decision): a variable there is refused with the same edit.
	streamed := withAssertions(map[models.AssertionType]interface{}{"header_equal": map[string]interface{}{"X-User": "{{string .id}}"}})
	streamed.Name = "s"
	refused = NewRun(nil, template, nil).Check([]*models.TestCase{streamed}, CheckOptions{Streaming: func(*models.TestCase) bool { return true }}).Refused
	require.Len(t, refused["s"], 1, "%v", refused)
	assert.Equal(t, "assertions header_equal.X-User", refused["s"][0].Where)
	assert.Equal(t, whyInAssertion+`: write its recorded value "u_REC1" here`, refused["s"][0].Why)

	text := map[models.AssertionType]interface{}{"header_equal": map[string]interface{}{"X-Key": "{{string .secret.k}}", "X-Other": "{{string .other}}"}, "status_code": 200}
	res := NewRun(nil, template, map[string]interface{}{"k": "s"}).Check([]*models.TestCase{withAssertions(text)}, CheckOptions{})
	assert.Empty(t, res.Refused, "a secret and an undefined key are text")
	require.Len(t, res.Undefined, 1)
	assert.Equal(t, "assertions header_equal.X-Other", res.Undefined[0].Where)
	plain := NewRun(nil, nil, nil).Check([]*models.TestCase{withAssertions(map[models.AssertionType]interface{}{"header_equal": map[string]interface{}{"X-User": "{{ .Name }}"}})}, CheckOptions{})
	assert.Empty(t, plain.Refused)
	assert.Empty(t, plain.Undefined)
}

// A PLACE'S ADVICE IS ITS OWN. Why a capture cannot stand at a use is recorded
// on the place just recorded for it, and a secret is no place: before, the
// reason for a secret's neighbour was written onto whatever place came last,
// an earlier test case's at a place of the same name included, and that
// place's next step lost its capture edit.
func TestCheck_ASecretsNeighbourDoesNotChangeAnEarlierPlacesAdvice(t *testing.T) {
	r := NewRun(nil, map[string]interface{}{"tok": "t_1", "x": "2"}, map[string]interface{}{"s": "sec"})
	first := httpCase("test-1", models.HTTPReq{}, models.HTTPResp{Body: `{"auth":"{{string .tok}}"}`})
	alone := r.Check([]*models.TestCase{first}, CheckOptions{})
	require.Len(t, alone.Places, 1)
	require.Equal(t, "", alone.Places[0].NoCaptureWhy)
	res := r.Check([]*models.TestCase{
		first,
		httpCase("test-2", models.HTTPReq{}, models.HTTPResp{Body: `{"auth":"{{string .secret.s}} {{string .x}}"}`}),
	}, CheckOptions{})
	require.Len(t, res.Places, 2)
	assert.Equal(t, alone.Places[0], res.Places[0], "test-1's place is as it is without test-2")
	assert.Equal(t, "test-2", res.Places[1].TestCase)
	assert.Equal(t, noCaptureNotAlone, res.Places[1].NoCaptureWhy)
}
