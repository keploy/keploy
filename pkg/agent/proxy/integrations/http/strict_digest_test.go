package http

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/matcher"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// bodiesSame is the strict comparison's reading of two bodies of a recording
// with no noise of its own (bodyDiffs), under keploy.yml's body noise.
func bodiesSame(recBody, liveBody string, bodyNoise map[string][]string) bool {
	m := recorded("m", "/v1/orders", map[string]string{"Content-Type": "application/json"}, recBody)
	k := strictKnown{n: strictNoise{body: matcher.NewNoiseLookup(bodyNoise)}, values: util.NewNoiseChecker(m.Noise)}
	return len(k.bodyDiffs(m.Spec.HTTPReq, sent("/v1/orders", nil, liveBody))) == 0
}

// digestsSame reports whether the two bodies' digests are one, and whether
// either has none.
func digestsSame(recBody, liveBody string, bodyNoise map[string][]string) (same, none bool) {
	n := strictNoise{body: matcher.NewNoiseLookup(bodyNoise)}
	m := recorded("m", "/v1/orders", map[string]string{"Content-Type": "application/json"}, recBody)
	rd, rok := recordedBodyDigest(m, n)
	ld, lok := bodyDigest([]byte(liveBody), n)
	return rok && lok && rd == ld, !rok || !lok
}

// A JSON number's canonical text is its value: two numbers have one text
// exactly when they have one value (sameScalar).
func TestCanonicalNumberIsTheNumbersValue(t *testing.T) {
	for _, group := range [][]string{
		{"1.5", "1.50", "15e-1", "0.15E1", "0.15e+1", "150e-2"},
		{"100", "1e2", "1E+2", "100.000", "10e1", "0.001e5"},
		{"0", "-0", "0.0", "0e10", "-0.000e-3"},
		{"-12.5", "-125e-1", "-12.50"},
		{"12345678901234567890", "12345678901234567890.0", "1.234567890123456789e19"},
	} {
		first, ok := canonicalNumber(group[0])
		if !ok {
			t.Fatalf("%s has no canonical text", group[0])
		}
		for _, s := range group[1:] {
			if c, ok := canonicalNumber(s); !ok || c != first {
				t.Fatalf("%s reads %q (ok %v), %s reads %q", s, c, ok, group[0], first)
			}
			if !sameScalar(json.Number(s), json.Number(group[0])) {
				t.Fatalf("fixture: sameScalar(%s, %s) is false", s, group[0])
			}
		}
	}
	for _, pair := range [][2]string{{"1.5", "1.51"}, {"1", "-1"}, {"10", "1"}, {"1e2", "1e3"}, {"0.1", "1"}} {
		a, _ := canonicalNumber(pair[0])
		b, _ := canonicalNumber(pair[1])
		if a == b {
			t.Fatalf("%s and %s read as one value %q", pair[0], pair[1], a)
		}
	}
	if _, ok := canonicalNumber("1e99999999999999999999"); ok {
		t.Fatal("an exponent beyond 62 bits has a canonical text")
	}
}

