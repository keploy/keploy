package learn

import (
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A CLIENT TEST'S INCOMING REQUESTS.
//
// keploy mock's tests are a test harness's (SetClientTests): no keploy test
// case's request reaches the agent, so a value the harness gave the
// application (the auth_time claim of the bearer token it signed for its
// session) has no given place (Earlier.Given), and a call that carries it
// on (inside the token the application mints from it) is refused where it
// changed between the recording and this run. The application's incoming
// requests are what the harness gave it: keploy mock records them as test
// cases (record-requests) and the agent sees each again in a replay. Read in
// the test's window, before the call, they are the test's given place
// (IncomingGiven): a value one of their parts holds, the same in each of
// them that holds that part, as recorded and in this run, came from the
// harness, as a template value of a test case's request does.
//
// Bounds. Only the requests of the test's own window count, never another
// test's or the set's start-up's; as recorded, only those sent before the
// call's recorded instant; in this run, those the application was sent
// before the call. A part two requests of a side hold two values of is no
// place (keploy cannot tell which request the value came from); nor is a
// part one side alone holds. Only a request's URL and headers are read (a
// header's JWT claims too), never its body, which the application reads
// after keploy hands it on. Where the agent did not see this run's incoming
// requests (record-requests off, or no incoming stream), or the test sent
// none before the call in either run, the place could not be read
// (Earlier.Unread): it gives no place, so no variable is bound there, and
// refuses nothing by itself (requestUnread), a client test's own rules
// standing as where keploy reads no incoming request.

// IncomingRequest is one request the application was sent in a client
// test's window: as keploy mock recorded it (a test case of the set's), or
// as the agent saw it in this run.
type IncomingRequest struct {
	// Name is the test case keploy recorded the request as, "" for one
	// of this run.
	Name string
	// At is when the application was sent it.
	At time.Time
	// Parts are its URL ("url") and headers ("header.<name>"), each one
	// value, as models.TestRequest names them; a body is not read.
	Parts []models.TestRequestPart
}

// IncomingHTTP is an HTTP request the application was sent (name, the test
// case keploy recorded it as, "" for one of this run; at, when) as an
// IncomingRequest: its URL ("url") and each header ("header.<name>", its
// lines joined as a recorded test case joins them), in the order of their
// names, so a request recorded and the same request sent in this run give
// the same parts.
func IncomingHTTP(name string, at time.Time, url string, header map[string]string) IncomingRequest {
	r := IncomingRequest{Name: name, At: at, Parts: make([]models.TestRequestPart, 0, len(header)+1)}
	r.Parts = append(r.Parts, models.TestRequestPart{Name: "url", Recorded: url})
	names := make([]string, 0, len(header))
	for k := range header {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		r.Parts = append(r.Parts, models.TestRequestPart{Name: "header." + k, Recorded: header[k]})
	}
	return r
}

// incomingUncaptured is why a client test's incoming requests could not be
// read where the agent saw none of this run's.
const incomingUncaptured = "keploy did not see the application's incoming requests in this run (record-requests is off, or the agent has no incoming stream), so a value its test harness gave it cannot be told from one it made"

// IncomingGiven is the given place (Earlier.Given, Earlier.Incoming) of a
// client test (test, its name) at a call recorded at callAt: the parts of
// the recorded requests of its window sent before callAt (recorded), each
// with its value in the requests the application was sent in this run
// before the call (live; seen false where the agent did not see this run's
// requests). Each request is read as GivenRequest reads a test case's
// request (its URL's path segments and query values, its headers and the
// claims of a JWT one holds), its URL and headers alone. A leaf is kept
// where every request of a side that holds it holds one value, on both
// sides; the place is named by the first recorded request (the earliest)
// that holds a kept leaf. See the section above for the bounds.
func IncomingGiven(test string, recorded, live []IncomingRequest, seen bool, callAt time.Time) Earlier {
	e := Earlier{Mock: incomingPlace(test, ""), Given: true, Incoming: true}
	if !seen {
		e.Unread = incomingUncaptured
		return e
	}
	var before []IncomingRequest
	for _, r := range recorded {
		if callAt.IsZero() || !r.At.After(callAt) {
			before = append(before, r)
		}
	}
	sort.SliceStable(before, func(i, j int) bool { return before[i].At.Before(before[j].At) })
	switch {
	case len(before) == 0:
		e.Unread = "the test sent the application no incoming request before this call in its recording"
		return e
	case len(live) == 0:
		e.Unread = "the test sent the application no incoming request before this call in this run"
		return e
	}
	recLeaves, order, from := incomingLeaves(before)
	liveLeaves, _, _ := incomingLeaves(live)
	first := -1
	for _, p := range order {
		r, ok := recLeaves[p]
		l, lok := liveLeaves[p]
		if !ok || !lok {
			continue
		}
		e.Seen = append(e.Seen, Seen{Side: Request, Leaf: r, Live: l.Value})
		if first < 0 || from[p] < first {
			first = from[p]
		}
	}
	if first < 0 {
		e.Unread = "no part of the test's incoming requests holds one value in each of them, as recorded and in this run"
		return e
	}
	e.Mock, e.At = incomingPlace(test, before[first].Name), before[first].At
	return e
}

// incomingLeaves are the leaves the requests reqs (one side's) hold each
// one value of: a leaf's path to the leaf, the paths in the order the first
// request holding each lists them, and the index of that request. A path
// two requests hold two values of is dropped.
func incomingLeaves(reqs []IncomingRequest) (map[string]Leaf, []string, map[string]int) {
	leaves, from := map[string]Leaf{}, map[string]int{}
	var order []string
	split := map[string]bool{}
	for i, r := range reqs {
		parts := make([]models.TestRequestPart, 0, len(r.Parts))
		for _, p := range r.Parts {
			if p.Name == "url" || strings.HasPrefix(p.Name, "header.") {
				parts = append(parts, models.TestRequestPart{Name: p.Name, Recorded: p.Recorded, Sent: p.Recorded})
			}
		}
		g := GivenRequest(&models.TestRequest{Test: r.Name, Parts: parts})
		for _, s := range g.Seen {
			p := s.Leaf.Path
			if split[p] {
				continue
			}
			if l, ok := leaves[p]; ok {
				if l.Value != s.Leaf.Value {
					split[p] = true
					delete(leaves, p)
				}
				continue
			}
			leaves[p], from[p] = s.Leaf, i
			order = append(order, p)
		}
	}
	kept := order[:0]
	for _, p := range order {
		if !split[p] {
			kept = append(kept, p)
		}
	}
	return leaves, kept, from
}

// incomingPlace names a client test's incoming requests in a reason:
// "incoming request <first> of test <test>", the request the value was
// first sent in as recorded.
func incomingPlace(test, first string) string {
	place := "the test's incoming requests"
	if first != "" {
		place = "incoming request " + first
	}
	if test != "" {
		place += " of test " + test
	}
	return place
}
