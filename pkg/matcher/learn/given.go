package learn

import (
	"net/url"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// THE TEST CASE'S OWN REQUEST.
//
// A keploy test case's request is what the application was given before any
// call of the test, and an earlier place of the test as any call's is
// (Earlier.Given): a value the application took from it (an id it carries
// on, a part of a template value, the next of the slots it lists picked by
// the clock, which moves between record and record's full round as a read
// of the clock does) is no noise. The classifier reads the request, each
// part as recorded and as keploy test sent it, as it reads an exchange F
// served: a value that drifted with a part keploy test sent with a template
// value came from there (sources); a word of what changed that the request
// holds is a place it may have come from (unaccounted); and the echo reads
// its instants (instants.go). Nothing binds a variable at the request, so a
// value found there is refused (the HTTP adapter), never learned as noise;
// nor is anything read back from it (ReadBack): it writes no record. And a
// value whose recorded moment the request, as recorded, holds is refused
// where record's full round saw a value the request, as sent, holds by no
// rule that names the recorded one (requestHolds): a pick from it whose pick
// in F the request does not list (a slot after the last, none).
//
// The replay sends the request with the filter that opens the test case
// (models.MockFilterParams.TestRequest), and the agent gives it to the
// tracker (Tracker.Give). Where it could not be read (a keploy older than its
// agent sent none, or keploy reads no request of the test case's kind), the
// place says so (Earlier.Unread), and every value that changed is refused
// rather than learned as noise or an echo (ClassifyIn): whatever the request
// held, keploy cannot tell what of it the value took.

// givenPlace names a test case's request in a reason: "test case <name>".
func givenPlace(test string) string {
	if test == "" {
		return "the test case's request"
	}
	return "test case " + test
}

// unsentRequest is why a test case's request could not be read when the
// filter that opened it carried none.
const unsentRequest = "keploy test sent the agent no copy of the test case's request, as a keploy older than its agent does"

// unopenedRequest is why it could not be read when the test's calls came
// before the filter that opens it brought it (Tracker.Await), or no such
// filter came.
const unopenedRequest = "the filter that opens the test case had not brought its request to the agent"

// strayRequest is why it could not be read when it came while no test was
// open (Tracker.Give): the test's begin never reached the agent.
const strayRequest = "the test case's request reached the agent while no test was open, so keploy could not tie it to the calls the test made"

// GivenRequest is req, a keploy test case's own request as the filter that
// opened it carries it (models.TestRequest), as an earlier place of the test
// (Earlier.Given), read as the HTTP adapter reads an exchange F served: each
// part a leaf, recorded and as sent; its URL as its path's segments and its
// query's values (url.path.<i>, url.query.<key>), never its scheme, host or
// port, where the application is reached; a JSON body's leaves too, ahead of
// the body whole, so a reason names the leaf, and its object keys
// (KeysLeaf); the names of its query parameters, headers and form fields
// (request{names}); and none of what addresses or frames it (a Host, an
// :authority or a :scheme header, a Content-Length, a Content-Type, a
// header keploy adds). A nil req (a keploy older than its agent sent none),
// or one keploy test could not read (TestRequest.Unread), is a place that
// could not be read (Earlier.Unread), with no leaves.
func GivenRequest(req *models.TestRequest) Earlier {
	if req == nil {
		return Earlier{Mock: givenPlace(""), Given: true, Unread: unsentRequest}
	}
	e := Earlier{Mock: givenPlace(req.Test), Given: true, Unread: req.Unread}
	if req.Unread != "" {
		return e
	}
	recNames, sentNames := map[string]bool{}, map[string]bool{}
	name := func(n, recorded, sent string) {
		// A part one side alone holds is "" on the other.
		if recorded != "" || sent == "" {
			recNames[n] = true
		}
		if sent != "" || recorded == "" {
			sentNames[n] = true
		}
	}
	for _, part := range req.Parts {
		if !givenPart(part.Name) {
			continue
		}
		switch {
		case part.Name == "url":
			if leaves, rn, sn, ok := urlSeen(part.Recorded, part.Sent); ok {
				e.Seen = append(e.Seen, leaves...)
				for _, n := range rn {
					recNames[n] = true
				}
				for _, n := range sn {
					sentNames[n] = true
				}
				continue
			}
		case part.Name == "body":
			e.Seen = append(e.Seen, jsonSeen(part.Recorded, part.Sent)...)
		}
		e.Seen = append(e.Seen, Seen{Side: Request, Leaf: Leaf{Path: part.Name, Noise: part.Name, Type: String, Value: part.Recorded}, Live: part.Sent})
		if n, ok := paramName(part.Name); ok {
			name(n, part.Recorded, part.Sent)
		}
	}
	if len(recNames)+len(sentNames) > 0 {
		e.Seen = append(e.Seen, Seen{Side: Request, Leaf: Leaf{Path: "request{names}", Type: Keys, Value: sortedSet(recNames), Unnamed: true}, Live: sortedSet(sentNames)})
	}
	return e
}

// givenPart reports whether a part of a test case's request is one the
// application is given: none that addresses or frames the request (a Host,
// an :authority or a :scheme header, a Content-Length, a Content-Type) or
// that keploy adds to it (a header named keploy...), as the HTTP adapter
// reads no such header of an exchange.
func givenPart(part string) bool {
	h, ok := strings.CutPrefix(part, "header.")
	if !ok {
		return true
	}
	switch h = strings.ToLower(h); h {
	case "host", ":authority", ":scheme", "content-length", "content-type":
		return false
	}
	return !strings.HasPrefix(h, "keploy")
}

// urlSeen are the leaves of a test case's URL, recorded and as sent: its
// path's segments (url.path.<i>) and its query's values (url.query.<key>, a
// repeated key's values sorted and joined), with the names of its query
// parameters on each side; ok is false for a URL that does not parse, read
// whole then.
func urlSeen(recorded, sent string) (leaves []Seen, recNames, sentNames []string, ok bool) {
	ru, err := url.Parse(recorded)
	if err != nil {
		return nil, nil, nil, false
	}
	lu, lerr := url.Parse(sent)
	segs := func(u *url.URL) []string {
		var out []string
		for _, seg := range strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/") {
			if v, err := url.PathUnescape(seg); err == nil {
				seg = v
			}
			out = append(out, seg)
		}
		return out
	}
	query := func(u *url.URL) map[string]string {
		out := map[string]string{}
		for k, vs := range u.Query() {
			vs = append([]string(nil), vs...)
			sort.Strings(vs)
			out[k] = strings.Join(vs, ", ")
		}
		return out
	}
	rs, rq := segs(ru), query(ru)
	var ls []string
	lq := map[string]string{}
	if lerr == nil {
		ls, lq = segs(lu), query(lu)
	}
	for i, seg := range rs {
		live := ""
		if i < len(ls) {
			live = ls[i]
		}
		p := "url.path." + strconv.Itoa(i)
		leaves = append(leaves, Seen{Side: Request, Leaf: Leaf{Path: p, Noise: p, Type: String, Value: seg}, Live: live})
	}
	keys := make([]string, 0, len(rq))
	for k := range rq {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := "url.query." + k
		leaves = append(leaves, Seen{Side: Request, Leaf: Leaf{Path: p, Noise: p, Type: String, Value: rq[k], Unnamed: strings.ContainsAny(k, ".[]")}, Live: lq[k]})
		recNames = append(recNames, k)
	}
	for k := range lq {
		sentNames = append(sentNames, k)
	}
	return leaves, recNames, sentNames, true
}

