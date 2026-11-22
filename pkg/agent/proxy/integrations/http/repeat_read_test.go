package http

import (
	"errors"
	"fmt"
	"regexp"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// repeatReadMock is a Frappe read as the browser recorded it, which edit
// changes before it is read.
func repeatReadMock(edit func(*models.Mock)) *models.Mock {
	m := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "/api/method/frappe.onboarding.get_onboarding_status", Header: map[string]string{
			"Host": "crm.test:8080", "Accept": "application/json", "Content-Length": "0",
			"Cookie": "sid=e7b5; system_user=yes", "Referer": "http://crm.test:8080/crm/leads/CRM-LEAD-1",
			"X-Frappe-Csrf-Token": "7fbd",
		}},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: `{"message":{}}`, Header: map[string]string{
			"Content-Type": "application/json", "Date": "Tue, 03 Nov 2026 20:45:17 GMT",
			"Set-Cookie": "sid=e7b5; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=612000; HttpOnly; Path=/; SameSite=Lax",
		}},
	}}
	if edit != nil {
		edit(m)
	}
	return m
}

// frappeNoise is the header noise the Frappe app's keploy.yml gives the
// matcher (Referer, whole) and an entry with a pattern (X-Trace), with the
// static list, as a replay hands them to its agent.
var frappeNoise = NewRepeatReadNoise(models.OutgoingOptions{NoiseConfig: map[string]map[string][]string{
	"header": {"Referer": {}, "X-Trace": {`^t-[0-9]+$`}},
}})

// One call and one answer are what the strict matcher and a test's response
// comparison compare, and nothing else: a recording differing from the first
// only where neither compares is the same read; one differing where either
// does is another call or another answer. A request header's value counts
// unless the matcher excuses it under the set's noise.
func TestRepeatReadOfComparesWhatTheMatcherAndTheResponseCompare(t *testing.T) {
	base := func(m *models.Mock) { m.Spec.HTTPReq.Header["X-Trace"] = "t-1" }
	call0, answer0, ok := RepeatReadOf(repeatReadMock(base), frappeNoise)
	if !ok {
		t.Fatal("a recorded HTTP read is not one keploy can prove")
	}
	learn := func(m *models.Mock, h string) {
		if m.Spec.ReqBodyNoise == nil {
			m.Spec.ReqBodyNoise = map[string][]string{}
		}
		m.Spec.ReqBodyNoise["header."+h] = []string{}
	}
	rows := []struct {
		name               string
		edit               func(*models.Mock)
		sameCall, sameAnsw bool
	}{
		{"identical", func(*models.Mock) {}, true, true},
		{"the answer's date", func(m *models.Mock) { m.Spec.HTTPResp.Header["Date"] = "Tue, 03 Nov 2026 20:45:19 GMT" }, true, true},
		{"a request id the server minted", func(m *models.Mock) { m.Spec.HTTPResp.Header["X-Request-Id"] = "r2" }, true, true},
		{"a cookie's Expires beside its Max-Age", func(m *models.Mock) {
			m.Spec.HTTPResp.Header["Set-Cookie"] = "sid=e7b5; Expires=Tue, 10 Nov 2026 22:45:19 GMT; Max-Age=612000; HttpOnly; Path=/; SameSite=Lax"
		}, true, true},
		{"a cookie's value", func(m *models.Mock) {
			m.Spec.HTTPResp.Header["Set-Cookie"] = "sid=0000; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=612000; HttpOnly; Path=/; SameSite=Lax"
		}, true, false},
		{"a cookie's Max-Age", func(m *models.Mock) {
			m.Spec.HTTPResp.Header["Set-Cookie"] = "sid=e7b5; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=1; HttpOnly; Path=/; SameSite=Lax"
		}, true, false},
		{"another header of the answer", func(m *models.Mock) { m.Spec.HTTPResp.Header["ETag"] = `"b"` }, true, false},
		{"the status", func(m *models.Mock) { m.Spec.HTTPResp.StatusCode = 500 }, true, false},
		{"the body", func(m *models.Mock) { m.Spec.HTTPResp.Body = `{"message":{"x":1}}` }, true, false},
		{"the binary body", func(m *models.Mock) { m.Spec.HTTPResp.Binary = "AAE=" }, true, false},
		{"the page it was asked from, which keploy.yml names noise", func(m *models.Mock) { m.Spec.HTTPReq.Header["Referer"] = "http://crm.test:8080/crm/deals/D-1" }, true, true},
		{"a header of the static list", func(m *models.Mock) { m.Spec.HTTPReq.Header["Traceparent"] = "00-ab-cd-01" }, true, true},
		{"keploy's own header", func(m *models.Mock) { m.Spec.HTTPReq.Header["Keploy-Test-Id"] = "t2" }, true, true},
		{"a value within keploy.yml's patterns", func(m *models.Mock) { m.Spec.HTTPReq.Header["X-Trace"] = "t-2" }, true, true},
		{"a value outside keploy.yml's patterns", func(m *models.Mock) { m.Spec.HTTPReq.Header["X-Trace"] = "trace-2" }, false, true},
		{"a header the other lacks", func(m *models.Mock) { m.Spec.HTTPReq.Header["X-Requested-With"] = "XMLHttpRequest" }, false, true},
		{"who calls", func(m *models.Mock) { m.Spec.HTTPReq.Header["Authorization"] = "token b" }, false, true},
		{"another user's cookie", func(m *models.Mock) { m.Spec.HTTPReq.Header["Cookie"] = "sid=0000; system_user=yes" }, false, true},
		{"a CSRF token", func(m *models.Mock) { m.Spec.HTTPReq.Header["X-Frappe-Csrf-Token"] = "0000" }, false, true},
		{"the language asked", func(m *models.Mock) { m.Spec.HTTPReq.Header["Accept"] = "text/html" }, false, true},
		{"a header learned as noise on this recording only", func(m *models.Mock) { learn(m, "accept") }, false, true},
		{"the host", func(m *models.Mock) { m.Spec.HTTPReq.Header["Host"] = "other.test:8080" }, false, true},
		{"the method", func(m *models.Mock) { m.Spec.HTTPReq.Method = "GET" }, false, true},
		{"the URL", func(m *models.Mock) { m.Spec.HTTPReq.URL += "?x=1" }, false, true},
		{"the body asked", func(m *models.Mock) { m.Spec.HTTPReq.Body = "{}" }, false, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			call, answer, ok := RepeatReadOf(repeatReadMock(func(m *models.Mock) { base(m); r.edit(m) }), frappeNoise)
			if !ok {
				t.Fatal("not a recording keploy can prove")
			}
			if (call == call0) != r.sameCall || (answer == answer0) != r.sameAnsw {
				t.Errorf("same call %v, same answer %v; want %v, %v", call == call0, answer == answer0, r.sameCall, r.sameAnsw)
			}
		})
	}
	// A header both recordings learned as noise may differ, as the matcher
	// takes a call for either one alike.
	ca, _, _ := RepeatReadOf(repeatReadMock(func(m *models.Mock) { learn(m, "accept"); m.Spec.HTTPReq.Header["Accept"] = "a" }), frappeNoise)
	cb, _, _ := RepeatReadOf(repeatReadMock(func(m *models.Mock) { learn(m, "accept"); m.Spec.HTTPReq.Header["Accept"] = "b" }), frappeNoise)
	if ca != cb {
		t.Error("two recordings that both learned Accept as noise are two calls")
	}
	// Learned as noise, a header's value is excused, not whether it was
	// sent: a recording that lacks it is another call.
	cn, _, _ := RepeatReadOf(repeatReadMock(func(m *models.Mock) { learn(m, "accept"); delete(m.Spec.HTTPReq.Header, "Accept") }), frappeNoise)
	if ca == cn {
		t.Error("a recording that sent Accept and one that did not, both having learned it as noise, are one call")
	}
}

