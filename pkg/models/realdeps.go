package models

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoRealRelay is an agent's refusal of a mock.real list when its proxy
// cannot relay real destinations: a run that lists any fails, instead of
// recording or mocking them.
var ErrNoRealRelay = errors.New("this keploy agent's proxy cannot relay mock.real destinations")

// ErrNoRealRoute is the CLI's error for an agent that has no mock.real route
// at all (an agent of another build): it answered 404.
var ErrNoRealRoute = errors.New("this keploy agent has no mock.real route")

// RealDepsReq is a run's keploy.yml mock.real, which the CLI hands its agent
// when the run starts (design v14 §4): the destinations every connection of
// the run reaches for real, never recorded and never mocked. Sent only under
// the proven switch.
type RealDepsReq struct {
	Real []string `json:"real"`
	// Keep: a later push of the same run (keploy test's next test set). An
	// agent that already holds this list keeps what the run saw; one that
	// holds another, or none (a replacement), installs it afresh.
	Keep bool `json:"keep,omitempty"`
}

// RealUse is a test's use of a mock.real destination: the connections that
// sent it bytes while the test was open, and the dials to it that failed then
// (design v14 §4's REAL row: `REAL <id>: <dest> (<n> connections, <f>
// failed)`).
type RealUse struct {
	// Dest is the destination as mock.real lists it, normalised: for a name
	// entry the name keploy read, with its port.
	Dest   string `json:"dest" yaml:"dest"`
	Conns  int    `json:"conns" yaml:"conns"`
	Failed int    `json:"failed,omitempty" yaml:"failed,omitempty"`
	// H2: a TLS connection to it offered h2, which can carry other names
	// under one certificate.
	H2 bool `json:"h2,omitempty" yaml:"h2,omitempty"`
}

// Capture-incomplete classes (design v14 §3.2). Each is the letter the design
// gives it.
const (
	// CaptureIncompleteRealName (e): a connection to a mock.real name
	// entry's address whose name keploy could not read, or one made real by
	// its name that carried a name mock.real does not list.
	CaptureIncompleteRealName = "e"
)

// CaptureIncomplete is a connection keploy could neither capture nor relay as
// a declared real dependency, and why (design v14 §3.2): evidence that fails
// the test it is charged to, as a miss does.
type CaptureIncomplete struct {
	Class       string    `json:"class" yaml:"class"`
	Destination string    `json:"destination,omitempty" yaml:"destination,omitempty"`
	Why         string    `json:"why" yaml:"why"`
	At          time.Time `json:"at,omitzero" yaml:"at,omitempty"`
	// Hosts are the host names Why names whole (a connection's other name:
	// its Host header, its SNI, its CONNECT target), each where the agent
	// wrote it, with the tag the agent's own lines name it by (LossHost),
	// as CaptureLoss.Hosts are for its What; HostsListed says the agent
	// listed them (none, for a Why that names none). An agent that
	// predates them lists none, and a log line writes its Why by its size
	// alone (LogWhy).
	Hosts       LossHosts `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	HostsListed bool      `json:"hosts_listed,omitempty" yaml:"hosts_listed,omitempty"`
}

// LogWhy is c's Why as a log line carries it: each host Hosts lists by its
// shape and tag at its place (LossHosts.LogText), nothing else read as a
// host; a Why whose hosts the agent did not list (an agent that predates
// them) by its size alone (UnlistedLogText), never whole. The report names it whole, each host
// beside its tag (RealUncapturedLosses, CaptureLosses.Text).
func (c CaptureIncomplete) LogWhy() string {
	switch {
	case c.Why == "":
		return ""
	case c.HostsListed:
		return c.Hosts.LogText(c.Why)
	}
	return UnlistedLogText(c.Why)
}

// ReportWhy is c's Why as the report and the console name it: each host it
// names whole beside its tag (LossHosts.ReportText).
func (c CaptureIncomplete) ReportWhy() string { return c.Hosts.ReportText(c.Why) }

// UntaggedIncomplete is inc with no host tag (LossHosts.Untagged): the
// evidence a test's harness and the receipts are given, which outlive the
// session a tag names a host in. nil for none.
func UntaggedIncomplete(inc []CaptureIncomplete) []CaptureIncomplete {
	if inc == nil {
		return nil
	}
	out := make([]CaptureIncomplete, len(inc))
	for i, c := range inc {
		c.Hosts = c.Hosts.Untagged()
		out[i] = c
	}
	return out
}

// Untagged is r with no host tag (UntaggedIncomplete), for a receipt.
func (r RealRun) Untagged() RealRun {
	r.Incomplete = UntaggedIncomplete(r.Incomplete)
	return r
}

// RealRun is what a run's agent saw of mock.real beyond any one test.
type RealRun struct {
	// Reached are the mock.real destinations any process of the run made a
	// successful connection to, in a test or outside one (design v14 §8.2
	// rule 4).
	Reached []string `json:"reached,omitempty" yaml:"reached,omitempty"`
	// Outside are those first reached in this read while no test was open
	// (a dial that connected, or an answer on a pooled connection): the app
	// talked to them on the run's start time, not on the real clock (design
	// v14 §3.4, F6).
	Outside []string `json:"outside,omitempty" yaml:"outside,omitempty"`
	// Incomplete is capture_incomplete evidence charged to no test: it
	// touched none (made before the first, between two, or after the last,
	// design v14 §3.2 as amended for class e), it was a test's that never
	// ended (left open, or interrupted), or no test ended while it was
	// undecided.
	Incomplete []CaptureIncomplete `json:"incomplete,omitempty" yaml:"incomplete,omitempty"`
}

// RealTest is what one keploy test case did with mock.real, read at its end
// (GET /agent/real/test): its REAL rows, and the capture_incomplete evidence
// charged to it.
type RealTest struct {
	Real       []RealUse           `json:"real,omitempty"`
	Incomplete []CaptureIncomplete `json:"incomplete,omitempty"`
}

// RealUncapturedLosses is capture_incomplete evidence of class e as a capture
// loss (CaptureLossRealUncaptured): one entry, counting the connections, whose
// What names each destination once with what to change, the first few in
// order, with the hosts each Why names (Hosts) at their places in What,
// listed where every Why it names had its hosts listed. nil for none.
func RealUncapturedLosses(inc []CaptureIncomplete) CaptureLosses {
	if len(inc) == 0 {
		return nil
	}
	const most = 3
	type key struct{ dest, why string }
	seen := map[key]bool{}
	var what []string
	var hosts LossHosts
	listed := true
	more := 0
	for _, c := range inc {
		k := key{c.Destination, c.Why}
		if seen[k] {
			continue
		}
		seen[k] = true
		if len(what) == most {
			more++
			continue
		}
		// Destination is the address the connection was made to (the
		// agent decides mock.real at the address the app dialed), never a
		// name: only the Why names one, each host at its place after what
		// goes before the Why.
		at := len(c.Destination) + len(": ")
		for _, w := range what {
			at += len(w) + len("; ")
		}
		what = append(what, c.Destination+": "+c.Why)
		hosts, listed = hosts.With(c.Hosts.Shifted(at)), listed && c.HostsListed
	}
	if more > 0 {
		what = append(what, fmt.Sprintf("and %d more", more))
	}
	return CaptureLosses{{Kind: CaptureLossRealUncaptured, Count: uint64(len(inc)), What: strings.Join(what, "; "), Hosts: hosts, HostsListed: listed}}
}
