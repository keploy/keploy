package http

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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

// One call and one answer are what the matcher and a test's response
// comparison compare, and nothing else: a recording differing from the first
// only where neither compares is the same read; one differing where either
// does is another call or another answer.
func TestRepeatReadOfComparesWhatTheMatcherAndTheResponseCompare(t *testing.T) {
	base := repeatReadMock(nil)
	call0, answer0, ok := RepeatReadOf(base)
	if !ok {
		t.Fatal("a recorded HTTP read is not one keploy can prove")
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
		{"the page it was asked from", func(m *models.Mock) { m.Spec.HTTPReq.Header["Referer"] = "http://crm.test:8080/crm/deals/D-1" }, true, true},
		{"a header the matcher ignores", func(m *models.Mock) { m.Spec.HTTPReq.Header["Traceparent"] = "00-ab-cd-01" }, true, true},
		{"keploy's own header", func(m *models.Mock) { m.Spec.HTTPReq.Header["Keploy-Test-Id"] = "t2" }, true, true},
		{"a header the other lacks", func(m *models.Mock) { m.Spec.HTTPReq.Header["X-Requested-With"] = "XMLHttpRequest" }, false, true},
		{"who calls", func(m *models.Mock) { m.Spec.HTTPReq.Header["Authorization"] = "token b" }, false, true},
		{"the host", func(m *models.Mock) { m.Spec.HTTPReq.Header["Host"] = "other.test:8080" }, false, true},
		{"the method", func(m *models.Mock) { m.Spec.HTTPReq.Method = "GET" }, false, true},
		{"the URL", func(m *models.Mock) { m.Spec.HTTPReq.URL += "?x=1" }, false, true},
		{"the body asked", func(m *models.Mock) { m.Spec.HTTPReq.Body = "{}" }, false, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			call, answer, ok := RepeatReadOf(repeatReadMock(r.edit))
			if !ok {
				t.Fatal("not a recording keploy can prove")
			}
			if (call == call0) != r.sameCall || (answer == answer0) != r.sameAnsw {
				t.Errorf("same call %v, same answer %v; want %v, %v", call == call0, answer == answer0, r.sameCall, r.sameAnsw)
			}
		})
	}
	// Both sides carry Authorization: its value is compared.
	a := repeatReadMock(func(m *models.Mock) { m.Spec.HTTPReq.Header["Authorization"] = "token a" })
	b := repeatReadMock(func(m *models.Mock) { m.Spec.HTTPReq.Header["Authorization"] = "token b" })
	ca, _, _ := RepeatReadOf(a)
	cb, _, _ := RepeatReadOf(b)
	if ca == cb {
		t.Error("two callers' recordings are one call")
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
	_, _, ok = RepeatReadOf(m)
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