// sortedSet is set's members, sorted, one per line.
func sortedSet(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// paramName is the name of the header or form field a test case's request
// part names ("header.<name>", "form.<key>", a repeated key's later parts
// "form.<key>#2"), and ok, false for any other part.
func paramName(part string) (string, bool) {
	if n, ok := strings.CutPrefix(part, "header."); ok {
		return n, true
	}
	if n, ok := strings.CutPrefix(part, "form."); ok {
		if i := strings.LastIndexByte(n, '#'); i >= 0 {
			n = n[:i]
		}
		return n, true
	}
	return "", false
}

// jsonSeen are the leaves of a JSON body, recorded and as sent, by path, and
// its object keys (KeysLeaf) after them; none for a body that is not JSON.
func jsonSeen(recorded, sent string) []Seen {
	rv, ok := DecodeJSON([]byte(recorded))
	if !ok {
		return nil
	}
	rec := map[string]Leaf{}
	JSONLeaves("body", rv, rec)
	live := map[string]Leaf{}
	lv, liveOK := DecodeJSON([]byte(sent))
	if liveOK {
		JSONLeaves("body", lv, live)
	}
	paths := make([]string, 0, len(rec))
	for p := range rec {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]Seen, 0, len(paths)+1)
	for _, p := range paths {
		out = append(out, Seen{Side: Request, Leaf: rec[p], Live: live[p].Value})
	}
	if k, ok := KeysLeaf("body", rv); ok {
		var lk Leaf
		if liveOK {
			lk, _ = KeysLeaf("body", lv)
		}
		out = append(out, Seen{Side: Request, Leaf: k, Live: lk.Value})
	}
	return out
}

