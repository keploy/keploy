package learn

import (
	"sort"
	"strings"
	"sync"
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

// incomingRead is a client test's incoming requests as one read of the
// tracker saw them (Tracker.Placed): the test's name, its recorded requests
// in the order they were sent, the live ones the application was sent so
// far, and whether the agent sees them (IncomingGiven's arguments, but the
// call's instant). A read's places are shared by every call that reads it
// (a round's, exact_round), and each call's recorded instant cuts the
// recorded requests at its own place (at): a request recorded after the
// call never gives it a value, nor splits one it holds. Each cut is read
// once per read.
type incomingRead struct {
	test           string
	recorded, live []IncomingRequest
	seen           bool

	mu    sync.Mutex
	byCut map[int]Earlier
}

// newIncomingRead is the read of test's recorded and live requests (recorded
// already in the order they were sent, sortIncoming).
func newIncomingRead(test string, recorded, live []IncomingRequest, seen bool) *incomingRead {
	return &incomingRead{test: test, recorded: recorded, live: live[:len(live):len(live)], seen: seen}
}

// at is the place (IncomingGiven) at a call recorded at callAt; the zero
// callAt reads every recorded request.
func (r *incomingRead) at(callAt time.Time) Earlier {
	cut := len(r.recorded)
	if !callAt.IsZero() {
		cut = sort.Search(len(r.recorded), func(i int) bool { return r.recorded[i].At.After(callAt) })
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byCut[cut]; ok {
		return e
	}
	e := IncomingGiven(r.test, r.recorded[:cut], r.live, r.seen, time.Time{})
	e.in = r
	if r.byCut == nil {
		r.byCut = map[int]Earlier{}
	}
	r.byCut[cut] = e
	return e
}

// sortIncoming is reqs in the order they were sent, those sent at one
// instant in the order given (as IncomingGiven orders them).
func sortIncoming(reqs []IncomingRequest) []IncomingRequest {
	out := append([]IncomingRequest(nil), reqs...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// incomingAt is earlier with a client test's incoming requests (an Earlier
// a Tracker read made, Earlier.in) read as the call recorded at at had them
// (incomingRead.at): only those recorded before it. earlier itself is never
// changed (a round's calls share it).
func incomingAt(earlier []Earlier, at time.Time) []Earlier {
	if at.IsZero() {
		return earlier
	}
	var out []Earlier
	for i, e := range earlier {
		if e.in == nil {
			continue
		}
		if out == nil {
			out = append([]Earlier(nil), earlier...)
		}
		out[i] = e.in.at(at)
	}
	if out == nil {
		return earlier
	}
	return out
}

// IncomingValueAt is the value the requests live (those the application was
// sent in this run in a client test's window, before a call) hold at place,
// a leaf path IncomingGiven names ("header.authorization{jwt}.auth_time"):
// a replay's read of a variable the learn step found there
// (models.MockVariable.Incoming). Each request is read as IncomingGiven
// reads it, so the two read one part alike. why says why there is none:
// no request was sent, none holds the part, or two hold two values of it
// (keploy cannot tell which one the call carries). A value is never
// guessed.
func IncomingValueAt(live []IncomingRequest, place string) (value, why string) {
	if len(live) == 0 {
		return "", "the application was sent no incoming request in this test before this call"
	}
	held := false
	for _, r := range live {
		leaves, _, _ := incomingLeaves([]IncomingRequest{r})
		l, ok := leaves[place]
		if !ok {
			continue
		}
		if held && l.Value != value {
			return "", "two of the incoming requests the application was sent in this test before this call hold two values at " + place + ", so keploy cannot tell which one the call carries"
		}
		value, held = l.Value, true
	}
	if !held {
		return "", "none of the incoming requests the application was sent in this test before this call holds " + place
	}
	return value, ""
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
