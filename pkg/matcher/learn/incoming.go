package learn

import (
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
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
// round, paired with the request the test sent in its recording (by method
// and path, then by its words, then by order: PairCase), is an earlier place
// of the test's calls from its arrival (Tracker.Incoming), read as a keploy test case's request is
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
// test that reached the application now, pairs with, and its index: one of
// cases not yet paired (used) whose key (CaseKey) is live's. ok is false
// where none is left: the run sent the call more times than its recording
// did, or a call its recording never made.
//
// Where one is left, live pairs with it. Where more are, the test sent the
// call more than once, and a run may send them in another order than its
// recording did (two workers of the test, each with its own request): live
// pairs with the one whose recording holds the most of live's words
// (pairWords: of its URL, and of its body where body says keploy read it),
// each counted once, and with the first of those in the recording's order.
// A word every request left holds tells none from another, and one none
// holds (a value this run made: a name from its clock, an id) is no nearer
// one than another. So requests that send the same words but for the values
// their run made pair by order, the n-th of the run with the n-th of the
// recording, as do the requests of a test that keploy cannot tell apart; a
// request paired with another's recording binds no variable there (the
// recording does not hold the variable's value at its place) or a value no
// call carries, and the call misses.
func PairCase(cases []ClientCase, used []bool, live *models.HTTPReq, body bool) (int, bool) {
	if live == nil {
		return -1, false
	}
	want := CaseKey(live.Method, live.URL)
	var left []int
	for i, c := range cases {
		if i < len(used) && used[i] || c.Req == nil {
			continue
		}
		if CaseKey(c.Req.Method, c.Req.URL) == want {
			left = append(left, i)
		}
	}
	switch len(left) {
	case 0:
		return -1, false
	case 1:
		return left[0], true
	}
	sent := map[string]bool{}
	pairWords(live, body, func(w string) { sent[w] = true })
	best, most := left[0], 0
	for _, i := range left {
		held := map[string]bool{}
		pairWords(cases[i].Req, body, func(w string) {
			if sent[w] {
				held[w] = true
			}
		})
		if len(held) > most {
			best, most = i, len(held)
		}
	}
	return best, true
}

// pairWordsBytes is the most of a request's body PairCase reads the words
// of: two requests of a call that differ only past it pair by order.
const pairWordsBytes = 64 << 10

// pairWords hands each word of what tells req from another request of its
// call: of its URL, and, where body says keploy read it, of the first
// pairWordsBytes of its body and of the values of its form's parts. Its
// headers are not read: they hold what every run makes anew (a date, a
// trace id, a signature).
//
// A word here is a whole value as its text delimits it: a run of bytes
// with no space, quote, bracket, comma, nor a mark that parts a URL's or a
// form's values (pairWordEnds) among them. So an instant, an id or a name a
// run made is one word, which no other run's holds: a word of letters and
// digits (eachWord) would cut it into pieces (a second, a hex group) that
// another run's value holds by chance, pairing the request with a
// recording that is not its own.
func pairWords(req *models.HTTPReq, body bool, each func(string)) {
	words := func(s string) {
		for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r <= ' ' || strings.ContainsRune(pairWordEnds, r) }) {
			each(w)
		}
	}
	words(req.URL)
	if !body {
		return
	}
	text := req.Body
	if len(text) > pairWordsBytes {
		text = text[:pairWordsBytes]
	}
	words(text)
	for _, f := range req.Form {
		for _, v := range f.Values {
			words(v)
		}
	}
}

// pairWordEnds are the marks, beside a space and a control byte, that end a
// word of pairWords.
const pairWordEnds = "\"'`,;&=?/{}[]()<>"

// IncomingRequest is the client test's request live, which reached the
// application in record's full round, beside the one rec it pairs with in
// the test's recording (test case name), as a keploy test case's request
// is read (models.TestRequest; GivenRequest): its URL, each header and its
// body, as recorded and as sent, and where it reached the recorded
// application (RecordedAt), and the text parts of a multipart form
// (clientFormParts). A body kept apart from the test case, and a form part
// that sends a file, keploy does not read here: the request says why
// (Unread).
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
	}
	form, why := clientFormParts(rec.Form, live.Form)
	if why != "" {
		out.Unread = why
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
	out.Parts = append(out.Parts, form...)
	return out
}

