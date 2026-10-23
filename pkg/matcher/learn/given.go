package learn

import (
	"sort"

	"go.keploy.io/server/v3/pkg/models"
)

// THE TEST CASE'S OWN REQUEST.
//
// A keploy test case's request is what the application was given before any
// call of the test: a value it picks from it by its clock (the next of the
// slots the request lists, a departure after now) moves between record and
// record's full round as a read of the clock does. The echo exists to catch
// such a pick (instants.go): it reads the request as the test's earliest
// place (Earlier.Given), each part as recorded and as keploy test sent it,
// so a pick from it is a variable of the request (which keploy cannot bind,
// and refuses) or, held there only as F saw it, unaccounted for. Nothing
// else reads it: the request is sent as recorded, so a value that drifted
// was not copied from it.
//
// The replay sends the request with the filter that opens the test case
// (models.MockFilterParams.TestRequest), and the agent gives it to the
// tracker (Tracker.Give). Where it could not be read (a keploy older than its
// agent sent none, or keploy reads no request of the test case's kind), the
// place says so (Earlier.Unread), and a changed instant the echo would have
// read it for is refused rather than learned as noise (ClassifyIn).

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
// opened it carries it (models.TestRequest), as the earlier place the echo
// reads (Earlier.Given): each part a leaf, recorded and as sent, and a JSON
// body's leaves too, ahead of the body whole, so a reason names the leaf. A
// nil req (a keploy older than its agent sent none), or one keploy test
// could not read (TestRequest.Unread), is a place that could not be read
// (Earlier.Unread), with no leaves.
func GivenRequest(req *models.TestRequest) Earlier {
	if req == nil {
		return Earlier{Mock: givenPlace(""), Given: true, Unread: unsentRequest}
	}
	e := Earlier{Mock: givenPlace(req.Test), Given: true, Unread: req.Unread}
	if req.Unread != "" {
		return e
	}
	for _, part := range req.Parts {
		if part.Name == "body" {
			e.Seen = append(e.Seen, jsonSeen(part.Recorded, part.Sent)...)
		}
		e.Seen = append(e.Seen, Seen{Side: Request, Leaf: Leaf{Path: part.Name, Noise: part.Name, Type: String, Value: part.Recorded}, Live: part.Sent})
	}
	return e
}

// jsonSeen are the leaves of a JSON body, recorded and as sent, by path; none
// for a body that is not JSON.
func jsonSeen(recorded, sent string) []Seen {
	rv, ok := DecodeJSON([]byte(recorded))
	if !ok {
		return nil
	}
	rec := map[string]Leaf{}
	JSONLeaves("body", rv, rec)
	live := map[string]Leaf{}
	if lv, ok := DecodeJSON([]byte(sent)); ok {
		JSONLeaves("body", lv, live)
	}
	paths := make([]string, 0, len(rec))
	for p := range rec {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]Seen, 0, len(paths))
	for _, p := range paths {
		out = append(out, Seen{Side: Request, Leaf: rec[p], Live: live[p].Value})
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

// unreadRequestWhy is why a drifted value holding an instant is refused
// where the test case's request could not be read (why).
func unreadRequestWhy(why string) string {
	return "it holds an instant, and keploy could not read the test case's own request (" + why + "), where a moment of it may stand: it may be a pick from what the application was given, by its clock, and keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// requestHolds names where the test case's own request (Earlier.Given), as
// recorded, holds one of lost, the moments a drifted value was recorded with
// and lost, at the value's own precision (covers), by an instant that names
// none of live, the moments F saw in it: "test case <name> at <part>", ""
// where none does. The value may be a pick from the request by the
// application's clock whose pick in F the request does not list (a slot
// after the last, none), which no echo finds; or one from a part a template
// value changed, which the learn step cannot follow.
func (dc *DriftCache) requestHolds(lost, live []moment, served []Earlier) string {
	if len(lost) == 0 {
		return ""
	}
	for _, e := range served {
		if !e.Given {
			continue
		}
		for _, s := range e.Seen {
			var held heldMoments
			dc.heldIn(&held, s.Leaf.Value, dc.rawExtra(s.Leaf.Value))
			for _, h := range held.list {
				if coversAny(h, lost) && !coversAny(h, live) {
					return e.Mock + " at " + s.Leaf.Path
				}
			}
		}
	}
	return ""
}

// coversAny reports whether held names one of ms (covers).
func coversAny(held moment, ms []moment) bool {
	for _, m := range ms {
		if covers(held, m) {
			return true
		}
	}
	return false
}

// requestHoldsWhy is why a drifted value is refused whose recorded moment
// stands at where, in the test case's own request as keploy test sent it
// (requestHolds).
func requestHoldsWhy(where string) string {
	return "the moment it was recorded with stands in " + where + ", which keploy test sent as recorded, while record's full round saw another value in it: it may be a pick from the test case's request, by the application's clock, and keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}