// The digest is a necessary condition of the strict comparison's body
// equality: two bodies bodyDiffs takes for one have one digest (key order,
// a number's spelling, white space, an escape, keploy.yml's request-body
// noise, member or element, with what it names absent on one side), and
// bodies that differ outside that noise have two.
func TestABodyDigestIsOneForBodiesTheStrictComparisonTakesForOne(t *testing.T) {
	declared := map[string][]string{"ts": {}, "items[].at": {}, "trace": {}}
	for _, tc := range []struct {
		name, rec, live string
		noise           map[string][]string
		same            bool
	}{
		{"the body", `{"id":1,"a":"x"}`, `{"id":1,"a":"x"}`, nil, true},
		{"key order", `{"id":1,"a":"x"}`, `{"a":"x","id":1}`, nil, true},
		{"white space", `{"id":1, "a": "x"}`, ` {"id":1,"a":"x"}`, nil, true},
		{"a number's spelling", `{"amount":1.50,"n":100}`, `{"amount":1.5,"n":1e2}`, nil, true},
		{"an escape", `{"a":"A"}`, `{"a":"A"}`, nil, true},
		{"nested", `{"o":{"p":[1,{"q":null}],"b":true}}`, `{"o":{"b":true,"p":[1,{"q":null}]}}`, nil, true},
		{"a value", `{"id":1}`, `{"id":2}`, nil, false},
		{"a type", `{"id":1}`, `{"id":"1"}`, nil, false},
		{"a member more", `{"id":1}`, `{"id":1,"b":2}`, nil, false},
		{"array order", `{"l":[1,2]}`, `{"l":[2,1]}`, nil, false},
		{"array length", `{"l":[1]}`, `{"l":[1,1]}`, nil, false},
		{"a key's case", `{"id":1}`, `{"ID":1}`, nil, false},
		{"declared noise", `{"id":1,"ts":5,"items":[{"id":"a","at":1}],"user":{"trace":"t1"}}`, `{"id":1,"ts":9,"items":[{"id":"a","at":7}],"user":{"trace":"t2"}}`, declared, true},
		{"declared noise absent", `{"id":1,"ts":5}`, `{"id":1}`, declared, true},
		{"declared element noise", `{"tags":["a","b"],"id":1}`, `{"tags":["c","d"],"id":1}`, map[string][]string{"tags[]": {}}, true},
		{"declared element noise keeps the length", `{"tags":["a","b"],"id":1}`, `{"tags":["c"],"id":1}`, map[string][]string{"tags[]": {}}, false},
		{"declared root element noise", `[{"a":1},{"a":2}]`, `[{"a":3},{"a":4}]`, map[string][]string{"[]": {}}, true},
		{"declared noise around a change", `{"id":1,"ts":5}`, `{"id":2,"ts":9}`, declared, false},
		{"not JSON", `a=1&b=2`, `a=1&b=2`, nil, true},
		{"not JSON, changed", `a=1&b=2`, `a=1&b=3`, nil, false},
		{"JSON and not", `{"a":1}`, `{"a":1`, nil, false},
		{"empty", ``, ``, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bodiesSame(tc.rec, tc.live, tc.noise); got != tc.same {
				t.Fatalf("fixture: the strict comparison reads them as one: %v, want %v", got, tc.same)
			}
			same, none := digestsSame(tc.rec, tc.live, tc.noise)
			if none {
				t.Fatal("a body has no digest")
			}
			if same != tc.same {
				t.Fatalf("one digest: %v, want %v", same, tc.same)
			}
		})
	}
}

// Over many bodies and spellings of them, no two bodies the strict comparison
// takes for one have two digests: a lookup that skips a recording of another
// digest never skips the call's recording.
func TestABodyDigestNeverSkipsTheCallsRecording(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	noises := []map[string][]string{nil, {"ts": {}}, {"items[].at": {}}, {"o.p": {}}, {"items": {}}}
	same, apart := 0, 0
	for i := 0; i < 4000; i++ {
		v := randomJSON(r, 3)
		rec := spell(r, v)
		live := spell(r, v)
		if r.Intn(3) == 0 {
			live = spell(r, mutate(r, v))
		}
		noise := noises[r.Intn(len(noises))]
		if !bodiesSame(rec, live, noise) {
			if one, _ := digestsSame(rec, live, noise); !one {
				apart++
			}
			continue
		}
		same++
		if one, none := digestsSame(rec, live, noise); !one && !none {
			t.Fatalf("the strict comparison takes %s for %s (noise %v), but their digests differ", live, rec, noise)
		}
	}
	// The property is not vacuous, and the digest narrows.
	if same < 1000 || apart < 300 {
		t.Fatalf("fixture: %d pairs taken for one, %d told apart by their digests", same, apart)
	}
}

