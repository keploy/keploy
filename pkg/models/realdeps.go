package models

import (
	"errors"
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
	// Outside are those first reached while no test was open: they ran on
	// the run's start time, not on the real clock (design v14 §3.4, F6).
	Outside []string `json:"outside,omitempty" yaml:"outside,omitempty"`
	// Incomplete is capture_incomplete evidence charged to no test: it came
	// after the run's last test ended.
	Incomplete []CaptureIncomplete `json:"incomplete,omitempty" yaml:"incomplete,omitempty"`
}
