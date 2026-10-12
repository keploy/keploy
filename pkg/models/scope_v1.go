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
	Misses     []UnmatchedCall `json:"misses,omitempty"`
	Unconsumed []string        `json:"unconsumed,omitempty"`
	// Reason is the text the test was failed with at its end.
	Reason string `json:"reason,omitempty"`
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
	return e == nil || (len(e.Misses) == 0 && len(e.Unconsumed) == 0)
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
