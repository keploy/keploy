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
	// Workers is how many test workers of the run its framework runs at
	// once (Playwright's workers, go test's -p), each claiming for itself.
	// 0, sent by an integration that predates it, is one. The agent refuses
	// more than it runs at once (ScopeErrTooManyWorkers: more than its limit,
	// or more than one in record's full round or in a replay that moves the
	// run's one clock to each test's recorded begin), and a count other than
	// a live worker's (ScopeErrWorkersMismatch), so whether a run is taken
	// never depends on how its workers happen to overlap.
	Workers int `json:"workers,omitempty"`
}

// InstanceResp answers POST /agent/instance. With one test process per run
// the instance is the run itself (InstanceRun); a worker of a run of more
// gets its keploy instance's number. TimeFile is the clock file the test process
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
// applies Skips skip-list entries and the Needs of Needs tests.
type ScopeGateResp struct {
	V      int    `json:"v"`
	Status string `json:"status"`
	Skips  int    `json:"skips"`
	Needs  int    `json:"needs"`
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
	// ScopeErrTooManyWorkers: a claim declared more test workers
	// (InstanceReq.Workers) than this keploy runs at once, or more than one
	// where the run runs one (record's full round, a replay that moves the
	// run's one clock).
	ScopeErrTooManyWorkers = "too_many_workers"
	// ScopeErrWorkersMismatch: a claim declared another worker count than
	// a live worker of the run did.
	ScopeErrWorkersMismatch = "workers_mismatch"
	// ScopeErrNotInInstance: a begin or end of a run of more than one worker
	// came from a process that is no live worker's claimant, or keploy
	// could not put a worker's processes in an instance of their own.
	ScopeErrNotInInstance = "not_in_instance"
	// ScopeErrNotOpen: an end for a test that is not open, or a given
	// while no test is open.
	ScopeErrNotOpen = "not_open"
	// ScopeErrBadRequest: an unreadable or invalid body.
	ScopeErrBadRequest = "bad_request"
	// ScopeErrNoCaller: the agent could not tell which process called it.
	ScopeErrNoCaller = "caller_unknown"
	// ScopeErrInternal: the agent failed to act on a valid call.
	ScopeErrInternal = "internal"
	// ScopeErrClockNotFrozen: the test process runs without keploy's clock
	// on a run whose clock keploy froze (macOS: the shim is not in it), so
	// its tests would read another clock than the recordings were made on.
	ScopeErrClockNotFrozen = "clock_not_frozen"
)

// ScopeUpgradeRemedy is the remedy of each ScopeErrUpgradeIntegration
// refusal of a harness that predates the wire. It names only the
// integrations of the wire that exist, and how a framework that has none
// makes the wire's calls itself.
const ScopeUpgradeRemedy = "use an integration that speaks keploy's scope protocol v1 (go-sdk keploytest for go test, @keploy/playwright for Playwright) and run one test process at a time; " +
	"keploy has none for other frameworks, whose test process makes the calls by hand: it claims the run (POST /agent/instance with its package folder, relative to the project, as key) before its first test, " +
	"then sends each test's begin with canSkip true, skips the test when the begin answers action skip, and sends its end, never with pid, at, dir or suite"

// ScopeEvidence is what keploy saw of one replayed test that fails it: the
// calls no recorded answer matched while it was open (a miss outside every
// test, and one that repeats a spent recording of the set's background, is
// charged to no test: UnmatchedCall.ChargedTo, WindowOf), and the recorded
// data-plane calls of its mapping it never made.
// Misses carry only the summary fields; never a recorded value.
type ScopeEvidence struct {
	Misses     []UnmatchedCall `json:"misses,omitempty" yaml:"misses,omitempty"`
	Unconsumed []string        `json:"unconsumed,omitempty" yaml:"unconsumed,omitempty"`
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
	// Unread: keploy could not read everything the test did (its misses,
	// or the mocks it consumed), so the test has no verdict of its own;
	// keploy's verdict on the run is "not judged", never the test's fault.
	Unread bool `json:"unread,omitempty" yaml:"unread,omitempty"`
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

// ChargeNotJudged charges the test with losses that leave it without a
// verdict (CaptureLosses.Split's notJudged), never failed: on CannotCapture,
// and in the reason one sentence names all of CannotCapture with its summed
// counts. The sentence the reason already holds for CannotCapture is written
// again in its place; else the sentence goes after what the reason says.
// What the agent charged (a worker's REAL use) and what the record charges
// (a mock), or each late mock whose container keploy could not read, are one
// count: a reader sees one number, and each What charged, once.
func (e *ScopeEvidence) ChargeNotJudged(l CaptureLosses) {
	was := e.CannotCapture.NotJudgedText()
	sum := e.CannotCapture.Plus(l)
	for i := range sum {
		for _, c := range e.CannotCapture {
			if c.Kind != sum[i].Kind || c.What == "" {
				continue
			}
			// Plus keeps the What charged last: each What charged is named,
			// once.
			if !strings.Contains(c.What, sum[i].What) {
				sum[i].What = c.What + "; " + sum[i].What
			} else {
				sum[i].What = c.What
			}
		}
	}
	e.CannotCapture = sum
	text := sum.NotJudgedText()
	switch {
	case was != "" && strings.Contains(e.Reason, was):
		e.Reason = strings.Replace(e.Reason, was, text, 1)
	case e.Reason == "":
		e.Reason = "keploy: " + text + "."
	default:
		e.Reason = strings.TrimSpace(e.Reason + " Also " + text + ".")
	}
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

// TestIDPath is the path of a test's ID (TestID): its test file, else its
// instance's key; "" for a bare name. The name's escape holds no "::" and
// does not begin with ":", and a path does not end with one (IDPart), so the
// ID's last "::" ends its path, and a "::" before it ends its project.
func TestIDPath(id string) string {
	i := strings.LastIndex(id, "::")
	if i < 0 {
		return ""
	}
	head := id[:i]
	if j := strings.LastIndex(head, "::"); j >= 0 {
		head = head[j+2:]
	}
	return head
}