// requestUnread says why the running test case's own request could not be
// read (its Given place's Unread), "" when it was read, and when no test
// case's request is among served (a call outside a test, a test keploy mock
// opened).
func requestUnread(served []Earlier) string {
	for _, e := range served {
		if e.Given && e.Unread != "" {
			return e.Unread
		}
	}
	return ""
}

// unreadRequestWhy is why a drifted value is refused where the test case's
// request could not be read (why), and how to correct it: where keploy test
// sent none (a keploy older than its agent), run the two of one version, so
// that keploy test sends it; else, a field the application makes anew every
// run may be declared noise.
func unreadRequestWhy(why string) string {
	fix := "If the field is a value the application makes anew every run (a time it reads from its own clock, a random id), declare it noise in keploy.yml, and keploy stops comparing it"
	if why == unsentRequest {
		fix = "Run keploy record with the keploy agent of its own version, which hands the agent each test case's request"
	}
	return "keploy could not read the test case's own request (" + why + "), what the application was given before any call of the test: the value may have come from there (a part of it, or a pick from it by the application's clock), and keploy cannot tell. " + fix
}

// requestHolds names where the test case's own request (Earlier.Given), as
// recorded, holds one of lost, the moments a drifted value was recorded with
// and lost, by a rule (cuts) by which the request as keploy test sent it
// names none of live, the moments F saw in it: "test case <name> at
// <part>", with sent, whether keploy test sent that part as recorded; "" where
// none does. The value may be a pick from the request by the application's
// clock whose pick in F the request does not list (a slot after the last,
// none), which no echo finds; or one from a part a template value changed,
// which the learn step cannot follow. An instant that names a lost moment by
// one rule and a moment F saw by another (one truncated, one rounded) does
// not explain F's: the application cuts by one rule.
func (dc *DriftCache) requestHolds(lost, live []moment, served []Earlier) (where string, asRecorded bool) {
	if len(lost) == 0 {
		return "", false
	}
	for _, e := range served {
		if !e.Given {
			continue
		}
		// The rules by which the request, as sent, names a moment F saw.
		var sent heldMoments
		for _, s := range e.Seen {
			dc.heldIn(&sent, s.Live, dc.rawExtra(s.Live))
		}
		explained := map[cutRule]bool{}
		for _, h := range sent.list {
			for _, m := range live {
				cuts(h, m, func(c cutRule) bool {
					explained[c] = true
					return false
				})
			}
		}
		for _, s := range e.Seen {
			var held heldMoments
			dc.heldIn(&held, s.Leaf.Value, dc.rawExtra(s.Leaf.Value))
			for _, h := range held.list {
				for _, m := range lost {
					if cuts(h, m, func(c cutRule) bool { return !explained[c] }) {
						return e.Mock + " at " + s.Leaf.Path, s.Live == s.Leaf.Value
					}
				}
			}
		}
	}
	return "", false
}

// requestHoldsWhy is why a drifted value is refused whose recorded moment
// stands at where, in the test case's own request as recorded
// (requestHolds), which keploy test sent as recorded, or with template
// values in it (asRecorded false).
func requestHoldsWhy(where string, asRecorded bool) string {
	sent := "which keploy test sent as recorded"
	if !asRecorded {
		sent = "which keploy test sent with template values in it"
	}
	return "the moment it was recorded with stands in " + where + ", " + sent + ", while record's full round saw another value, which the request as sent holds by no rule that gives the recorded one: it may be a pick from the test case's request, by the application's clock, and keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}
