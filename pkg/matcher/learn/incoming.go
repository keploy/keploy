package learn

import (
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A CLIENT TEST'S OWN REQUESTS.
//
// A client test (keploy mock's: a test harness drives the application) has
// no keploy test case to give its request (Tracker.Give). It sends the
// application requests of its own, which keploy mock records as the test's
// test cases: each is what the application was given before the calls it
// led to, as a keploy test case's request is. A value the client made and
// sent (a name it builds from its clock, the auth_time of a session token
// it signs) and the application then writes or reads back is the value of
// that request's place, never a read of the application's clock nor a value
// the application made.
//
// So each request of a test that reaches the application in record's full
// round, paired with the request the test sent in its recording (by order,
// method and path: PairCase), is an earlier place of the test's calls from
// its arrival (Tracker.Incoming), read as a keploy test case's request is
// (GivenRequest). A call reads it only where it reached the application
// before the call in both runs (arrivedBefore): a request the client sent
// after the call held nothing the call could have taken. A request with no
// recorded request of the test to pair with gives nothing, nor does one sent
// while no test was open.

// ClientCase is one request a client test sent the application in its
// recording: its test case's name and its request as recorded.
type ClientCase struct {
	Name string
	Req  *models.HTTPReq
}

// idSegment is a URL path segment that names a record (a number, a long hex
// id, a UUID): two requests that differ there alone are the same call.
var idSegment = regexp.MustCompile(`^([0-9]+|[0-9a-fA-F]{12,}|[0-9a-fA-F]{8}-[0-9a-fA-F-]{27})$`)

// CaseKey is what makes two requests of a test the same call, as keploy
// mock's replay pairs the requests it saw with the recorded ones: the method
// and the URL's path with each segment that names a record (idSegment)
// blanked. Its query is not read: a client test that puts a value it made
// there sends the same call with another value.
func CaseKey(method models.Method, rawURL string) string {
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	parts := strings.Split(path, "/")
	for i, p := range parts {
		if idSegment.MatchString(p) {
			parts[i] = "{id}"
		}
	}
	return strings.ToUpper(string(method)) + " " + strings.Join(parts, "/")
}

// caseCursor pairs the requests of the open client test, as they reach the
// application, with the test's recorded requests (PairCase).
type caseCursor struct {
	cases []ClientCase
	used  []bool
}

// PairCase is the recorded request of cases that live, a request of the
// test that reached the application now, pairs with, and its index: the
// first of cases not yet paired (used) whose key (CaseKey) is live's. So the
// n-th request of the run with a key pairs with the n-th recorded request of
// the test with that key, whatever the test's other requests did between
// them. ok is false where none is left: the run sent the call more times
// than its recording did, or a call its recording never made.
func PairCase(cases []ClientCase, used []bool, live *models.HTTPReq) (int, bool) {
	if live == nil {
		return -1, false
	}
	want := CaseKey(live.Method, live.URL)
	for i, c := range cases {
		if i < len(used) && used[i] || c.Req == nil {
			continue
		}
		if CaseKey(c.Req.Method, c.Req.URL) == want {
			return i, true
		}
	}
	return -1, false
}

// IncomingRequest is the client test's request live, which reached the
// application in record's full round, beside the one rec it pairs with in
// the test's recording (test case name), as a keploy test case's request
// is read (models.TestRequest; GivenRequest): its URL, each header and its
// body, as recorded and as sent, and where it reached the recorded
// application (RecordedAt). A body kept apart from the test case, and a
// form, keploy does not read here: the request says why (Unread).
func IncomingRequest(name string, rec, live *models.HTTPReq) *models.TestRequest {
	out := &models.TestRequest{Test: name}
	if rec == nil || live == nil {
		out.Unread = "keploy holds no copy of it"
		return out
	}
	out.RecordedAt = rec.Timestamp
	switch {
	case rec.BodyRef.Path != "" || live.BodyRef.Path != "":
		out.Unread = "its body is kept apart from the test case, which keploy does not read for a client test's request"
		return out
	case len(rec.Form) > 0 || len(live.Form) > 0:
		out.Unread = "it sends a form, which keploy does not read for a client test's request"
		return out
	}
	add := func(name, recorded, sent string) {
		out.Parts = append(out.Parts, models.TestRequestPart{Name: name, Recorded: recorded, Sent: sent})
	}
	add("url", rec.URL, live.URL)
	names := make([]string, 0, len(rec.Header)+len(live.Header))
	for k := range rec.Header {
		names = append(names, k)
	}
	for k := range live.Header {
		if _, ok := rec.Header[k]; !ok {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		add("header."+k, rec.Header[k], live.Header[k])
	}
	add("body", rec.Body, live.Body)
	if rec.Binary != "" || live.Binary != "" {
		add("binary", rec.Binary, live.Binary)
	}
	return out
}

// arrivedBefore is earlier without each client test's request
// (Tracker.Incoming) that did not reach the application before the call in
// both runs: in its recording at or before rec (the call's recorded
// instant), and in record's full round at or before live (the real instant
// the call reached keploy). One whose recorded instant is not known, or
// whose call's is not (the zero time, which every request is after), is
// left out too: keploy cannot show the client sent it before the call.
// earlier is returned as it is where none is left out.
func arrivedBefore(earlier []Earlier, rec, live time.Time) []Earlier {
	var out []Earlier
	for i, e := range earlier {
		keep := !e.incoming || !e.arrivedRec.IsZero() && !e.arrivedRec.After(rec) && !e.arrivedLive.After(live)
		if keep {
			if out != nil {
				out = append(out, e)
			}
			continue
		}
		if out == nil {
			out = append(make([]Earlier, 0, len(earlier)-1), earlier[:i]...)
		}
	}
	if out == nil {
		return earlier
	}
	return out
}

// clientCase reports whether mock, a test case's request among the call's
// earlier places, is a client test's (Earlier.incoming, Place.Client).
func (dc *decider) clientCase(mock string) bool {
	for _, e := range dc.in.Earlier {
		if e.Given && e.Mock == mock {
			return e.incoming
		}
	}
	return false
}
