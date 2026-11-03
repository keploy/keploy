package http

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A recorded value the obfuscator redacted is not compared, but where
// keploy.yml's patterns guard the field the live value alone is held to them:
// a live value outside them, or none, is a change named with the field, the
// patterns and the live side, in the strict comparison's header, JSON, form
// and multipart readers. A live value inside them is served, and a field no
// pattern guards keeps main's rule. Before, the redacted recorded value
// excused the field whatever the live value.
func TestARedactedRecordingHoldsTheLiveValueToKeployYmlPatterns(t *testing.T) {
	ct := map[string]string{"Content-Type": "application/json"}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	mp := map[string]string{"Content-Type": "multipart/form-data; boundary=XB"}
	part := func(v string) string {
		return "--XB\r\nContent-Disposition: form-data; name=\"token\"\r\n\r\n" + v + "\r\n--XB--\r\n"
	}
	withKey := func(v string) map[string]string {
		return map[string]string{"Content-Type": "application/json", "X-Key": v}
	}
	token := map[string][]string{"token": {`^t-\d$`}}
	key := map[string][]string{"X-Key": {`^k-\d$`}}
	const redacted = `^REDACTED_abc$`
	for _, c := range []struct {
		name                   string
		bodyNoise, headerNoise map[string][]string
		header, liveHeader     map[string]string
		recBody, body          string
		obfuscated             string
		served                 bool
		field, why             string
	}{
		{name: "a header, the live value outside", headerNoise: key, header: withKey("REDACTED_abc"), liveHeader: withKey("evil"), recBody: `{}`, body: `{}`,
			field: "header.x-key", why: "the live value (a string of 4 bytes)"},
		{name: "a header, the live value inside", headerNoise: key, header: withKey("REDACTED_abc"), liveHeader: withKey("k-1"), recBody: `{}`, body: `{}`, served: true},
		{name: "a header no pattern guards", header: withKey("REDACTED_abc"), liveHeader: withKey("evil"), recBody: `{}`, body: `{}`, served: true},
		{name: "a JSON field, the live value outside", bodyNoise: token, header: ct, recBody: `{"token":"REDACTED_abc","n":1}`, body: `{"token":"evil","n":1}`,
			field: "body.token", why: "the live value (a string of 4 bytes)"},
		{name: "a JSON field, the live value of another type", bodyNoise: token, header: ct, recBody: `{"token":"REDACTED_abc","n":1}`, body: `{"token":7,"n":1}`,
			field: "body.token", why: "the live value (a number of 1 character)"},
		{name: "a JSON field the live body lacks", bodyNoise: token, header: ct, recBody: `{"token":"REDACTED_abc","n":1}`, body: `{"n":1}`,
			field: "body.token", why: "the live value (absent)"},
		{name: "a JSON field, the live value inside", bodyNoise: token, header: ct, recBody: `{"token":"REDACTED_abc","n":1}`, body: `{"token":"t-1","n":1}`, served: true},
		{name: "a JSON field no pattern guards", header: ct, recBody: `{"token":"REDACTED_abc","n":1}`, body: `{"token":"evil","n":1}`, served: true},
		{name: "a JSON field the live body lacks, no pattern guards", header: ct, recBody: `{"token":"REDACTED_abc","n":1}`, body: `{"n":1}`, served: true},
		{name: "a form key, the live value outside", bodyNoise: token, header: form, recBody: `token=REDACTED_abc&x=a`, body: `token=evil&x=a`, obfuscated: `^token=REDACTED_abc$`,
			field: "body.token", why: "the live value (a string of 4 bytes)"},
		{name: "a form key, the live value inside", bodyNoise: token, header: form, recBody: `token=REDACTED_abc&x=a`, body: `token=t-1&x=a`, obfuscated: `^token=REDACTED_abc$`, served: true},
		{name: "a form key no pattern guards", header: form, recBody: `token=REDACTED_abc&x=a`, body: `token=evil&x=a`, obfuscated: `^token=REDACTED_abc$`, served: true},
		{name: "a multipart field, the live value outside", bodyNoise: token, header: mp, recBody: part("REDACTED_abc"), body: part("evil"),
			field: "body.token", why: "the live value (a string of 4 bytes)"},
		{name: "a multipart field, the live value inside", bodyNoise: token, header: mp, recBody: part("REDACTED_abc"), body: part("t-1"), served: true},
		{name: "a multipart field no pattern guards", header: mp, recBody: part("REDACTED_abc"), body: part("evil"), served: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			lh := c.liveHeader
			if lh == nil {
				lh = c.header
			}
			m := recorded("m", "/x", c.header, c.recBody)
			m.Noise = []string{redacted}
			if c.obfuscated != "" {
				m.Noise = []string{c.obfuscated}
			}
			got, diag := strictMatch(t, sent("/x", lh, c.body), c.headerNoise, requestBodyNoise(c.bodyNoise).Body, nil, m)
			if (got != nil) != c.served {
				t.Fatalf("served %v (differs %v); want %v", got != nil, differs(diag), c.served)
			}
			if c.served {
				return
			}
			if why := whyOf(diag, c.field); !strings.Contains(why, c.why) || !strings.Contains(why, "vary only within") || strings.Contains(why, "evil") {
				t.Fatalf("reason %q for %s; want the pattern and %q, never the value (differs %v)", why, c.field, c.why, differs(diag))
			}
		})
	}
}

