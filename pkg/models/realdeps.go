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
	// Incomplete is capture_incomplete evidence charged to no test: it came
	// after the run's last test ended.
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
// order. nil for none.
func RealUncapturedLosses(inc []CaptureIncomplete) CaptureLosses {
	if len(inc) == 0 {
		return nil
	}
	const most = 3
	type key struct{ dest, why string }
	seen := map[key]bool{}
	var what []string
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
		what = append(what, c.Destination+": "+c.Why)
	}
	if more > 0 {
		what = append(what, fmt.Sprintf("and %d more", more))
	}
	return CaptureLosses{{Kind: CaptureLossRealUncaptured, Count: uint64(len(inc)), What: strings.Join(what, "; ")}}
}
