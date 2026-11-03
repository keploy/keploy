package http

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// learnedOn is a recording whose earlier learning learned noise at each of
// places, as the learn step stores it.
func learnedOn(m *models.Mock, places ...string) *models.Mock {
	m.Spec.ReqBodyNoise = map[string][]string{}
	for _, p := range places {
		m.Spec.ReqBodyNoise[p] = []string{}
	}
	return m
}

// keploy.yml's entry with patterns is the user's word on its field: the learn
// step (record's full round, under the proven switch) never learns a value
// outside them as noise. The call misses, and the miss names the field, the
// patterns and the side outside them, by its kind and length only. A
// recording that already learned noise there is held to the patterns too.
// Before, the learn step learned the field like any other drift and served
// the call, as if keploy.yml had no entry. The noise reaches the learn step
// through the options, as keploy.yml writes it.
func TestTheLearnStepNeverLearnsAValueOutsideKeployYmlPatterns(t *testing.T) {
	body := func(id string) string { return `{"request_id":"` + id + `","item":"book"}` }
	for _, c := range []struct {
		name            string
		cfg             map[string]map[string][]string
		rec, call       string
		recHdr, callHdr map[string]string
		learned         []string
		served          bool
		why             string
	}{
		{name: "a body field, the live value outside", cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			rec: body("r-1"), call: body("evil"),
			why: `keploy.yml's noise lets body.request_id vary only within "^r-\\d$"; the live value (a string of 4 bytes) is outside it`},
		{name: "a body field, the recorded value outside", cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-[2-9]$`}}},
			rec: body("r-1"), call: body("r-2"),
			why: `keploy.yml's noise lets body.request_id vary only within "^r-[2-9]$"; the recorded value (a string of 3 bytes) is outside it`},
		{name: "a body field the recording learned, the live value outside", cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			rec: body("r-1"), call: body("evil"), learned: []string{"body.request_id"},
			why: `keploy.yml's noise lets body.request_id vary only within "^r-\\d$"; the live value (a string of 4 bytes) is outside it`},
		{name: "a header, the live value outside", cfg: map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`}}},
			rec: body("r-1"), call: body("r-1"), recHdr: map[string]string{"X-Id": "a-1"}, callHdr: map[string]string{"X-Id": "evil"},
			why: `keploy.yml's noise lets header.x-id vary only within "^a-\\d$"; the live value (a string of 4 bytes) is outside it`},
		{name: "a header the recording learned, the live value outside", cfg: map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`}}},
			rec: body("r-1"), call: body("r-1"), recHdr: map[string]string{"X-Id": "a-1"}, callHdr: map[string]string{"X-Id": "evil"}, learned: []string{"header.x-id"},
			why: `keploy.yml's noise lets header.x-id vary only within "^a-\\d$"; the live value (a string of 4 bytes) is outside it`},
		{name: "a number field, the live value outside", cfg: map[string]map[string][]string{"requestbody": {"seq": {`^\d$`}}},
			rec: `{"seq":7,"item":"book"}`, call: `{"seq":123,"item":"book"}`,
			why: `keploy.yml's noise lets body.seq vary only within "^\\d$"; the live value (a number of 3 characters) is outside it`},
		{name: "a body field, both values inside", cfg: map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			rec: body("r-1"), call: body("r-2"), served: true},
		{name: "a header, both values inside", cfg: map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`}}},
			rec: body("r-1"), call: body("r-1"), recHdr: map[string]string{"X-Id": "a-1"}, callHdr: map[string]string{"X-Id": "a-2"}, served: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			opts.NoiseConfig = c.cfg
			noise := NewRequestNoise(zap.NewNop(), opts)
			n := h.newLearnNoise(opts, noise.Body, noise.URL)
			db := &learnDb{perTest: []*models.Mock{learnedOn(learnRecorded("mock-0", 0, "POST", "/v1/orders", c.rec, c.recHdr, `{"ok":true}`), c.learned...)}}
			served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/orders", c.call, c.callHdr), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if c.served {
				if served == nil || miss != nil {
					t.Fatalf("served %v, miss %+v; want served", served != nil, miss)
				}
				for _, m := range db.consumed {
					if _, ok := m.Spec.ReqBodyNoise["body.request_id"]; ok {
						t.Fatalf("learned %v of a field keploy.yml's patterns excuse", m.Spec.ReqBodyNoise)
					}
				}
				return
			}
			if served != nil || miss == nil || len(db.consumed) != 0 {
				t.Fatalf("served %v, consumed %d, miss %v; want a miss and nothing learned", served != nil, len(db.consumed), miss != nil)
			}
			if miss.res.Reason != models.MissRequestChanged || !strings.Contains(miss.res.Why, c.why) || !strings.Contains(miss.res.Why, "keploy learns nothing from this call") {
				t.Fatalf("miss %q (%q); want %q", miss.res.Why, miss.res.Reason, c.why)
			}
			for _, secret := range []string{"evil", "r-1", "r-2", "a-1", "123"} {
				if strings.Contains(miss.res.Why, secret) {
					t.Fatalf("the miss carries the value %q: %q", secret, miss.res.Why)
				}
			}
		})
	}
}

// The learn step's reason names a leaf by its kind and length, never its
// text, in the strict comparison's words, whatever the leaf's type.
func TestLeafShapeNamesKindAndLength(t *testing.T) {
	for _, c := range []struct {
		l    learn.Leaf
		want string
	}{
		{learn.Leaf{Type: learn.String, Value: "evil"}, "a string of 4 bytes"},
		{learn.Leaf{Type: learn.Number, Value: "123"}, "a number of 3 characters"},
		{learn.Leaf{Type: learn.Bool, Value: "false"}, "a boolean"},
		{learn.Leaf{Type: learn.Null}, "null"},
		{learn.Leaf{Type: learn.EmptyObj}, "an object"},
		{learn.Leaf{Type: learn.EmptyArray}, "an array"},
	} {
		if got := leafShape(c.l); got != c.want {
			t.Errorf("leafShape(%c %q) = %q; want %q", c.l.Type, c.l.Value, got, c.want)
		}
	}
}

// A keploy.yml entry with no patterns keeps main's rule in the learn step:
// on a leaf the recording also learned, the learned entry decides first and
// keeps the leaf's type, so a new value passes and a retype is still a
// change. Only an entry with patterns sets learned noise aside.
func TestAWholeFieldEntryOnALearnedLeafKeepsMainsRule(t *testing.T) {
	for _, c := range []struct {
		name, call string
		served     bool
	}{
		{"a new value", `{"request_id":"r-2","item":"book"}`, true},
		{"a retype", `{"request_id":2,"item":"book"}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			opts.NoiseConfig = map[string]map[string][]string{"requestbody": {"request_id": {}}}
			noise := NewRequestNoise(zap.NewNop(), opts)
			n := h.newLearnNoise(opts, noise.Body, noise.URL)
			db := &learnDb{perTest: []*models.Mock{learnedOn(learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"request_id":"r-1","item":"book"}`, nil, `{"ok":true}`), "body.request_id")}}
			served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/orders", c.call, nil), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if (served != nil) != c.served || (miss != nil) == c.served {
				t.Fatalf("served %v, miss %v; want served %v", served != nil, miss != nil, c.served)
			}
		})
	}
}

// The chain a user meets under the proven switch: record's full round meets
// a call outside keploy.yml's pattern, then the replay meets another. Each
// is refused, the first by the learn step, the second by the strict
// comparison against the recording the first left as it was. Before, the
// full round learned the field and the replay served the second call too.
func TestLearnThenReplayHoldKeployYmlPatterns(t *testing.T) {
	for _, c := range []struct {
		name              string
		cfg               map[string]map[string][]string
		recBody, liveBody string
		recHdr, liveHdr   map[string]string
		replayBody        string
		replayHdr         map[string]string
		field             string
	}{
		{"a requestBody pattern", map[string]map[string][]string{"requestbody": {"request_id": {`^r-\d$`}}},
			`{"request_id":"r-1","item":"book"}`, `{"request_id":"evil","item":"book"}`, nil, nil,
			`{"request_id":"evil-2","item":"book"}`, nil, "body.request_id"},
		{"a header pattern", map[string]map[string][]string{"header": {"X-Id": {`^a-\d$`}}},
			`{"item":"book"}`, `{"item":"book"}`, map[string]string{"X-Id": "a-1"}, map[string]string{"X-Id": "evil"},
			`{"item":"book"}`, map[string]string{"X-Id": "evil-2"}, "header.x-id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			opts.NoiseConfig = c.cfg
			noise := NewRequestNoise(zap.NewNop(), opts)
			n := h.newLearnNoise(opts, noise.Body, noise.URL)
			rec := learnRecorded("mock-0", 0, "POST", "/v1/orders", c.recBody, c.recHdr, `{"ok":true}`)
			db := &learnDb{perTest: []*models.Mock{rec}}
			served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/orders", c.liveBody, c.liveHdr), db, n, opts)
			if err != nil || served != nil || miss == nil || len(db.consumed) != 0 {
				t.Fatalf("full round: served %v, miss %v, consumed %d, err %v; want a miss", served != nil, miss != nil, len(db.consumed), err)
			}
			if len(rec.Spec.ReqBodyNoise) != 0 {
				t.Fatalf("full round learned %v", rec.Spec.ReqBodyNoise)
			}
			got, diag := strictMatch(t, live("POST", "/v1/orders", c.replayBody, c.replayHdr), noise.Header, noise.Body, noise.URL, rec)
			if got != nil || !strings.Contains(whyOf(diag, c.field), "the live value (a string of 6 bytes) is outside it") {
				t.Fatalf("replay: served %v, differs %v, reason %q", got != nil, differs(diag), whyOf(diag, c.field))
			}
		})
	}
}

// Under the strict comparison keploy.yml's patterns decide a field they
// guard before any learned noise: a value they refuse is a change, even on
// a field the recording learned, or under a container it learned, in a JSON
// body, a form, a multipart form and a header. Before, the learned entry
// excused it, and the call was served.
func TestALearnedEntryNeverExcusesAValueKeployYmlPatternsRefuse(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	mp := map[string]string{"Content-Type": "multipart/form-data; boundary=XB"}
	part := func(v string) string {
		return "--XB\r\nContent-Disposition: form-data; name=\"id\"\r\n\r\n" + v + "\r\n--XB--\r\n"
	}
	withID := func(v string) map[string]string {
		return map[string]string{"Content-Type": "application/json", "X-Id": v}
	}
	for _, c := range []struct {
		name          string
		bodyNoise     map[string][]string
		headerNoise   map[string][]string
		header        map[string]string
		liveHeader    map[string]string
		recBody, body string
		learned       []string
		served        bool
		field, why    string
	}{
		{name: "JSON, the field learned", bodyNoise: map[string][]string{"id": {`^1$`}}, header: ct, recBody: `{"id":1,"name":"a"}`, body: `{"id":2,"name":"a"}`,
			learned: []string{"body.id"}, field: "body.id", why: "the live value (a number of 1 characters)"},
		{name: "JSON, a container of the field learned", bodyNoise: map[string][]string{"id": {`^1$`}}, header: ct, recBody: `{"o":{"id":1,"n":"a"}}`, body: `{"o":{"id":2,"n":"b"}}`,
			learned: []string{"body.o"}, field: "body.o.id", why: "the live value (a number of 1 characters)"},
		{name: "JSON, the field learned, both values inside", bodyNoise: map[string][]string{"id": {`^[12]$`}}, header: ct, recBody: `{"id":1}`, body: `{"id":2}`,
			learned: []string{"body.id"}, served: true},
		{name: "a form, the key learned", bodyNoise: map[string][]string{"id": {`^1$`}}, header: form, recBody: `id=1&x=a`, body: `id=2&x=a`,
			learned: []string{"body.id"}, field: "body.id", why: "the live value (a string of 1 bytes)"},
		{name: "a multipart field learned", bodyNoise: map[string][]string{"id": {`^1$`}}, header: mp, recBody: part("1"), body: part("2"),
			learned: []string{"body.id"}, field: "body.id", why: "the live value (a string of 1 bytes)"},
		{name: "a header learned", headerNoise: map[string][]string{"X-Id": {`^a-\d$`}}, header: withID("a-1"), liveHeader: withID("evil"), recBody: `{}`, body: `{}`,
			learned: []string{"header.x-id"}, field: "header.x-id", why: "the live value (a string of 4 bytes)"},
		{name: "a header learned, both values inside", headerNoise: map[string][]string{"X-Id": {`^a-\d$`}}, header: withID("a-1"), liveHeader: withID("a-2"), recBody: `{}`, body: `{}`,
			learned: []string{"header.x-id"}, served: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			lh := c.liveHeader
			if lh == nil {
				lh = c.header
			}
			bn := requestBodyNoise(c.bodyNoise).Body
			got, diag := strictMatch(t, sent("/x", lh, c.body), c.headerNoise, bn, nil, learnedOn(recorded("m", "/x", c.header, c.recBody), c.learned...))
			if (got != nil) != c.served {
				t.Fatalf("served %v (differs %v); want %v", got != nil, differs(diag), c.served)
			}
			if c.served {
				return
			}
			if why := whyOf(diag, c.field); !strings.Contains(why, c.why) || !strings.Contains(why, "vary only within") {
				t.Fatalf("reason %q for %s; want the pattern and %q (differs %v)", why, c.field, c.why, differs(diag))
			}
		})
	}
}

// Strict enforcement without the switch: keploy.yml's entry with patterns
// decides its field over noise an earlier detection round learned there, a
// case twin of the entry's key or a more specific path under it. Before, the
// learned entry won the matcher's tie-break (userId sorts before the
// lowercased userid) or its most specific first rule, and served a value
// outside the patterns.
func TestKeployYmlPatternWinsOverDetectedNoise(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct {
		name, key           string
		rec, detected, call string
	}{
		{"the same key", "id", `{"id":"u-1","name":"a"}`, `{"id":"evil","name":"a"}`, `{"id":"evil2","name":"a"}`},
		{"a case twin", "userId", `{"userId":"u-1","name":"a"}`, `{"userId":"evil","name":"a"}`, `{"userId":"evil2","name":"a"}`},
		{"a more specific path", "id", `{"user":{"id":"u-1"},"name":"a"}`, `{"user":{"id":"evil"},"name":"a"}`, `{"user":{"id":"evil2"},"name":"a"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			noise := requestBodyNoise(map[string][]string{c.key: {`^u-\d+$`}})
			m := recorded("m", "/x", ct, c.rec)
			eng := mocknoise.New(httpNoiseAdapter{}, true, true)
			drift, _ := eng.Detect(m, []byte(c.detected), noise.Body)
			if eng.Learn(m, drift) == 0 {
				t.Fatalf("detection learned nothing of %s (drift %v)", c.detected, drift)
			}
			if allowed, _ := eng.StrictReject(m, []byte(c.call), noise.Body); allowed {
				t.Fatalf("strict enforcement allowed a value outside the pattern over learned %v", m.Spec.ReqBodyNoise)
			}
			db := &mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}
			ok, _, _, err := newHTTP().match(context.Background(), sent("/x", ct, c.call), db, noise.Header, noise.Body, noise.URL, true, false, true, true, true)
			if err != nil || ok {
				t.Fatalf("served %v (err %v) a value outside the pattern over learned %v", ok, err, m.Spec.ReqBodyNoise)
			}
			inside := strings.Replace(c.call, "evil2", "u-7", 1)
			if allowed, d := eng.StrictReject(m, []byte(inside), noise.Body); !allowed {
				t.Fatalf("strict enforcement refused a value inside the pattern: %v", d)
			}
		})
	}
}

// A keploy.yml entry with no patterns keeps main's rule under strict
// enforcement: noise detection learned on a more specific path under it
// (user.id, learned before the user wrote id) still type-enforces that
// path, beside an entry with patterns elsewhere. Only an entry with
// patterns sets learned noise aside.
func TestAWholeFieldEntryKeepsLearnedTypeEnforcement(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	noise := requestBodyNoise(map[string][]string{"id": {}, "token": {`^t-\d$`}})
	m := recorded("m", "/x", ct, `{"user":{"id":"u-1"},"token":"t-1"}`)
	m.Spec.ReqBodyNoise = map[string][]string{"body.user.id": {}}
	eng := mocknoise.New(httpNoiseAdapter{}, true, true)
	if allowed, d := eng.StrictReject(m, []byte(`{"user":{"id":"u-2"},"token":"t-2"}`), noise.Body); !allowed {
		t.Fatalf("strict enforcement refused a new value under a whole-field entry: %v", d)
	}
	if allowed, _ := eng.StrictReject(m, []byte(`{"user":{"id":2},"token":"t-2"}`), noise.Body); allowed {
		t.Fatalf("strict enforcement allowed a retype of a learned path under a whole-field entry")
	}
}
