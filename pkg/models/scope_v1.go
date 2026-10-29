package models

import "strings"

// The single-worker integration wire of the proven model (design v14 §6.1,
// cut to one test process per run for the first release): a test framework's
// integration claims an instance for its test process, then opens and closes
// each test with /agent/scope/begin and /agent/scope/end. Every answer carries
// V, so an integration fails loudly against an agent that does not speak the
// protocol instead of going inert. It is served only under the proven switch
// (pkg/provenswitch); with the switch off the scope API answers as before.

// ScopeProtocolVersion is the V every answer of the wire carries.
const ScopeProtocolVersion = 1

// TestPIDEnv is the variable a test process's keploy integration exports
// once it has claimed the run: the claimer's pid, so the processes its tests
// start (a helper binary, a fuzz worker) know they belong to a claimed
// process and do not claim the run themselves. keploy removes it from the
// environment of each command it wraps: a value inherited from an enclosing
// keploy run names a process of that run, not of this one.
const TestPIDEnv = "KEPLOY_TEST_PID"

// InstanceReq is the body of POST /agent/instance, the claim a test
// process's keploy integration makes before its first test. Key names the
// pool slice the instance consumes outside tests, and the path its tests' IDs
// carry: for Go, the test binary's package directory relative to the project
// directory. The caller's process is read from the connection, never from
// the body.
type InstanceReq struct {
	Key string `json:"key"`
	// Dir is the test process's working directory, absolute: where its tests
	// run (a Go package's directory). Kept on each test's window, so the
	// recording's mapping names the folder each test runs in.
	Dir string `json:"dir,omitempty"`
}

// InstanceResp answers POST /agent/instance. With one test process per run
// the instance is the run itself. TimeFile is the clock file the test process
// runs on, empty when the run is not frozen.
type InstanceResp struct {
	V        int    `json:"v"`
	Instance string `json:"instance"`
	TimeFile string `json:"timeFile"`
}

// InstanceRun is the InstanceResp.Instance of a single-worker run.
const InstanceRun = "run"

// ScopeEndResp answers POST /agent/scope/end under the wire. A replayed test
// with keploy evidence (a call no recorded answer matched, a recorded call it
// never made) gets Action ScopeActionFail and the Reason; its harness fails
// the test with that text.
type ScopeEndResp struct {
	V      int    `json:"v"`
	Status string `json:"status"`
	Action string `json:"action,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ScopeGateResp acknowledges POST /agent/replay/gate under the wire: the agent
// applies Skips skip-list entries.
type ScopeGateResp struct {
	V      int    `json:"v"`
	Status string `json:"status"`
	Skips  int    `json:"skips"`
}

// ScopeActionFail is the ScopeEndResp.Action telling a harness to fail the test.
const ScopeActionFail = "fail"

// ScopeError is the body of every non-2xx answer of the wire. Code is one of
// the ScopeErr* constants; Message says what happened and Remedy what to do.
type ScopeError struct {
	Code    string `json:"error"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
	// Status is the HTTP status the error is answered with; not sent.
	Status int `json:"-"`
}

// RunRefusal is what keploy refused during a run under the proven wire: a
// test process's claim, a test's begin, or what a test framework's
// integration refused and told keploy (POST /agent/refusal). The run fails,
// naming it, with its own reason.
type RunRefusal struct {
	Code    string `json:"code" yaml:"code"`
	Message string `json:"message" yaml:"message"`
	Remedy  string `json:"remedy,omitempty" yaml:"remedy,omitempty"`
}

// Refusal is e as a RunRefusal.
func (e *ScopeError) Refusal() RunRefusal {
	return RunRefusal{Code: e.Code, Message: e.Message, Remedy: e.Remedy}
}

func (e *ScopeError) Error() string {
	if e.Remedy == "" {
		return e.Message
	}
	return e.Message + " (" + e.Remedy + ")"
}

// The ScopeError codes.
const (
	// ScopeErrUpgradeIntegration: the harness predates the wire (no canSkip),
	// or uses a call the wire no longer has (a suite scope, an app start).
	ScopeErrUpgradeIntegration = "upgrade_integration"
	// ScopeErrOneTestAtATime: a test began while another test of the run was
	// open, in-process concurrency the integration did not serialize.
	ScopeErrOneTestAtATime = "one_test_at_a_time"
	// ScopeErrNotInRun: a process outside the run's own processes claimed
	// it: keploy does not intercept it.
	ScopeErrNotInRun = "not_in_run"
	// ScopeErrOneTestProcess: a test process claimed the run while another
	// live one held it; the first release runs one at a time.
	ScopeErrOneTestProcess = "one_test_process_at_a_time"
	// ScopeErrNotOpen: an end for a test that is not open.
	ScopeErrNotOpen = "not_open"
	// ScopeErrBadRequest: an unreadable or invalid body.
	ScopeErrBadRequest = "bad_request"
	// ScopeErrNoCaller: the agent could not tell which process called it.
	ScopeErrNoCaller = "caller_unknown"
	// ScopeErrInternal: the agent failed to act on a valid call.
	ScopeErrInternal = "internal"
)