// FuzzABodyDigestNeverSkipsTheCallsRecording is the property above, fuzzed.
func FuzzABodyDigestNeverSkipsTheCallsRecording(f *testing.F) {
	f.Add(`{"a":1.50,"b":[1,2]}`, `{"b":[1,2],"a":1.5}`)
	f.Add(`{"ts":1,"id":2}`, `{"id":2}`)
	f.Add(`x`, `x`)
	f.Fuzz(func(t *testing.T, rec, live string) {
		for _, noise := range []map[string][]string{nil, {"ts": {}}} {
			if !bodiesSame(rec, live, noise) {
				continue
			}
			if same, none := digestsSame(rec, live, noise); !same && !none {
				t.Fatalf("the strict comparison takes %q for %q (noise %v), but their digests differ", live, rec, noise)
			}
		}
	})
}

// A recording with noise of its own (a field it learned, a value the
// obfuscator redacted), whose comparison excuses what a digest reads, and one
// recorded as a form or a multipart form, compared field by field, has no
// digest: it is always compared.
func TestARecordingWithNoiseOfItsOwnHasNoDigest(t *testing.T) {
	n := strictNoise{}
	plain := recorded("plain", "/v1/orders", map[string]string{"Content-Type": "application/json"}, `{"id":1}`)
	if _, ok := recordedBodyDigest(plain, n); !ok {
		t.Fatal("a plain recording has no digest")
	}
	learned := recorded("learned", "/v1/orders", map[string]string{"Content-Type": "application/json"}, `{"id":1}`)
	learned.Spec.ReqBodyNoise = map[string][]string{"body.id": {}}
	redacted := recorded("redacted", "/v1/orders", map[string]string{"Content-Type": "application/json"}, `{"id":"REDACTED-1"}`)
	redacted.Noise = []string{`^REDACTED-\d+$`}
	form := recorded("form", "/v1/orders", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, `b=2&a=1`)
	multi := recorded("multi", "/v1/orders", map[string]string{"Content-Type": "multipart/form-data; boundary=x"}, "--x--")
	for _, m := range []*models.Mock{learned, redacted, form, multi} {
		if _, ok := recordedBodyDigest(m, n); ok {
			t.Fatalf("%s has a digest", m.Name)
		}
	}
}

// strictSame is strictDiff with no field, over every part a call may differ
// in.
func TestStrictSameIsStrictDiffWithNoField(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	rec := recorded("m", "/v1/orders?a=1", map[string]string{"Content-Type": "application/json", "X-Tenant": "t1"}, `{"id":1,"ts":5}`)
	learned := recorded("l", "/v1/orders?a=1", map[string]string{"Content-Type": "application/json", "X-Tenant": "t1"}, `{"id":1,"ts":5}`)
	learned.Spec.ReqBodyNoise = map[string][]string{"body.ts": {}, "url.query.a": {}}
	withTenant := func(h map[string]string) map[string]string {
		out := map[string]string{"X-Tenant": "t1"}
		for k, v := range h {
			out[k] = v
		}
		return out
	}
	calls := []*req{
		sent("/v1/orders?a=1", withTenant(ct), `{"id":1,"ts":5}`),
		sent("/v1/orders?a=1", withTenant(ct), `{"ts":5,"id":1}`),
		sent("/v1/orders?a=2", withTenant(ct), `{"id":1,"ts":5}`),
		sent("/v1/order?a=1", withTenant(ct), `{"id":1,"ts":5}`),
		sent("/v1/orders?a=1", ct, `{"id":1,"ts":5}`),
		sent("/v1/orders?a=1", withTenant(ct), `{"id":1,"ts":6}`),
		sent("/v1/orders?a=1", withTenant(ct), `{"id":2,"ts":5}`),
	}
	get := sent("/v1/orders?a=1", withTenant(ct), `{"id":1,"ts":5}`)
	get.method = "GET"
	calls = append(calls, get)
	n := strictNoise{header: map[string]bool{}}
	same := 0
	for _, m := range []*models.Mock{rec, learned} {
		for i, c := range calls {
			live := liveDestination(context.Background(), c)
			want := len(strictDiff(c, live, m, n)) == 0
			if got := strictSame(c, live, m, n); got != want {
				t.Fatalf("%s, call %d: strictSame %v, strictDiff with no field %v", m.Name, i, got, want)
			}
			if want {
				same++
			}
		}
	}
	// rec is calls 0 and 1; learned is those and the ones that changed
	// what it learned (2 and 5).
	if same != 6 {
		t.Fatalf("fixture: %d calls are their recording, want 6", same)
	}
}