// clientFormParts are the text parts of a client test's multipart form, as
// recorded (rec) and as sent (live), as a keploy test case's are read
// (replay's testRequest): the n-th part (from 0) of a name is
// "form.<name>", then "form.<name>#<n+1>", its values one per line, and a
// part one side alone sends is "" on the other. why says why keploy does not
// read them: a part that sends a file, or one kept apart from the test case,
// whose text keploy does not hold.
func clientFormParts(rec, live []models.FormData) (parts []models.TestRequestPart, why string) {
	type slot struct {
		name           string
		recorded, sent string
	}
	var slots []*slot
	at := map[string]*slot{}
	read := func(form []models.FormData, recorded bool) string {
		seen := map[string]int{}
		for _, f := range form {
			if len(f.FileNames) > 0 || len(f.Paths) > 0 || len(f.FileContents) > 0 {
				return "its form part " + f.Key + " sends a file or a value kept apart from the test case, which keploy does not read for a client test's request"
			}
			name := "form." + f.Key
			if n := seen[f.Key]; n > 0 {
				name += "#" + strconv.Itoa(n+1)
			}
			seen[f.Key]++
			s := at[name]
			if s == nil {
				s = &slot{name: name}
				at[name] = s
				slots = append(slots, s)
			}
			if recorded {
				s.recorded = strings.Join(f.Values, "\n")
			} else {
				s.sent = strings.Join(f.Values, "\n")
			}
		}
		return ""
	}
	if why := read(rec, true); why != "" {
		return nil, why
	}
	if why := read(live, false); why != "" {
		return nil, why
	}
	for _, s := range slots {
		parts = append(parts, models.TestRequestPart{Name: s.name, Recorded: s.recorded, Sent: s.sent})
	}
	return parts, ""
}

// arrivedBefore is earlier without each client test's request
// (Tracker.Incoming) that did not reach the application before the call in
// both runs: in its recording at or before rec (the call's recorded
// instant), and in record's full round at or before live (the real instant
// the call reached keploy). One whose recorded instant is not known, or
// whose call's is not (the zero time, which every request is after), is
// left out too: keploy cannot show the client sent it before the call.
// One whose body was still coming as its place was read is read as the
// places it is by now (comingRequest.places). earlier is returned as it is
// where none is left out and none was coming.
func arrivedBefore(earlier []Earlier, rec, live time.Time) []Earlier {
	var out []Earlier
	for i, e := range earlier {
		keep := !e.incoming || !e.arrivedRec.IsZero() && !e.arrivedRec.After(rec) && !e.arrivedLive.After(live)
		if keep && e.coming == nil {
			if out != nil {
				out = append(out, e)
			}
			continue
		}
		if out == nil {
			out = append(make([]Earlier, 0, len(earlier)+1), earlier[:i]...)
		}
		if keep {
			out = append(out, e.coming.places(e)...)
		}
	}
	if out == nil {
		return earlier
	}
	return out
}

// ofOwnCall is earlier without each client test's request (Earlier.incoming)
// where in's call is no call of its test's own list: one keploy record
// marked trailing a test (Input.Trails: a worker: recording, which no test
// lists), or one mappings.yaml places outside the test's list (Input.Scope,
// where it tells). A client test's request is a place of the test's own
// calls only: a value bound there is a variable of the test whose list holds
// the call (models.CaseBind, on that test's mappings entry), and a call no
// test lists reads none. Such a call reads its words as main does, with no
// client's request among their sources: a value the trailed test's own call
// sent stays bound there, as the run's variable.
func ofOwnCall(in Input, earlier []Earlier) []Earlier {
	if s := in.ScopeOf(in.Mock); in.Trails.Test == "" && (s == ScopeUnknown || s == ScopeTest(in.Test)) {
		return earlier
	}
	return slices.DeleteFunc(slices.Clone(earlier), func(e Earlier) bool { return e.incoming })
}