// ScopeEvidence is what keploy saw of one replayed test that fails it: the
// calls no recorded answer matched while it was open, or since the previous
// test ended (a miss outside every test is charged to the next test that
// begins), and the recorded data-plane calls of its mapping it never made.
// Misses carry only the summary fields; never a recorded value.
type ScopeEvidence struct {
	Misses     []UnmatchedCall `json:"misses,omitempty" yaml:"misses,omitempty"`
	Unconsumed []string        `json:"unconsumed,omitempty" yaml:"unconsumed,omitempty"`
	// Background lists, in recorded order, the test's own recordings judging
	// marked as the application's background work (MockEntry.Background) that
	// it did not call while it was open. Never a failure, and never in
	// Unconsumed: it tells judging which ones a replay did not use.
	Background []string `json:"background,omitempty" yaml:"background,omitempty"`
	// ServedAside lists, in the order they answered, the calls of this test's
	// window that a set-aside test's recording answered (ServedAside): a test
	// the replay skipped made no call, so what its recordings answer (a cache
	// fill another test then makes) is served to the calls after it, by exact
	// match only, unchecked and never required. Each reads "<mock> served
	// from <test> (set aside)". Never a failure, and never part of Empty.
	ServedAside []string `json:"servedAside,omitempty" yaml:"servedAside,omitempty"`
	// CaptureIncomplete is what the agent could not capture since the run
	// began, when the test ended (design v14 §3.2): it taints the test open
	// when it happened and every later one, which therefore fail in a replay.
	CaptureIncomplete CaptureLosses `json:"captureIncomplete,omitempty" yaml:"captureIncomplete,omitempty"`
	// Incomplete is capture_incomplete evidence of class e (design v14 §3.2,
	// §4): a connection keploy could neither capture nor relay as real.
	Incomplete []CaptureIncomplete `json:"incomplete,omitempty" yaml:"incomplete,omitempty"`
	// CannotCapture is what keploy cannot capture at all that the application
	// did while this test or one before it ran (decision 20: a call over a
	// Unix socket): the test has no verdict. Never a failure.
	CannotCapture CaptureLosses `json:"cannotCapture,omitempty" yaml:"cannotCapture,omitempty"`
	// Reason is the text the test was failed with at its end.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Reported is, in a replay that failed the test for this evidence, the
	// verdict it ended with before keploy did (models.ScopeOutcome*): its
	// harness's (ScopeReq.Outcome), or failed when keploy ended it (its
	// process exited, or another test's end closed it). Judging reads it to
	// tell a test whose own assertions passed and that only left recordings
	// unmade from one that failed its own. "" in a record, which never fails
	// a test, and on evidence that failed nothing. Never part of Empty.
	Reported string `json:"reported,omitempty" yaml:"reported,omitempty"`
	// Unread: keploy could not read everything the test did (its misses,
	// or the mocks it consumed), so the test has no verdict of its own;
	// keploy's verdict on the run is "not judged", never the test's fault.
	Unread bool `json:"unread,omitempty" yaml:"unread,omitempty"`
}

// ServedAsideLine is how ScopeEvidence.ServedAside names one call a
// set-aside test's recording answered: the recording and its test.
func ServedAsideLine(mock, from string) string {
	return mock + " served from " + from + " (set aside)"
}

// escapeTestName escapes a test's name for its ID (TestID), so that the ID
// never holds "::" after its path and no two names share an ID. A name with
// "::", a leading ":", "%25" or "%3A" is escaped: "%" is written "%25", then
// "::" "%3A%3A", then a leading ":" "%3A". Any other name is its own escape,
// and holds neither "%25" nor "%3A", which every escaped name does.
func escapeTestName(name string) string {
	if !strings.Contains(name, "::") && !strings.HasPrefix(name, ":") &&
		!strings.Contains(name, "%25") && !strings.Contains(name, "%3A") {
		return name
	}
	name = strings.ReplaceAll(strings.ReplaceAll(name, "%", "%25"), "::", "%3A%3A")
	if strings.HasPrefix(name, ":") {
		name = "%3A" + name[1:]
	}
	return name
}

// IDPart reports whether s can be a project, file or key in a test's ID: it
// holds no "::", and neither begins nor ends with ":", either of which would
// move where the ID splits.
func IDPart(s string) bool {
	return !strings.Contains(s, "::") && !strings.HasPrefix(s, ":") && !strings.HasSuffix(s, ":")
}

// Empty reports whether there is no evidence.
func (e *ScopeEvidence) Empty() bool {
	return e == nil || (len(e.Misses) == 0 && len(e.Unconsumed) == 0 && len(e.CaptureIncomplete) == 0 && len(e.Incomplete) == 0 && len(e.CannotCapture) == 0)
}

// TestID is a test's ID on the wire (design v14 §6.2):
// [<project>::]<path>::<name>, where path is the test file a harness names,
// else its instance's key. With neither, the ID is the bare name. The name
// is the framework's own, and may hold "::" (a Go subtest named after an
// IPv6 address, a pytest class's test): escapeTestName writes it so that the
// last "::" of an ID always ends its path. The parts before it must pass
// IDPart, which the agent checks.
func TestID(project, file, key, name string) string {
	path := file
	if path == "" {
		path = key
	}
	id := escapeTestName(name)
	if path != "" {
		id = path + "::" + id
	}
	if project != "" {
		id = project + "::" + id
	}
	return id
}