// The learn step holds the live value alone to keploy.yml's patterns where
// the obfuscator redacted the recorded one: a live value outside them is a
// miss that names the field and learns nothing, a body field and a header
// alike; one inside them is served. Before, the redacted recorded value
// excused the leaf, and the call was served.
func TestTheLearnStepHoldsTheLiveValueUnderARedactedRecording(t *testing.T) {
	for _, c := range []struct {
		name            string
		cfg             map[string]map[string][]string
		rec, call       string
		recHdr, callHdr map[string]string
		served          bool
		why             string
	}{
		{name: "a body field, the live value outside", cfg: map[string]map[string][]string{"requestbody": {"token": {`^t-\d$`}}},
			rec: `{"token":"REDACTED_abc","item":"book"}`, call: `{"token":"evil","item":"book"}`,
			why: `keploy.yml's noise lets body.token vary only within "^t-\\d$"; the live value (a string of 4 bytes) is outside it`},
		{name: "a body field, the live value inside", cfg: map[string]map[string][]string{"requestbody": {"token": {`^t-\d$`}}},
			rec: `{"token":"REDACTED_abc","item":"book"}`, call: `{"token":"t-1","item":"book"}`, served: true},
		{name: "a header, the live value outside", cfg: map[string]map[string][]string{"header": {"X-Key": {`^k-\d$`}}},
			rec: `{"item":"book"}`, call: `{"item":"book"}`, recHdr: map[string]string{"X-Key": "REDACTED_abc"}, callHdr: map[string]string{"X-Key": "evil"},
			why: `keploy.yml's noise lets header.x-key vary only within "^k-\\d$"; the live value (a string of 4 bytes) is outside it`},
		{name: "a header, the live value inside", cfg: map[string]map[string][]string{"header": {"X-Key": {`^k-\d$`}}},
			rec: `{"item":"book"}`, call: `{"item":"book"}`, recHdr: map[string]string{"X-Key": "REDACTED_abc"}, callHdr: map[string]string{"X-Key": "k-1"}, served: true},
		{name: "a body field no pattern guards", rec: `{"token":"REDACTED_abc","item":"book"}`, call: `{"token":"evil","item":"book"}`, served: true},
		// A field only the call holds is no value the obfuscator redacted in
		// the recording: under a pattern it is a change, even where the
		// call's value looks redacted and the pattern admits it.
		{name: "a body field only the call holds", cfg: map[string]map[string][]string{"requestbody": {"token": {`^REDACTED_\w+$`}}},
			rec: `{"item":"book"}`, call: `{"token":"REDACTED_abc","item":"book"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _, opts := learnOn(t)
			opts.NoiseConfig = c.cfg
			noise := NewRequestNoise(zap.NewNop(), opts)
			n := h.newLearnNoise(opts, noise.Body, noise.URL)
			m := learnRecorded("mock-0", 0, "POST", "/v1/orders", c.rec, c.recHdr, `{"ok":true}`)
			m.Noise = []string{`^REDACTED_abc$`}
			db := &learnDb{perTest: []*models.Mock{m}}
			served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/orders", c.call, c.callHdr), db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if c.served {
				if served == nil || miss != nil {
					t.Fatalf("served %v, miss %+v; want served", served != nil, miss)
				}
				return
			}
			if served != nil || miss == nil || len(db.consumed) != 0 {
				t.Fatalf("served %v, consumed %d, miss %v; want a miss and nothing learned", served != nil, len(db.consumed), miss != nil)
			}
			if c.why != "" && !strings.Contains(miss.res.Why, c.why) || strings.Contains(miss.res.Why, "evil") {
				t.Fatalf("miss %q; want %q, never the value", miss.res.Why, c.why)
			}
		})
	}
}