// randomJSON is a random JSON value, nested up to depth.
func randomJSON(r *rand.Rand, depth int) interface{} {
	switch k := r.Intn(7); {
	case k == 0 && depth > 0:
		o := map[string]interface{}{}
		for i, n := 0, r.Intn(4); i < n; i++ {
			o[[]string{"id", "ts", "at", "items", "o", "p", "name"}[r.Intn(7)]] = randomJSON(r, depth-1)
		}
		return o
	case k == 1 && depth > 0:
		var a []interface{}
		for i, n := 0, r.Intn(3); i < n; i++ {
			a = append(a, randomJSON(r, depth-1))
		}
		return a
	case k == 2:
		return []string{"a", "b", "", "ü"}[r.Intn(4)]
	case k == 3:
		return r.Intn(2) == 0
	case k == 4:
		return nil
	default:
		return json.Number([]string{"1", "1.0", "2", "0", "-0", "1e2", "100", "1.5", "1.50"}[r.Intn(9)])
	}
}

// mutate is v with one random change somewhere in it.
func mutate(r *rand.Rand, v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := map[string]interface{}{}
		for k, c := range t {
			out[k] = c
		}
		keys := jsonKeys(out)
		if len(keys) == 0 || r.Intn(4) == 0 {
			out["ts"] = randomJSON(r, 1)
			return out
		}
		k := keys[r.Intn(len(keys))]
		if r.Intn(3) == 0 {
			delete(out, k)
		} else {
			out[k] = mutate(r, out[k])
		}
		return out
	case []interface{}:
		out := append([]interface{}(nil), t...)
		if len(out) == 0 {
			return append(out, randomJSON(r, 1))
		}
		i := r.Intn(len(out))
		out[i] = mutate(r, out[i])
		return out
	}
	return randomJSON(r, 1)
}

// spell writes v as JSON, its keys in a random order, its numbers in a
// random spelling of their value, with random white space.
func spell(r *rand.Rand, v interface{}) string {
	var b strings.Builder
	var write func(v interface{})
	ws := func() {
		if r.Intn(3) == 0 {
			b.WriteString(" ")
		}
	}
	write = func(v interface{}) {
		ws()
		switch t := v.(type) {
		case map[string]interface{}:
			keys := jsonKeys(t)
			r.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
			b.WriteString("{")
			for i, k := range keys {
				if i > 0 {
					b.WriteString(",")
				}
				kb, _ := json.Marshal(k)
				b.Write(kb)
				b.WriteString(":")
				write(t[k])
			}
			b.WriteString("}")
		case []interface{}:
			b.WriteString("[")
			for i, e := range t {
				if i > 0 {
					b.WriteString(",")
				}
				write(e)
			}
			b.WriteString("]")
		case json.Number:
			c, _ := canonicalNumber(t.String())
			// The value, spelled as recorded or as its digits scaled.
			if r.Intn(2) == 0 {
				b.WriteString(t.String())
			} else {
				b.WriteString(c)
			}
		default:
			out, _ := json.Marshal(t)
			if s, ok := t.(string); ok && r.Intn(2) == 0 && s != "" {
				out = []byte(fmt.Sprintf(`"\u%04x%s"`, []rune(s)[0], string([]rune(s)[1:])))
			}
			b.Write(out)
		}
		ws()
	}
	write(v)
	return b.String()
}

func jsonKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// digestCase is a random recorded request and a live call made from it: one
// the strict comparison may take for it (every change it excuses), or one
// with a change it does not.
type digestCase struct {
	rec  *models.Mock
	live *req
}