// comingRequest is a client test's request whose body is still coming
// (Tracker.IncomingComing): what a call that reaches keploy meanwhile can
// read of it. The application has its URL and headers whole, and of its body
// no more than what passed to it by the time the call is judged.
type comingRequest struct {
	// head is the request's URL and headers, which keploy read as it
	// arrived, as an earlier place of the test's calls (headPlace).
	head Earlier
	// passed gives the words of the body that passed to the application so
	// far (RequestWords.Passed), of the whole body once it ended; nil where
	// keploy cannot tell them (a body sent encoded that has not ended, one
	// that did not come whole). nil where the request's taker reads none.
	passed func() *RequestWords
}

// places are the places a call reads e as, a request whose body was still
// coming as the call read its test's places (e.coming is c): its URL and
// headers (head), then its body, a place keploy could not read
// (ComingUnread) holding the words that passed of it by now, so a value
// whose change they cannot hold reads the body as no source of it
// (requestUnreadOf), as a call after a request keploy could not read does.
// Where keploy cannot tell those words the body may hold any, and the
// request is one place keploy could not read: every value that changed is
// refused.
func (c *comingRequest) places(e Earlier) []Earlier {
	e.coming = nil
	if c.passed != nil {
		e.unreadWords = c.passed()
	}
	if e.unreadWords.MayHoldAny() {
		e.unreadWords = nil
		return []Earlier{e}
	}
	return []Earlier{c.head, e}
}

// headPlace is the URL and headers of live, a request of a client test that
// reached the application at at, paired with c, its recorded request, as an
// earlier place of the test's calls (incomingPlace), with no body on either
// side: what keploy read of a request whose body is still coming.
func headPlace(c ClientCase, live *models.HTTPReq, at time.Time) Earlier {
	head := func(r *models.HTTPReq) *models.HTTPReq {
		return &models.HTTPReq{Method: r.Method, ProtoMajor: r.ProtoMajor, ProtoMinor: r.ProtoMinor, URL: r.URL, URLParams: r.URLParams, Header: r.Header, Timestamp: r.Timestamp}
	}
	req := IncomingRequest(c.Name, head(c.Req), head(live))
	e := GivenRequest(req)
	e.incoming, e.arrivedRec, e.arrivedLive = true, req.RecordedAt, at
	return e
}

// clientCase reports whether mock, a place among the call's earlier places
// (by its name), is a client test's request (Earlier.incoming,
// Place.Client): a keploy test case's request, a value the test gave and a
// recording are not.
func (dc *decider) clientCase(mock string) bool {
	for _, e := range dc.in.Earlier {
		if e.Mock == mock {
			return e.incoming
		}
	}
	return false
}

// readOverClients is served without the requests of client tests
// (Earlier.incoming) where the call's own window w holds each of pairs, the
// changed instants of a value as recorded and as round F saw them, as a read
// of the application's clock in both runs (Window.holdsRead, in zone). That
// read explains the value at its cut, as main reads the call: a client's
// request that holds the same moment (a token signed in that second, a Date
// header it uploads, the digits of a UnixNano name it made) holds it by the
// coincidence of one second, and is no source beside the read, so no pick,
// copy, two sources or source outside the scope comes from it. The value is
// still a Clock that replay compares in its own window, so a wrong reading
// fails as a miss, never as a pass. Where no read explains every pair (no
// window, a moment outside it, no pair), served is as given: the client's
// request stays a source of the moment (a sign-in time a token carries), and
// a pick from it is refused. Words are not read here: a client's request
// stays a source of the words it holds.
func readOverClients(served []Earlier, w *Window, pairs []driftedInstant, zone *time.Location) []Earlier {
	if len(pairs) == 0 || !slices.ContainsFunc(served, func(e Earlier) bool { return e.incoming }) {
		return served
	}
	for _, p := range pairs {
		if !w.holdsRead(p.rec, p.live, zone) {
			return served
		}
	}
	return slices.DeleteFunc(slices.Clone(served), func(e Earlier) bool { return e.incoming })
}
