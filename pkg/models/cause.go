package models

// A page's poller (a job bell polling every few seconds) makes its calls on a
// timer, so how many of them fall inside a test, and which test, is a matter of
// timing, not of what the test does. The test's harness, which can see what
// made each call (a browser's request initiator), tells keploy which calls a
// repeating timer made (POST /agent/cause). A record keeps them with the test's
// window (ScopeWindow.Timers); keploy mock record then gives their recordings
// to the set's worker scope (Mapping.Worker, MockEntry.Cause), each with the
// stretch of the set's writes it was recorded in (MockEntry.Stretch), and a
// replay answers such a call from its stretch, never charging it to a test.

// CauseTimer is MockEntry.Cause of a recording a repeating timer made.
const CauseTimer = "timer"

// CauseReq is the body of POST /agent/cause: a call of the open test that its
// harness saw a repeating timer make, named as the recording holds it: its
// method, Host, target (URL path and query), the hex SHA-256 of its body, and
// N, its order (from 0) among the test's calls with those four. Cause is
// CauseTimer.
type CauseReq struct {
	Method string `json:"method"`
	Host   string `json:"host"`
	URL    string `json:"url"`
	Body   string `json:"body"`
	N      int    `json:"n"`
	Cause  string `json:"cause"`
}

// TimerCall is a call of a test's recording that its harness reported a
// repeating timer made (CauseReq), as the test's window keeps it.
type TimerCall struct {
	Method string `json:"method" yaml:"method" bson:"method"`
	Host   string `json:"host" yaml:"host" bson:"host"`
	URL    string `json:"url" yaml:"url" bson:"url"`
	Body   string `json:"body" yaml:"body" bson:"body"`
	N      int    `json:"n" yaml:"n" bson:"n"`
}

// Stretch is where in a set's run a timer's recording answers
// (MockEntry.Stretch): after the write named After was answered (empty: from
// the set's start), and until the write named Until is (empty: to the set's
// end). Only a write changes what a read answers, so every read inside one
// stretch reads the same state; a recording is never answered across a write.
type Stretch struct {
	After string `json:"after,omitempty" yaml:"after,omitempty" bson:"after,omitempty"`
	Until string `json:"until,omitempty" yaml:"until,omitempty" bson:"until,omitempty"`
}