// randomRequest is a random request's parts: a method, path segments, query
// parameters (each with its values), headers and a JSON body.
type randomRequest struct {
	method  string
	path    []string
	query   map[string][]string
	headers map[string]string
	body    interface{}
}

func newRandomRequest(r *rand.Rand) randomRequest {
	q := randomRequest{
		method:  []string{"GET", "POST"}[r.Intn(2)],
		query:   map[string][]string{},
		headers: map[string]string{},
		body:    randomJSON(r, 2),
	}
	for i, n := 0, 1+r.Intn(3); i < n; i++ {
		q.path = append(q.path, []string{"v1", "orders", "jobs", "o-12", "o-7", "x"}[r.Intn(6)])
	}
	for i, n := 0, r.Intn(4); i < n; i++ {
		name := []string{"cursor", "a", "b", "keployRun", "X-Amz-Date", "X-Amz-Signature", "x-amz-date"}[r.Intn(7)]
		for j, m := 0, 1+r.Intn(2); j < m; j++ {
			q.query[name] = append(q.query[name], []string{"c1", "c2", "", "o-3", "o-4", "a b"}[r.Intn(6)])
		}
	}
	for i, n := 0, r.Intn(5); i < n; i++ {
		name := []string{"Content-Type", "User-Agent", "X-Tenant", "X-Noise", "Keploy-Test-Id", "Content-Length", "Authorization", "X-Amz-Content-Sha256", "Accept"}[r.Intn(9)]
		choices := map[string][]string{
			"Content-Type":         {"application/json", "application/json; charset=utf-8", "text/plain"},
			"User-Agent":           {"sdk/1.0 go", "sdk/2.1", "other/1.0"},
			"Authorization":        {"Bearer t1", "Bearer t2"},
			"X-Amz-Content-Sha256": {"UNSIGNED-PAYLOAD", "abc"},
		}[name]
		if len(choices) == 0 {
			choices = []string{"v1", "v2", ""}
		}
		q.headers[name] = choices[r.Intn(len(choices))]
	}
	return q
}

// excused is q changed in ways the strict comparison excuses under the noise
// digestNoise gives: a segment or a query value the url noise masks, a
// keploy parameter, the
// order of the query and of a repeated parameter's values, a noise header,
// keploy's own and Host and Content-Length headers, a header name's case, a
// Content-Type's parameters, a User-Agent's version, and the body's
// spelling.
func (q randomRequest) excused(r *rand.Rand) randomRequest {
	out := randomRequest{method: q.method, query: map[string][]string{}, headers: map[string]string{}, body: q.body}
	for _, seg := range q.path {
		if strings.HasPrefix(seg, "o-") && r.Intn(2) == 0 {
			seg = "o-" + fmt.Sprint(r.Intn(100))
		}
		out.path = append(out.path, seg)
	}
	for name, vals := range q.query {
		vals = append([]string(nil), vals...)
		for i, v := range vals {
			if strings.HasPrefix(v, "o-") && r.Intn(2) == 0 {
				vals[i] = "o-" + fmt.Sprint(r.Intn(100))
			}
		}
		r.Shuffle(len(vals), func(i, j int) { vals[i], vals[j] = vals[j], vals[i] })
		if strings.HasPrefix(strings.ToLower(name), "keploy") {
			vals = []string{"r" + fmt.Sprint(r.Intn(9))}
		}
		out.query[name] = vals
	}
	if r.Intn(3) == 0 {
		out.query["keployRun"] = []string{"r9"}
	}
	for name, v := range q.headers {
		switch name {
		case "X-Noise", "Keploy-Test-Id", "Content-Length":
			v = "changed"
		case "Content-Type":
			if v == "application/json" {
				v = "application/json; charset=utf-8"
			}
		case "User-Agent":
			v = strings.Replace(v, "/1.0", "/1.1", 1)
		}
		if r.Intn(2) == 0 {
			name = strings.ToLower(name)
		}
		out.headers[name] = v
	}
	if r.Intn(3) == 0 {
		out.headers["X-Noise"] = "added"
	}
	return out
}