// Only a Set-Cookie value loses its cookies' Expires dates: another header
// of the answer that holds the same text keeps its date, and two answers
// whose such header differs only by its date are two answers.
func TestRepeatReadOfLeavesOutOnlyASetCookiesDates(t *testing.T) {
	session := func(date string) func(*models.Mock) {
		return func(m *models.Mock) {
			m.Spec.HTTPResp.Header["X-Session"] = "sid=e7b5; Expires=" + date + "; Max-Age=612000; Path=/"
		}
	}
	_, a, okA := RepeatReadOf(repeatReadMock(session("Tue, 10 Nov 2026 22:45:17 GMT")), frappeNoise)
	_, b, okB := RepeatReadOf(repeatReadMock(session("Tue, 10 Nov 2026 22:45:19 GMT")), frappeNoise)
	if !okA || !okB {
		t.Fatal("not a recording keploy can prove")
	}
	if a == b {
		t.Error("two answers whose X-Session header differs by its Expires date are one answer")
	}
}

// The proof excuses only what the matcher excuses under the set's noise:
// with no noise given, no header; with the static list turned off
// (test.disableAutoHeaderNoise), not the static list's headers either.
func TestRepeatReadOfExcusesOnlyTheSetsNoise(t *testing.T) {
	referer := func(m *models.Mock) { m.Spec.HTTPReq.Header["Referer"] = "http://crm.test:8080/crm/deals/D-1" }
	trace := func(m *models.Mock) { m.Spec.HTTPReq.Header["Traceparent"] = "00-ab-cd-01" }
	rows := []struct {
		name  string
		noise RepeatReadNoise
		edit  func(*models.Mock)
		same  bool
	}{
		{"no noise: the page", RepeatReadNoise{}, referer, false},
		{"no noise: a static list header", RepeatReadNoise{}, trace, false},
		{"the static list: a static list header", NewRepeatReadNoise(models.OutgoingOptions{}), trace, true},
		{"the static list: the page", NewRepeatReadNoise(models.OutgoingOptions{}), referer, false},
		{"the static list turned off: a static list header", NewRepeatReadNoise(models.OutgoingOptions{DisableAutoHeaderNoise: true}), trace, false},
		{"the static list turned off, keploy.yml's noise kept: the page", NewRepeatReadNoise(models.OutgoingOptions{DisableAutoHeaderNoise: true, NoiseConfig: map[string]map[string][]string{"header": {"referer": {}}}}), referer, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			c0, _, _ := RepeatReadOf(repeatReadMock(nil), r.noise)
			c1, _, _ := RepeatReadOf(repeatReadMock(r.edit), r.noise)
			if (c0 == c1) != r.same {
				t.Errorf("same call %v; want %v", c0 == c1, r.same)
			}
		})
	}
}