// changed is q with one change the strict comparison does not excuse.
func (q randomRequest) changed(r *rand.Rand) randomRequest {
	out := q.excused(r)
	switch r.Intn(5) {
	case 0:
		out.method = map[string]string{"GET": "POST", "POST": "GET"}[q.method]
	case 1:
		out.path = append(append([]string(nil), out.path...), "extra")
	case 2:
		out.query["cursor"] = append(append([]string(nil), out.query["cursor"]...), "c9")
	case 3:
		out.headers["X-Tenant"] = out.headers["X-Tenant"] + "-other"
	default:
		out.body = mutate(r, q.body)
	}
	return out
}

func (q randomRequest) rawURL(r *rand.Rand) string {
	var parts []string
	for name, vals := range q.query {
		for _, v := range vals {
			parts = append(parts, url.QueryEscape(name)+"="+url.QueryEscape(v))
		}
	}
	r.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
	u := "/" + strings.Join(q.path, "/")
	if len(parts) > 0 {
		u += "?" + strings.Join(parts, "&")
	}
	return u
}

func (q randomRequest) recording(r *rand.Rand) *models.Mock {
	m := recorded("m", "/", q.headers, spell(r, q.body))
	m.Spec.HTTPReq.Method = models.Method(q.method)
	m.Spec.HTTPReq.URL = q.rawURL(r)
	m.Spec.HTTPReq.URLParams = nil
	return m
}

func (q randomRequest) call(r *rand.Rand) *req {
	u, _ := url.Parse(q.rawURL(r))
	h := http.Header{}
	h.Set("Host", "api.example")
	for k, v := range q.headers {
		h.Add(k, v)
	}
	return &req{method: q.method, url: u, header: h, body: []byte(spell(r, q.body))}
}

// digestNoise is the noise digestCases are read under: a header, a url
// pattern and, at random, body noise.
func digestNoise(r *rand.Rand) strictNoise {
	bodies := []map[string][]string{nil, {"ts": {}}, {"items[].at": {}}}
	return strictNoise{
		header: map[string]bool{"x-noise": true},
		body:   matcher.NewNoiseLookup(bodies[r.Intn(len(bodies))]),
		url:    compileURLNoise(zap.NewNop(), []string{`o-[0-9]+`}),
	}
}

// Over many requests and calls made from them, a recording the strict
// comparison takes for the call (strictSame) is never ruled out by its
// digest, whatever the call changed that the comparison excuses: a lookup
// that compares only the recordings its digest does not rule out never
// skips the call's recording. And the digest narrows: most calls with a
// change it does not excuse rule their recording out, in every part.
func TestARequestDigestNeverRulesOutTheCallsRecording(t *testing.T) {
	r := rand.New(rand.NewSource(11))
	same, apart, differ := 0, 0, 0
	for i := 0; i < 6000; i++ {
		q := newRandomRequest(r)
		rec := q.recording(r)
		var live *req
		if r.Intn(3) == 0 {
			live = q.changed(r).call(r)
		} else {
			live = q.excused(r).call(r)
		}
		n := digestNoise(r)
		ruledOut := recordedRequestDigest(rec, n).rulesOut(liveRequestDigest(live, n))
		if strictSame(live, liveDestination(context.Background(), live), rec, n) {
			same++
			if ruledOut {
				t.Fatalf("the strict comparison takes %s %s %v %s for %s %s %v %s, but its digest rules it out",
					live.method, live.url, live.header, live.body, rec.Spec.HTTPReq.Method, rec.Spec.HTTPReq.URL, rec.Spec.HTTPReq.Header, rec.Spec.HTTPReq.Body)
			}
			continue
		}
		differ++
		if ruledOut {
			apart++
		}
	}
	if same < 2000 || differ < 1000 || apart*10 < differ*8 {
		t.Fatalf("fixture: %d calls taken for their recording, %d not, %d of those ruled out by the digest", same, differ, apart)
	}
}

// Each part of a request's digest tells apart calls that differ in it alone:
// the method, the path, a query value, a header the comparison reads and the
// body. A part either side has none of (a recording that learned a field,
// one with values the obfuscator redacted, one recorded as a form) is not
// read, so it rules nothing out.
func TestARequestDigestReadsEveryPartTheComparisonReads(t *testing.T) {
	n := strictNoise{header: map[string]bool{"x-noise": true}}
	base := func() *models.Mock {
		m := recorded("m", "/", map[string]string{"Content-Type": "application/json", "X-Tenant": "t1", "X-Noise": "n1"}, `{"id":1}`)
		m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPReq.URLParams = "POST", "/v1/orders?cursor=c1&cursor=c2", nil
		return m
	}
	call := func(method, target, tenant, body string) *req {
		c := sent(target, map[string]string{"Content-Type": "application/json; charset=utf-8", "X-Tenant": tenant, "X-Noise": "n2", "Keploy-Test-Id": "t"}, body)
		c.method = method
		return c
	}
	same := call("POST", "/v1/orders?cursor=c2&cursor=c1&keployRun=1", "t1", `{ "id" : 1.0 }`)
	if recordedRequestDigest(base(), n).rulesOut(liveRequestDigest(same, n)) {
		t.Fatal("the call the recording is was ruled out")
	}
	for name, c := range map[string]*req{
		"method":  call("PUT", "/v1/orders?cursor=c1&cursor=c2", "t1", `{"id":1}`),
		"path":    call("POST", "/v1/order?cursor=c1&cursor=c2", "t1", `{"id":1}`),
		"query":   call("POST", "/v1/orders?cursor=c1&cursor=c3", "t1", `{"id":1}`),
		"a param": call("POST", "/v1/orders?cursor=c1&cursor=c2&page=2", "t1", `{"id":1}`),
		"header":  call("POST", "/v1/orders?cursor=c1&cursor=c2", "t2", `{"id":1}`),
		"body":    call("POST", "/v1/orders?cursor=c1&cursor=c2", "t1", `{"id":2}`),
	} {
		if !recordedRequestDigest(base(), n).rulesOut(liveRequestDigest(c, n)) {
			t.Errorf("a call that differs in its %s alone is not ruled out", name)
		}
	}
	learned := base()
	learned.Spec.ReqBodyNoise = map[string][]string{"url.query.cursor": {}}
	redacted := base()
	redacted.Noise = []string{`^t1$`}
	form := base()
	form.Spec.HTTPReq.Header["Content-Type"] = "application/x-www-form-urlencoded"
	formCall := func(tenant, body string) *req {
		c := call("POST", "/v1/orders?cursor=c1&cursor=c2", tenant, body)
		c.header.Set("Content-Type", "application/x-www-form-urlencoded")
		return c
	}
	for _, tc := range []struct {
		name string
		m    *models.Mock
		call *req
	}{
		{"learned", learned, call("POST", "/v1/orders?cursor=c9&cursor=c2", "t1", `{"id":1}`)},
		{"redacted", redacted, call("POST", "/v1/orders?cursor=c1&cursor=c2", "t2", `{"id":1}`)},
		{"form", form, formCall("t1", `{"id":2}`)},
	} {
		if recordedRequestDigest(tc.m, n).rulesOut(liveRequestDigest(tc.call, n)) {
			t.Errorf("%s: a part read with the recording's own noise ruled it out", tc.name)
		}
	}
	// What one part of a redacted or form recording still reads.
	if !recordedRequestDigest(redacted, n).rulesOut(liveRequestDigest(call("POST", "/v1/jobs", "t2", `{"id":1}`), n)) {
		t.Error("a redacted recording's request line rules nothing out")
	}
	if !recordedRequestDigest(form, n).rulesOut(liveRequestDigest(formCall("t9", `{"id":1}`), n)) {
		t.Error("a form recording's headers rule nothing out")
	}
	// Names that differ in case only: the live headers' digest is none.
	odd := call("POST", "/v1/orders?cursor=c1&cursor=c2", "t2", `{"id":1}`)
	odd.header["x-tenant"] = []string{"t1"}
	if _, ok := liveHeaderDigest(odd.header, n); ok {
		t.Error("a call with two header names one in case has a headers digest")
	}
}