// patsOf is pats as written, by name.
func patsOf(pats map[string][]*regexp.Regexp) map[string][]string {
	out := make(map[string][]string, len(pats))
	for k, v := range pats {
		out[k] = patternStrings(v)
	}
	return out
}

// The proof's noise is the header noise the matcher itself is handed
// (NewRequestNoise) under the proven switch, so the two cannot drift.
func TestRepeatReadNoiseIsTheMatchersHeaderNoise(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, opts := range []models.OutgoingOptions{
		{},
		{DisableAutoHeaderNoise: true},
		{NoiseConfig: map[string]map[string][]string{"header": {"Referer": {}, "X-Trace": {`^t-`}, "traceparent": {`^00-`}}}},
	} {
		whole, pats := splitHeaderNoise(zap.NewNop(), NewRequestNoise(zap.NewNop(), opts).Header)
		n := NewRepeatReadNoise(opts)
		if fmt.Sprint(n.whole) != fmt.Sprint(whole) || fmt.Sprint(patsOf(n.pats)) != fmt.Sprint(patsOf(pats)) {
			t.Errorf("%+v: the proof's noise %v %v is not the matcher's %v %v", opts, n.whole, patsOf(n.pats), whole, patsOf(pats))
		}
	}
}

// What keploy cannot say it answered is proof of nothing.
func TestRepeatReadOfProvesNoAnswerItCannotRead(t *testing.T) {
	for name, edit := range map[string]func(*models.Mock){
		"another protocol": func(m *models.Mock) { m.Kind = models.GENERIC },
		"no request":       func(m *models.Mock) { m.Spec.HTTPReq = nil },
		"no response":      func(m *models.Mock) { m.Spec.HTTPResp = nil },
		"a body left out":  func(m *models.Mock) { m.Spec.HTTPResp.BodySkipped = true },
		"a streamed body":  func(m *models.Mock) { m.Spec.HTTPResp.StreamBody = []models.HTTPStreamChunk{{}} },
		"an answer kept apart that cannot be read back": func(m *models.Mock) {
			m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
				return nil, nil, errors.New("disk mocks: read back")
			})
		},
	} {
		if provesRead(t, repeatReadMock(edit)) {
			t.Errorf("%s: proved", name)
		}
	}
	if provesRead(t, nil) {
		t.Error("no recording: proved")
	}
}

// provesRead is RepeatReadOf's ok for m, a panic failing the test.
func provesRead(t *testing.T, m *models.Mock) (ok bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("RepeatReadOf panicked: %v", r)
			ok = false
		}
	}()
	_, _, ok = RepeatReadOf(m, frappeNoise)
	return ok
}

// The date of a cookie keploy cannot read, or of one that has no Max-Age, is
// kept: the cookie is kept by it.
func TestWithoutCookieDatesDropsOnlyADateMaxAgeOverrides(t *testing.T) {
	for in, want := range map[string]string{
		"a=1; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=60; Path=/":                                     "a=1; Expires=; Max-Age=60; Path=/",
		"a=1; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Path=/":                                                 "a=1; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Path=/",
		"a=1; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=x":                                              "a=1; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=x",
		"a=1; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=60, b=2; Expires=Tue, 10 Nov 2026 22:45:17 GMT": "a=1; Expires=; Max-Age=60, b=2; Expires=Tue, 10 Nov 2026 22:45:17 GMT",
		"a=1; Path=/": "a=1; Path=/",
		// an Expires with no date has none to drop
		"a=1; Expires=Tue, 10 Nov 2026 22:45:17 GMT; Max-Age=60, b=2; Expires; Max-Age=5": "a=1; Expires=; Max-Age=60, b=2; Expires; Max-Age=5",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%q: panicked: %v", in, r)
				}
			}()
			if got := withoutCookieDates(in); got != want {
				t.Errorf("%q: %q; want %q", in, got, want)
			}
		}()
	}
}