// Two lookups under one noise share their recordings' digests; another noise
// (a header, a url pattern, body noise) is another digest.
func TestARequestDigestIsNamedByTheNoiseItReads(t *testing.T) {
	n := strictNoise{header: map[string]bool{"x-a": true, "x-b": true}, url: compileURLNoise(zap.NewNop(), []string{`o-[0-9]+`}), body: matcher.NewNoiseLookup(map[string][]string{"ts": {}})}
	again := strictNoise{header: map[string]bool{"x-b": true, "x-a": true}, url: compileURLNoise(zap.NewNop(), []string{`o-[0-9]+`}), body: matcher.NewNoiseLookup(map[string][]string{"ts": {}})}
	if requestDigestOf(n) != requestDigestOf(again) {
		t.Fatal("one noise names two digests")
	}
	for _, other := range []strictNoise{
		{header: map[string]bool{"x-a": true}, url: n.url, body: n.body},
		// As many names, patterns or fields as n, but others.
		{header: map[string]bool{"x-a": true, "x-c": true}, url: n.url, body: n.body},
		{header: n.header, url: compileURLNoise(zap.NewNop(), []string{`p-[0-9]+`}), body: n.body},
		{header: n.header, url: n.url, body: matcher.NewNoiseLookup(map[string][]string{"at": {}})},
		{header: n.header, body: n.body},
		{header: n.header, url: n.url},
	} {
		if requestDigestOf(other) == requestDigestOf(n) {
			t.Fatalf("another noise names the same digest: %q", requestDigestOf(other))
		}
	}
}

// What the strict comparison reads by its SigV4 grammar (an Authorization's
// parts, a presigned URL's signing parameters, the payload hash against the
// bytes sent) differs on every call the comparison takes for its recording:
// a request digest never reads it, so a call signed again, a URL presigned
// again, or a payload hash over the body as sent, never rules out its
// recording.
func TestARequestDigestLeavesWhatSigV4ReadsByItsGrammar(t *testing.T) {
	hn := models.ProvenFlakyHeaderNoise()
	n := strictNoise{header: map[string]bool{}}
	for k := range hn {
		n.header[strings.ToLower(k)] = true
	}
	body := "hello"
	for _, tc := range []struct {
		name string
		rec  *models.Mock
		live *req
	}{
		{"signed again",
			recorded("put", "/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf(body)), body),
			sent("/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), "20261015T101533Z", hashOf(body)), body)},
		{"presigned again",
			recorded("get", presigned("AKID/20261015/us-east-1/s3/aws4_request", "20261015T101010Z", 900, 1), nil, ""),
			sent(presigned("OTHER/20261016/us-east-1/s3/aws4_request", "20261016T000003Z", 900, 2), nil, "")},
		{"a payload hash over the bytes sent",
			recorded("put", "/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(1)), "20261015T101010Z", hashOf("as chunks")), body),
			sent("/bucket/key", awsCall(sigV4("AKID", "20261015", "us-east-1", "s3", signed, sig(2)), "20261015T101533Z", hashOf(body)), body)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strictSame(tc.live, liveDestination(context.Background(), tc.live), tc.rec, n) {
				t.Fatalf("fixture: the strict comparison does not take the call for its recording: %v", strictDiff(tc.live, liveDestination(context.Background(), tc.live), tc.rec, n))
			}
			if recordedRequestDigest(tc.rec, n).rulesOut(liveRequestDigest(tc.live, n)) {
				t.Fatal("the call's digest rules its recording out")
			}
		})
	}
}
