package models

import (
	"errors"
	"strings"
	"time"
)

type TestReport struct {
	Version       Version      `json:"version" yaml:"version"`
	Name          string       `json:"name" yaml:"name"`
	Status        string       `json:"status" yaml:"status"`
	FailureReason string       `json:"failureReason,omitempty" yaml:"failure_reason,omitempty"`
	Success       int          `json:"success" yaml:"success"`
	Failure       int          `json:"failure" yaml:"failure"`
	Obsolete      int          `json:"obsolete,omitempty" yaml:"obsolete,omitempty"`
	HighRisk      int          `json:"high_risk,omitempty" yaml:"high-risk,omitempty"`
	MediumRisk    int          `json:"medium_risk,omitempty" yaml:"medium-risk,omitempty"`
	LowRisk       int          `json:"low_risk,omitempty" yaml:"low-risk,omitempty"`
	Ignored       int          `json:"ignored" yaml:"ignored"`
	Total         int          `json:"total" yaml:"total"`
	Tests         []TestResult `json:"tests" yaml:"tests,omitempty"`
	TestSet       string       `json:"testSet" yaml:"test_set"`
	CreatedAt     int64        `json:"created_at" yaml:"created_at"`
	TimeTaken     string       `json:"time_taken" yaml:"time_taken"`
	CmdUsed       string       `json:"cmdUsed,omitempty" yaml:"cmdUsed,omitempty"`
	AppLogs       string       `json:"appLogs,omitempty" yaml:"app_logs,omitempty"`
	// CaptureVerification is what keploy's check that its agent tracked every
	// process of this test set found, when it found something: the capture
	// was incomplete (a process went untracked; the set is FAILED, and its
	// JUnit suite carries a failed "keploy capture" case), or could not be
	// verified (the count could not be read). Absent when nothing was lost,
	// and when nothing was checked (an agent that does not count, a run that
	// does not mock): a report does not tell those two apart.
	CaptureVerification *CaptureVerification `json:"capture_verification,omitempty" yaml:"capture_verification,omitempty"`
	// Real is what the set's run saw of mock.real beyond its test cases
	// (proven switch): the destinations it reached, and the connections
	// keploy could not capture outside them. RealListed are the
	// destinations the recordings of its test cases that ran list. Absent
	// without a mock.real list.
	Real       *RealRun `json:"real,omitempty" yaml:"real,omitempty"`
	RealListed []string `json:"realListed,omitempty" yaml:"real_listed,omitempty"`
	// RealFailure is why mock.real failed the set (design v14 §8.2 rule 4):
	// a destination the recordings list that the run never reached, or
	// that never answered. JUnit fails a case of its own with it.
	RealFailure string `json:"realFailure,omitempty" yaml:"real_failure,omitempty"`
	// UnclaimedCalls are the calls the set's application made outside every
	// test that no mock answered (proven switch): after its last test, a pool
	// miss no later test takes, and before its first, as it started
	// (UnmatchedCall.BeforeSet). They fail the set; JUnit fails a case of its
	// own with them.
	UnclaimedCalls []UnmatchedCall `json:"unclaimedCalls,omitempty" yaml:"unclaimed_calls,omitempty"`
	// AppNotStopped, under the proven switch, says why keploy read the set's
	// end with its application still running (a stack kept across test-sets,
	// --keep-app-alive, an application keploy did not start): a call the
	// application made after that read is not judged (P101-c). The set's
	// verdict is what the read found.
	AppNotStopped string `json:"appNotStopped,omitempty" yaml:"app_not_stopped,omitempty"`
	// Proven says the set was run under the proven switch (KEPLOY_PROVEN=1),
	// so its results carry the proven model's evidence: a report renders it
	// from this, not from the switch of the process rendering it. Set on
	// every report a set run under the switch writes; absent for a set run
	// without it.
	Proven bool `json:"proven,omitempty" yaml:"proven,omitempty"`
	// SetAnswers are, under the proven switch, the answered calls
	// (UnmatchedCall.Answer) the set's end charged to the set, no test: made
	// before its first test or after its last. They never fail
	// the set, nor land in UnclaimedCalls. Absent when there is none.
	SetAnswers []UnmatchedCall `json:"setAnswers,omitempty" yaml:"set_answers,omitempty"`
}

// CaptureVerificationStatus is the outcome of checking that the agent tracked
// every process of a test set.
type CaptureVerificationStatus string

const (
	// CaptureIncomplete: a process went untracked (the kernel refused it a
	// place in the agent's PID map), so its dependency calls may have reached
	// the real service unmocked. The set is FAILED for it.
	CaptureIncompleteStatus CaptureVerificationStatus = "incomplete"
	// CaptureUnverified: the agent counts untracked processes, but keploy
	// could not read the count, so the capture could not be confirmed. With
	// the proven switch off the set's pass/fail is left as decided (only
	// mock mode's --strict fails on it); with it on the set is not judged.
	CaptureUnverifiedStatus CaptureVerificationStatus = "unverified"
)

// CaptureVerification is the capture-completeness outcome carried in a
// TestReport (and the JUnit and json projections).
type CaptureVerification struct {
	Status CaptureVerificationStatus `json:"status" yaml:"status"`
	Reason string                    `json:"reason" yaml:"reason"`
}

type TestCoverage struct {
	FileCov  map[string]string `json:"fileCoverage" yaml:"file_coverage"`
	TotalCov string            `json:"totalCoverage" yaml:"total_coverage"`
	Loc      Loc               `json:"loc" yaml:"loc"`
}

type Loc struct {
	Total   int `json:"total" yaml:"total"`
	Covered int `json:"covered" yaml:"covered"`
}

func (tr *TestReport) GetKind() string {
	return "TestReport"
}

type TestResult struct {
	Kind         Kind        `json:"kind" yaml:"kind"`
	Name         string      `json:"name" yaml:"name"`
	Status       TestStatus  `json:"status" yaml:"status"`
	Started      int64       `json:"started" yaml:"started"`
	Completed    int64       `json:"completed" yaml:"completed"`
	TestCasePath string      `json:"testCasePath" yaml:"test_case_path"`
	MockPath     string      `json:"mockPath" yaml:"mock_path"`
	TestCaseID   string      `json:"testCaseID" yaml:"test_case_id"`
	Req          HTTPReq     `json:"req" yaml:"req,omitempty"`
	Res          HTTPResp    `json:"resp" yaml:"resp,omitempty"`
	GrpcReq      GrpcReq     `json:"grpcReq,omitempty" yaml:"grpcReq,omitempty"`
	GrpcRes      GrpcResp    `json:"grpcRes,omitempty" yaml:"grpcRes,omitempty"`
	Noise        Noise       `json:"noise" yaml:"noise,omitempty"`
	Result       Result      `json:"result" yaml:"result"`
	TimeTaken    string      `json:"time_taken" yaml:"time_taken"`
	FailureInfo  FailureInfo `json:"failure_info,omitempty" yaml:"failure_info,omitempty"`
	// SkipReason says why keploy skipped this test: set on an IGNORED result
	// whose skip carried a reason (a quarantine, with its class and why), and
	// rendered into the JUnit <skipped> message and the text report. Empty for
	// every other result, and for a test skipped without one.
	SkipReason string `json:"skip_reason,omitempty" yaml:"skip_reason,omitempty"`
	// MockMismatches captures expected vs actually-consumed mocks for THIS test.
	// Distinct from FailureInfo.MockMismatch (which only fires when the mock
	// pool diverged AND the test case is OBSOLETE) — this field is set for
	// every test that flows through the standard replay loop (pass or fail),
	// so passing tests can also surface their consumed mocks for the report UI.
	// Name mirrors the sandbox integration runner's
	// IntegrationStepResult.MockMismatches.
	//
	// Populated by:
	//   - Replayer.RunTestSet's per-test result block, both instrument and
	//     non-instrument (k8s-proxy / remote-agent) modes.
	//
	// NOT populated by:
	//   - The deferred streaming-test path (Phase 2 stream replay) — those
	//     TestResults are persisted without this field.
	//   - Standard replay loop when the per-test consumed-mock fetch
	//     (GetConsumedMocks, in either instrument or non-instrument mode)
	//     fails. The replayer skips emitting MockMismatches rather than
	//     attribute stale loop-scoped data to the wrong test case.
	//
	// Consumers should treat an absent value as "data not available for
	// this test / run mode", NOT "no mocks were consumed".
	MockMismatches *MockMismatchInfo `json:"mock_mismatches,omitempty" yaml:"mock_mismatches,omitempty"`
	// Captured is what this test case's {{capture ...}} placeholders took
	// from the app's answer, in order: each key's value for the rest of the
	// run. Absent when the expected response captures nothing.
	Captured []TemplateCapture `json:"captured,omitempty" yaml:"captured,omitempty"`
	// TemplateFailures are the template variable places the app's answer
	// did not satisfy: a capture that found no value of its type, a use that
	// did not hold its key's value; or, for a test case refused when its set
	// loaded (never sent), each placeholder refused (Refused). Each fails
	// the test case on its own, noise or not, unless the mock set's
	// divergence demotes the test case to OBSOLETE (as it demotes any
	// failure without --strict-failure); the report still names the places.
	// Absent when there is none.
	TemplateFailures []TemplateFailure `json:"template_failures,omitempty" yaml:"template_failures,omitempty"`
	// Real are the mock.real destinations the test case used while it ran
	// (proven switch): its REAL rows. Absent when it used none.
	Real []RealUse `json:"real,omitempty" yaml:"real,omitempty"`
	// Variables are the test variables bound while the test case ran (proven
	// switch): the values its request sent, and each value a dependency call
	// carried where the learn step found it first, with where it was taken.
	// A later call that sent another value failed the test case as a miss.
	// Absent when none was bound.
	Variables []VariableBinding `json:"variables,omitempty" yaml:"variables,omitempty"`
	// Answers are, under the proven switch, the answered calls
	// (UnmatchedCall.Answer) charged to this test's window, or to the gap
	// before it, as misses are: a pairing record's full round
	// made, a background write answered by its kind. They never fail the
	// test, nor land in FailureInfo.UnmatchedCalls. Absent when there is
	// none.
	Answers []UnmatchedCall `json:"answers,omitempty" yaml:"answers,omitempty"`
}

// TemplateFailure is one template variable place a test case failed on, or
// one placeholder it was refused for when its test set loaded.
type TemplateFailure struct {
	// Where is the place: in the answer for a failure ("body.id", "header
	// Set-Cookie.sid"), in the test case for a refusal ("request body",
	// "response body.items[0].id").
	Where string `json:"where" yaml:"where"`
	// Placeholder is the placeholder as the test case writes it.
	Placeholder string `json:"placeholder" yaml:"placeholder"`
	// Why says what the place needed and what the answer held; for a
	// refusal, why the placeholder cannot run and the edit to make.
	Why string `json:"why" yaml:"why"`
	// Refused marks a placeholder refused when the test set loaded: the test
	// case was never sent, and there is no answer to accept. A refusal with
	// no Placeholder refuses the test case's whole test set (one an
	// interrupted keploy templatize left half-written; category
	// TEST_SET_REFUSED), not a placeholder of it.
	Refused bool `json:"refused,omitempty" yaml:"refused,omitempty"`
}

// TemplateCapture is one value a test case's expected response took from the
// app's answer through a {{capture ...}} placeholder.
type TemplateCapture struct {
	Key   string `json:"key" yaml:"key"`
	Value string `json:"value" yaml:"value"`
	// Where is the place in the answer: "body.items[0].id", "header Set-Cookie.sid".
	Where string `json:"where" yaml:"where"`
}

// FailureInfo captures structured diagnostic data about why a test case failed or became obsolete.
// Populated by the matcher (Risk/Category/Assessment) and the replayer (MatchedCalls/UnmatchedCalls/MockMismatch).
// Consumed by k8s-proxy to build TestCaseFailureDetails for the platform API.
type FailureInfo struct {
	Risk           RiskLevel          `json:"risk,omitempty" yaml:"risk,omitempty"`
	Category       []FailureCategory  `json:"category,omitempty" yaml:"category,omitempty"`
	Assessment     *FailureAssessment `json:"assessment,omitempty" yaml:"assessment,omitempty"`
	MockMismatch   *MockMismatchInfo  `json:"mock_mismatch,omitempty" yaml:"mock_mismatch,omitempty"`
	MatchedCalls   []MatchedCall      `json:"matched_calls,omitempty" yaml:"matched_calls,omitempty"`
	UnmatchedCalls []UnmatchedCall    `json:"unmatched_calls,omitempty" yaml:"unmatched_calls,omitempty"`
	// UnmatchedCallsUnread says UnmatchedCalls may be missing calls: the
	// agent's record of the calls no mock answered during this test could not
	// all be read (models.ErrUnmatchedCallsUnread, or the read failed). The
	// test's evidence is unknown, so its test set is not judged. Set only under
	// the proven switch.
	UnmatchedCallsUnread bool `json:"unmatched_calls_unread,omitempty" yaml:"unmatched_calls_unread,omitempty"`
	// MissesUnattributed says, with the proven switch off, what
	// UnmatchedCallsUnread says under it: the calls no mock answered during
	// this test could not all be read, so a miss of this test may be missing
	// from it, or have been read with another test's. With the switch off a
	// miss fails no test, so the test's verdict stands, and nothing reads this
	// as no verdict; but nothing may take a failed difference of its test set
	// for noise, which may be that miss's (enterprise's auto-noise learns none
	// for the set). Set only with the switch off.
	MissesUnattributed bool `json:"misses_unattributed,omitempty" yaml:"misses_unattributed,omitempty"`
	// TestMocksUnread says the mocks this test consumed could not be read
	// from keploy's agent, so whether it made every call it recorded is
	// unknown: it has no verdict, and its test set is not judged. Set only
	// under the proven switch, before any hook reads the result.
	TestMocksUnread bool `json:"test_mocks_unread,omitempty" yaml:"test_mocks_unread,omitempty"`
	// VariablesUnchecked says why what this test captured could not be
	// checked against what its dependency calls carried (a keploy-side
	// fault): it has no verdict, and its test set is not judged. Set only
	// under the proven switch.
	VariablesUnchecked string `json:"variables_unchecked,omitempty" yaml:"variables_unchecked,omitempty"`
	// UnconsumedMocks are, under KEPLOY_PROVEN=1, the test mocks this test
	// did not consume: the mocks of its recorded mapping its per-test
	// assertion owns (data-plane mocks; connection failures and
	// protocol-phase mocks are not owned) that it made no call for, each of
	// which fails it (a recorded call was not made). In recorded order, every
	// one: Result.DepResult caps its rows and names a target, not the mock.
	// Empty with the switch off, and for a test that owned none or whose
	// consumption was not read (TestMocksUnread).
	UnconsumedMocks []UnconsumedMock `json:"unconsumed_mocks,omitempty" yaml:"unconsumed_mocks,omitempty"`
	// CaptureIncomplete is, under KEPLOY_PROVEN=1, what keploy's agent could
	// not capture that is charged to this test, each of which fails it:
	//   - an agent-life kind (pid_map_refused, app_subtree_untracked, and any
	//     other kind): what the agent lost since the test set began, as this
	//     test ended. Such a loss taints the test running when it happened
	//     and every later test of the set (design v14 §3.2); a process of the
	//     application's that keploy could not place (AppLife) taints every
	//     test since that application started, in this set or an earlier one
	//     it served (--keep-app-alive), and one that outlives the application
	//     every test since the run began.
	//   - a test's own kind that is judged (CaptureLossKind.PerTest:
	//     tls_handshake_failed, capture_unread, connection_unrecorded; the
	//     other test's own kinds are not judged, CannotCapture, or evidence):
	//     what the agent lost while this test ran, since the previous test
	//     ended, for this test alone.
	//   - real_uncaptured (class e, mock.real): the connections keploy could
	//     neither capture nor relay as real that were undecided or used while
	//     this test ran, counted for this test alone. One undecided across
	//     several tests is charged to each; a test that never touched it is
	//     not (§3.2 as amended for class e). keploy relays nothing of such a
	//     connection, so no later test's calls escape capture through it.
	// Empty with the switch off.
	CaptureIncomplete CaptureLosses `json:"capture_incomplete,omitempty" yaml:"capture_incomplete,omitempty"`
	// CaptureUnread says, under KEPLOY_PROVEN=1, that keploy could not read
	// whether its agent captured every call while this test ran (the count
	// could not be read, at the test's end or at its set's mark): the test
	// has no verdict, and its set is not judged.
	CaptureUnread bool `json:"capture_unread,omitempty" yaml:"capture_unread,omitempty"`
	// CannotCapture is, under KEPLOY_PROVEN=1, what keploy cannot capture at
	// all that the application did while this test or one before it ran
	// (decision 20: a call over a Unix socket, or Unix sockets keploy cannot
	// see), and every other loss that leaves a test without a verdict
	// (CaptureLossKind.NotJudged: an agent started again while the
	// application ran on, agent_started_again, among them): the test has no
	// verdict, and its set is not judged. Never a failure.
	CannotCapture CaptureLosses `json:"cannot_capture,omitempty" yaml:"cannot_capture,omitempty"`
	// StreamHeldOpen says, under KEPLOY_PROVEN=1, why the app's stream was
	// not read to its end: the app kept it open on keepalives, past its
	// recording (every recorded frame, or message, came, then a keepalive
	// beyond the recorded ones), or short of it (two keepalives in a row
	// and nothing of the rest between them) (row W1). The test has no
	// verdict, and its set is not judged. Never a pass, and a failure only
	// on what came.
	StreamHeldOpen string `json:"stream_held_open,omitempty" yaml:"stream_held_open,omitempty"`
	// AnswerStalled says, under KEPLOY_PROVEN=1 with no apiTimeout of the
	// user's, what the stall rule waited for when it ended the test's
	// exchange before the app's answer began, or part-way through an answer
	// that is not streamed: the app took none of the request, or not all of
	// it, or sent nothing back for the rule's grace of keploy's runnable time
	// (pkg.AnswerStallGrace, rule 15). Nothing was compared: the row is
	// built as a test with no answer is (FAILED), and the test has no
	// verdict all the same (VerdictUnknown), nor has its set.
	AnswerStalled string `json:"answer_stalled,omitempty" yaml:"answer_stalled,omitempty"`
	// MappingStale says why an OBSOLETE test has no verdict: it answered
	// differently from its recording while the dependency calls it made
	// diverged from what its recorded mapping lists (a recorded mock of its
	// mapping left unconsumed, or a mock outside it consumed). keploy cannot
	// tell a regression from a stale mapping then, so the test is not
	// judged, and neither is its set: never the silent pass an OBSOLETE once
	// was (false-pass row 0). Set with the proven switch off, the only place
	// a wrong answer is demoted.
	MappingStale string `json:"mapping_stale,omitempty" yaml:"mapping_stale,omitempty"`
	// LearningNotKept says, under KEPLOY_PROVEN=1 in record's full round, why
	// keploy did not keep what its agent learned on the mocks this test
	// consumed, and what it did not keep: the agent did not say it read the
	// test case's request (an agent older than keploy), so a value the
	// application took from the request may have been learned as noise, or
	// as a variable that takes any value. A test that passed may have passed
	// on it alone, so the test has no verdict, and its test set is not
	// judged.
	LearningNotKept string `json:"learning_not_kept,omitempty" yaml:"learning_not_kept,omitempty"`
}

// MappingStaleRemedy is what to do about a test whose recorded mapping
// diverged from what it consumed (FailureInfo.MappingStale).
const MappingStaleRemedy = "re-record the test-set, or run keploy test with --disable-mapping --update-test-mapping to replay it by recorded time and write its mapping afresh"

// NotJudged is why the test has no verdict, whatever its status says: what
// keploy could not read or check of it (UnmatchedCallsUnread,
// TestMocksUnread, VariablesUnchecked, CaptureUnread, LearningNotKept), or
// what it cannot capture at all (CannotCapture, decision 20), one reason
// each.
// Empty for a judged test. Each is set only under the proven switch, so a
// result written without it is never unjudged, but for MappingStale, which is
// set with it off, the only place a wrong answer is demoted to OBSOLETE.
func (fi FailureInfo) NotJudged() []string {
	var why []string
	if fi.UnmatchedCallsUnread {
		why = append(why, "keploy could not read every call it made that no recorded mock answered")
	}
	if fi.TestMocksUnread {
		why = append(why, "keploy could not read which of its recorded calls it made")
	}
	if fi.VariablesUnchecked != "" {
		why = append(why, "keploy could not check what it captured against its dependency calls: "+fi.VariablesUnchecked)
	}
	if fi.CaptureUnread {
		why = append(why, "keploy could not read whether its agent captured every call while it ran")
	}
	if len(fi.CannotCapture) > 0 {
		why = append(why, strings.TrimPrefix(fi.CannotCapture.NotJudgedText(), "not judged: "))
	}
	if fi.StreamHeldOpen != "" {
		why = append(why, fi.StreamHeldOpen)
	}
	if fi.AnswerStalled != "" {
		why = append(why, fi.AnswerStalled)
	}
	if fi.MappingStale != "" {
		why = append(why, fi.MappingStale)
	}
	if fi.LearningNotKept != "" {
		why = append(why, fi.LearningNotKept)
	}
	return why
}

// VerdictUnknown is why the test has no verdict although its row reads as
// one (FailureInfo.NotJudged): empty for a judged test, for a FAILED one
// (what could not be read can only add to why it failed) unless nothing was
// compared because the stall rule ended its answer (AnswerStalled), and for
// one keploy skipped (IGNORED, which says it has no verdict). MappingStale
// counts on an OBSOLETE row only. JUnit and the NDJSON mark a test by this one
// predicate.
func (r TestResult) VerdictUnknown() []string {
	if r.Status == TestStatusIgnored {
		return nil
	}
	if r.Status == TestStatusFailed && r.FailureInfo.AnswerStalled == "" {
		return nil
	}
	// A FAILED row whose answer the stall rule ended (AnswerStalled) was
	// compared with nothing: its FAILED says no answer came, not that one
	// differed.
	fi := r.FailureInfo
	if r.Status != TestStatusObsolete {
		// MappingStale is a demoted wrong answer's: a row a hook relabelled
		// since says its own verdict.
		fi.MappingStale = ""
	}
	return fi.NotJudged()
}

// UnconsumedMock is a test mock a test did not consume
// (FailureInfo.UnconsumedMocks).
type UnconsumedMock struct {
	Name string `json:"name" yaml:"name"`
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`
	// ConsumedBy names another test of the set that consumed this mock, when
	// one did: the call was made, by a sibling, and the order in which the
	// shared mock pool served the two can decide which test it counts for,
	// until each test's consumption is exact. Empty when no test consumed it.
	ConsumedBy string `json:"consumed_by,omitempty" yaml:"consumed_by,omitempty"`
	// Concurrent names, under the proven switch, the other tests whose
	// requests were open while this mock was recorded (Mapping.Groups),
	// comma-separated in window order (a string, so the struct stays
	// comparable). Unkept counts the tests whose requests were open beside
	// them that the recording did not keep (models.ConcurrencyGroup.Unkept):
	// one of those may have made it.
	Concurrent string `json:"concurrent,omitempty" yaml:"concurrent,omitempty"`
	// WhyNot says why this replay did not share the mock between those
	// tests (ConcurrentNotShared*: it lists it under this test by time
	// alone, or where keploy record's full round left it, unproven), with
	// Stated for ConcurrentNotSharedStates. Empty, with Concurrent or Unkept
	// set, only for the one case keploy proved: keploy record's full round
	// shared it between them and no call of theirs matched it exactly (it
	// learns nothing against a recording they share), so it is charged to
	// each of them that ran; Closest then names the one whose call came
	// closest to it, when the learn step saw one.
	WhyNot  string `json:"why_not,omitempty" yaml:"why_not,omitempty"`
	Stated  string `json:"stated,omitempty" yaml:"stated,omitempty"`
	Closest string `json:"closest,omitempty" yaml:"closest,omitempty"`
	Unkept  int    `json:"unkept,omitempty" yaml:"unkept,omitempty"`
	// RepeatOf says that this recorded call, which this replay never made, is
	// identical to a recording an earlier test of the set made, which
	// answered that test's call at this replay: an application that keeps an
	// answer for a span of elapsed time asked again at record, where the
	// recording ran past the span, and not at a faster replay
	// (models.RepeatNote). NextSteps is its sentence (ExplainUnmade). Both are
	// empty when no such recording is known.
	RepeatOf  *RepeatNote `json:"repeat_of,omitempty" yaml:"repeat_of,omitempty"`
	NextSteps string      `json:"next_steps,omitempty" yaml:"next_steps,omitempty"`
}

// MockMismatchMock identifies a mock in the expected/actual mock sets carried by
// MockMismatchInfo. Used for both the obsolete-mismatch reporting path
// (FailureInfo.MockMismatch) and the always-populated TestResult.MockMismatches
// field that exposes per-test mock consumption to report consumers.
type MockMismatchMock struct {
	Name string `json:"name" yaml:"name"`
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`
}

// MockMismatchInfo records the expected (recorded) vs actual (consumed during
// replay) mock sets for a test case. Used in two places with different
// semantics:
//
//   - FailureInfo.MockMismatch — populated only when the test became OBSOLETE
//     due to mock-pool divergence (mocks were added/removed between recording
//     and replay).
//   - TestResult.MockMismatches — populated for EVERY test that went through
//     the replay loop, regardless of pass/fail or obsolescence. Lets report
//     UIs render expected vs consumed mocks for passing tests too.
type MockMismatchInfo struct {
	ExpectedMocks []MockMismatchMock `json:"expected_mocks,omitempty" yaml:"expected_mocks,omitempty"`
	ActualMocks   []MockMismatchMock `json:"actual_mocks,omitempty" yaml:"actual_mocks,omitempty"`
}

// MatchedCall represents an outgoing call that was successfully matched to a recorded mock.
type MatchedCall struct {
	MockName string `json:"mock_name" yaml:"mock_name"`                   // internal mock reference for View Mock
	Protocol string `json:"protocol,omitempty" yaml:"protocol,omitempty"` // Http, Mongo, Postgres, etc.
	Summary  string `json:"summary,omitempty" yaml:"summary,omitempty"`   // e.g. "GET /posts?id=1", "DNS dep-service", "MongoDB find"
}

// UnmatchedCall represents an outgoing call during replay that had no matching mock.
type UnmatchedCall struct {
	Protocol      string `json:"protocol" yaml:"protocol"`
	ActualSummary string `json:"actual_summary,omitempty" yaml:"actual_summary,omitempty"` // e.g. "POST /comments"
	Destination   string `json:"destination,omitempty" yaml:"destination,omitempty"`       // outgoing call's destination/domain (HTTP Host, or host:port) — which upstream missed
	// LiveDestination is, under the proven switch, the address the call was
	// made to when Destination names it as the recording does instead (the
	// mocks' destAddr: 127.0.0.1:27017 where the app reached [::1]:27017,
	// the same loopback port), so a reader who greps mocks.yaml for
	// Destination finds the mocks; "" when the two are the same.
	LiveDestination string `json:"live_destination,omitempty" yaml:"live_destination,omitempty"`
	ClosestMock     string `json:"closest_mock,omitempty" yaml:"closest_mock,omitempty"` // internal mock reference for View Closest
	Diff            string `json:"diff,omitempty" yaml:"diff,omitempty"`
	NextSteps       string `json:"next_steps,omitempty" yaml:"next_steps,omitempty"` // actionable remediation guidance from the matcher
	// MatchPhase, CandidateCount and FieldDiffs mirror MockMismatchReport so
	// report consumers (CLI `keploy report`, report yaml, platform UI) can
	// render structured field-level drift instead of an opaque string. Paths
	// in FieldDiffs use the noise-config vocabulary and can be copied
	// verbatim into test.globalNoise / spec.assertions.noise.
	MatchPhase string `json:"match_phase,omitempty" yaml:"match_phase,omitempty"`
	// DestinationScope mirrors MockMismatchReport.DestinationScope
	// (DestinationScope* constants). It is a separate axis from MatchPhase:
	// "not_in_compared_set" says no mock this miss was compared against
	// targeted the call's upstream, while MatchPhase still reports where the
	// cascade stopped. Omitted when empty — the protocol supplied no
	// destination evidence, and an absent field is the only honest rendering
	// of "we did not check".
	DestinationScope string          `json:"destination_scope,omitempty" yaml:"destination_scope,omitempty"`
	CandidateCount   int             `json:"candidate_count,omitempty" yaml:"candidate_count,omitempty"`
	FieldDiffs       []MockFieldDiff `json:"field_diffs,omitempty" yaml:"field_diffs,omitempty"`
	// ClosestMockReq / ReceivedReq carry the FULL rendered requests for the CLI
	// side-by-side whole-mock diff (left = mock, right = live request).
	ClosestMockReq string `json:"closest_mock_req,omitempty" yaml:"closest_mock_req,omitempty"`
	ReceivedReq    string `json:"received_req,omitempty" yaml:"received_req,omitempty"`
	// VariablePlaces mirrors MockMismatchReport.VariablePlaces: the URL path
	// places of a test variable on this call, under the proven switch only.
	VariablePlaces []string `json:"variable_places,omitempty" yaml:"variable_places,omitempty"`
	// At is when the call missed, so a client can tell which test made it.
	At time.Time `json:"at,omitzero" yaml:"at,omitempty"`
	// Kind is where the call was made, under the proven switch (MissInTest,
	// MissExtraCall, MissOutsideTest); empty without it. Recorded is, for an
	// extra call, how many times the test recorded that call.
	Kind     string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Recorded int    `json:"recorded,omitempty" yaml:"recorded,omitempty"`
	// BeforeSet says, under the proven switch, that the call was made before
	// its test set's first test began, with no test's window open: the
	// application's startup. Its Kind is MissOutsideTest. It is the set's,
	// no test's: the replay fails the set with it, naming the call, and
	// charges no test (the read of the set's first test carries it).
	BeforeSet bool `json:"before_set,omitempty" yaml:"before_set,omitempty"`
	// Differs mirrors MockMismatchReport.Differs: the names of the fields in
	// which the call differs from its closest recording, without values.
	Differs []string `json:"differs,omitempty" yaml:"differs,omitempty"`
	// NotOwned mirrors MockMismatchReport.NotOwned: the call is ClosestMock,
	// a recording the test does not own.
	NotOwned bool `json:"not_owned,omitempty" yaml:"not_owned,omitempty"`
	// RecordedIn names, under the proven switch, the other test of the set
	// that owns the recording a NotOwned call is (ClosestMock): the call is
	// likely one that test recorded, such as a later exchange of a connection
	// it opened, whose handshake was recorded there.
	RecordedIn string `json:"recorded_in,omitempty" yaml:"recorded_in,omitempty"`
	// Reason is why the call missed, in judging's words (design v14 §5.3,
	// §7.3): MissRequestChanged, MissUnstableOrder or MissUnmockedCall. Only
	// record's full round, where the learn step runs, gives one; empty
	// everywhere else, and for a call of that round no reason of judging's
	// names: one keploy could not judge because its own store could not read
	// a recorded answer back, one a parser's pairing found tied between
	// recordings whose replies differ, or one call more than the test
	// recorded (Kind and Recorded say so).
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
	// Concurrent is, under the proven switch, what keploy knows of the call
	// when the test was recorded while other tests' requests were open
	// (Mapping.Groups): with whom, and which of them its recording was
	// attributed to or taken by. nil for a call of a test recorded alone.
	Concurrent *ConcurrentNote `json:"concurrent,omitempty" yaml:"concurrent,omitempty"`
	// Answer, under the proven switch, says the call WAS answered, and how;
	// "" for a miss. AnswerProvisional: record's full round answered it with
	// ClosestMock, a recording another test owns (a pairing),
	// which judging then moves to the set's background; RecordedIn stays
	// empty on an answer (the parser's store names no recording's test), and
	// judging reads ClosestMock's owner off mappings.yaml. Drift says it
	// drifted, Differs naming the leaves learned as noise.
	// AnswerByKind: a background write answered by its kind, unchecked;
	// Reused says the kind's recordings were used up. AnswerBackgroundAgain:
	// a background call made again in its test's window, answered again with
	// ClosestMock, the marked recording it repeated, unchecked. An answer
	// never counts against a test and never lands in a miss list.
	Answer    string `json:"answer,omitempty" yaml:"answer,omitempty"`
	Drift     bool   `json:"drift,omitempty" yaml:"drift,omitempty"`
	Reused    bool   `json:"reused,omitempty" yaml:"reused,omitempty"`
	WriteKind string `json:"write_kind,omitempty" yaml:"write_kind,omitempty"`
	// Source is the process that made the call (Origin's Comm and Exe; Clock
	// unused), which the proxy's sink fills from the connection's pid, for a
	// miss and an answer alike.
	Source *Origin `json:"source,omitempty" yaml:"source,omitempty"`
	// Repeat mirrors MockMismatchReport.Repeat: the call is identical to a
	// recording an earlier test of the set made (ClosestMock, or
	// RepeatNote.Mock for a NotOwned call, whose ClosestMock stays the
	// recording made elsewhere), which already answered that test's call at
	// this replay; the replay names that test (RepeatNote.Test). A call a
	// pairing answered (Answer) carries it by a NotOwned miss's rule. nil
	// otherwise.
	Repeat *RepeatNote `json:"repeat_of,omitempty" yaml:"repeat_of,omitempty"`
}

// ConcurrentNote is what keploy knows of a call made by a test that was
// recorded while other tests' requests were open (UnmatchedCall.Concurrent):
// names of tests and mocks only, never a value.
type ConcurrentNote struct {
	// With are the other tests whose requests were open beside this test's
	// while the call's recordings were made, in window order; Unkept counts
	// those the recording did not keep (deleted, or lost).
	With   []string `json:"with,omitempty" yaml:"with,omitempty"`
	Unkept int      `json:"unkept,omitempty" yaml:"unkept,omitempty"`
	// AttributedTo is the test the recording closest to the call, one made
	// while this test's request and others' were open, is listed under by
	// time alone (the one whose request came first), when it is not this
	// test. WhyNot says why this replay did not share that recording with
	// this test.
	AttributedTo string `json:"attributed_to,omitempty" yaml:"attributed_to,omitempty"`
	// MayBe are, for a call with no closest recording, the other tests that
	// list a recording of the call's own kind (KindsOfProtocol) made while
	// their requests and this test's were open, which this replay did not
	// share with this test: the call's recording may be one of theirs.
	// WhyNot then says why they were not shared, when it is one reason for
	// all of them.
	MayBe []string `json:"may_be,omitempty" yaml:"may_be,omitempty"`
	// Claimant is the test that answered its call with the recording this
	// call pairs with, recorded once while both were open.
	Claimant string `json:"claimant,omitempty" yaml:"claimant,omitempty"`
	// Class names the recordings keploy cannot tell apart for this call
	// (Case ConcurrentCaseClass: they got different answers;
	// ConcurrentCaseIncomparable: each is closer to it in some place).
	Class []string `json:"class,omitempty" yaml:"class,omitempty"`
	// Case is what keploy record's full round could not settle of the call
	// (ConcurrentCase*), for a call it charged with MissConcurrentAmbiguous
	// or MissConcurrentConflict; "" on a call it merely notes.
	Case string `json:"case,omitempty" yaml:"case,omitempty"`
	// Places are the call's places that differ from the recording closest
	// to it (bind.$N, literal.N), for ConcurrentCaseNotOwn.
	Places []string `json:"places,omitempty" yaml:"places,omitempty"`
	// WhyNot says why keploy did not share the call's recordings between
	// the tests (ConcurrentNotShared*), when it did not; Kind is the kind
	// keploy shares none of yet (ConcurrentNotSharedKind), and Stated what
	// the recording states that keploy does not share
	// (ConcurrentNotSharedStates: noise, variables, volatile positions, a
	// redacted value).
	WhyNot string `json:"why_not,omitempty" yaml:"why_not,omitempty"`
	Kind   string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Stated string `json:"stated,omitempty" yaml:"stated,omitempty"`
}

// What keploy record's full round could not settle of a call of a test
// recorded beside others (ConcurrentNote.Case).
const (
	// ConcurrentCaseClass: recordings of the call keploy cannot tell apart
	// got different answers.
	ConcurrentCaseClass = "class"
	// ConcurrentCaseIncomparable: the recordings closest to the call are
	// each closer to it in some place.
	ConcurrentCaseIncomparable = "incomparable"
	// ConcurrentCaseNotOwn: it differs (at Places) from the recording
	// closest to it (Class), which another of the tests open while it was
	// recorded may have made: keploy does not learn what changed against a
	// recording another test may own.
	ConcurrentCaseNotOwn = "not_own"
	// ConcurrentCaseRetry: its attempts were answered by different
	// recordings.
	ConcurrentCaseRetry = "retry"
	// ConcurrentCaseConflict: it and another test's call (Claimant) pair
	// with one recording.
	ConcurrentCaseConflict = "conflict"
	// ConcurrentCaseUnresolved: the test lists a recording keploy record's
	// full round could not give to one test (MockEntryUnresolvedAmbiguous).
	ConcurrentCaseUnresolved = "unresolved"
)

// Why keploy did not share a call's recordings between the tests recorded
// beside each other (ConcurrentNote.WhyNot).
const (
	// ConcurrentNotSharedKind: keploy shares no recording of the kind yet.
	ConcurrentNotSharedKind = "kind"
	// ConcurrentNotSharedStates: the recording states what keploy does not
	// share (ConcurrentNote.Stated).
	ConcurrentNotSharedStates = "states"
	// ConcurrentNotSharedStrict: only keploy record's full round shares
	// recordings; this replay serves each to the test that lists it.
	ConcurrentNotSharedStrict = "strict"
	// ConcurrentNotSharedUnkept: it was recorded while a test the recording
	// did not keep was open, and no kept test lists it.
	ConcurrentNotSharedUnkept = "unkept"
	// ConcurrentNotSharedCall: keploy shares recordings of the kind, not
	// this one (ConcurrentNote.Kind): for SQL, only queries it compares by
	// value.
	ConcurrentNotSharedCall = "call"
	// ConcurrentNotSharedAgent: keploy record's full round, with an agent
	// that does not share recordings between tests (another version).
	ConcurrentNotSharedAgent = "agent"
	// ConcurrentNotSharedUnproven: keploy record's full round shared it, could
	// not tell which of the tests made its call, and left it with the test
	// that lists it now, which it charged (MockEntry.Unresolved).
	ConcurrentNotSharedUnproven = "unproven"
)

// Clone is a copy of n that shares nothing with it; nil for nil.
func (n *ConcurrentNote) Clone() *ConcurrentNote {
	if n == nil {
		return nil
	}
	c := *n
	c.With = append([]string(nil), n.With...)
	c.MayBe = append([]string(nil), n.MayBe...)
	c.Class = append([]string(nil), n.Class...)
	c.Places = append([]string(nil), n.Places...)
	return &c
}

// The reasons a call of a test recorded beside other tests carries
// (UnmatchedCall.Reason) when keploy record's full round, which shares their
// recordings, cannot tell which of them made it.
const (
	// MissConcurrentAmbiguous: the call's recordings, made while several
	// tests' requests were open, cannot be told apart for it: they got
	// different answers, or the learn step found none of them closest, or
	// the one closest to it is one another of the tests may have made
	// (keploy learns nothing against such a recording), or retries were
	// answered by different recordings.
	MissConcurrentAmbiguous = "concurrent_ambiguous"
	// MissConcurrentConflict: the call pairs with a recording another test
	// that was open beside it already answered with: one of the two made a
	// call it did not record.
	MissConcurrentConflict = "concurrent_conflict"
)

// The answers an answered call reports (UnmatchedCall.Answer,
// MockMismatchReport.Answer, ReportAnswer), under the proven switch.
const (
	// AnswerProvisional: record's full round paired the call with a recording
	// another test owns and answered it with that recording.
	AnswerProvisional = "provisional"
	// AnswerByKind: a background write answered by its write kind, unchecked.
	AnswerByKind = "by_kind"
	// AnswerBackgroundAgain: a call identical to a recording of the open
	// test that judging marked as the application's background work
	// (MockEntry.Background), made again after that recording answered in
	// the test's window, answered again with it (ClosestMock), unchecked.
	AnswerBackgroundAgain = "background_again"
)

// The reasons a miss of record's full round carries (UnmatchedCall.Reason),
// as the learn step decides them (package matcher/learn).
const (
	// MissRequestChanged: a recorded call had the request's method,
	// destination and shape, but the request differs from it in a way the
	// learn step does not learn (a field added or removed, a type changed, a
	// value several recorded calls explain equally, a value that flows from an
	// earlier call when nothing can follow it).
	MissRequestChanged = "request_changed"
	// MissUnstableOrder: the closest recorded call holds an array the request
	// sent with the recorded elements in another order (the order rule).
	MissUnstableOrder = "unstable_order"
	// MissUnmockedCall: no recorded call has the request's method,
	// destination and shape.
	MissUnmockedCall = "unmocked_call"
)

// MockOutcome is the end of a mock replay as the agent saw it: the mocks it
// served and the outgoing calls that matched none of them -- what
// /consumedmocks and /mockerrors answer, in one document. A containerised
// agent writes it as it stops (pkg/platform/docker AgentOutcomeFile), for the
// one caller that cannot ask it over HTTP: a docker compose run, where the
// agent is stopped along with the app.
type MockOutcome struct {
	Consumed []MockState     `json:"consumed"`
	Missed   []UnmatchedCall `json:"missed"`
	// Windows are the per-test scopes the runner reported (with their
	// verdicts), which a CLI that can no longer reach the agent — under
	// compose it is stopped with the runner — reads from here instead. A
	// window is a few hundred bytes, so they share the account's read bound
	// with the served and missed calls comfortably up to suites of tens of
	// thousands of tests.
	Windows []ScopeWindow `json:"windows,omitempty"`
	// MissedUnread is why Missed may be missing calls, empty when it is
	// complete: the agent's read of the calls no mock answered was
	// incomplete (ErrUnmatchedCallsUnread, under the proven switch). Missed
	// holds what was read.
	MissedUnread string `json:"missed_unread,omitempty"`
	// PIDMapRefusals are the inserts into the agent's PID maps the kernel
	// refused over its life, which under compose is the replay's: processes
	// its hooks could not track. nil when it cannot count them, which is
	// "unknown".
	PIDMapRefusals *PIDMapRefusals `json:"pidMapRefusals,omitempty"`
	// PIDMapRefusalsError is why hooks that count those refusals could not be
	// read as the agent stopped; "" when they were read, or when the hooks do
	// not count them at all. It is what tells the CLI "the count was lost"
	// (completeness unproven: --strict fails it) from "this agent never
	// counts" (an older agent, or hooks without PID maps), which a nil
	// PIDMapRefusals alone cannot.
	PIDMapRefusalsError string `json:"pidMapRefusalsError,omitempty"`
}

// MockSummaryFromSpec builds a protocol-generic summary string from a mock's spec.
func MockSummaryFromSpec(mock *Mock) string {
	if d := mock.onDisk; d != nil {
		if d.longSummary {
			return d.display(mock, MockSummaryFromSpec, string(mock.Kind))
		}
		return d.summary
	}
	if mock.Spec.HTTPReq != nil {
		return string(mock.Spec.HTTPReq.Method) + " " + mock.Spec.HTTPReq.URL
	}
	if mock.Spec.DNSReq != nil {
		return "DNS " + mock.Spec.DNSReq.Name
	}
	if len(mock.Spec.MongoRequests) > 0 {
		if op := mock.Spec.Metadata["operation"]; op != "" {
			return "MongoDB " + op
		}
		return "MongoDB"
	}
	if len(mock.Spec.MySQLRequests) > 0 {
		if op := mock.Spec.Metadata["operation"]; op != "" {
			return "MySQL " + op
		}
		return "MySQL"
	}
	if len(mock.Spec.PostgresRequestsV2) > 0 {
		if op := mock.Spec.Metadata["operation"]; op != "" {
			return "PostgreSQL " + op
		}
		return "PostgreSQL"
	}
	if mock.Spec.GRPCReq != nil {
		if op := mock.Spec.Metadata["operation"]; op != "" {
			return "gRPC " + op
		}
		return "gRPC"
	}
	if op := mock.Spec.Metadata["operation"]; op != "" {
		return string(mock.Kind) + " " + op
	}
	return string(mock.Kind)
}

func (tr *TestResult) GetKind() string {
	return string(tr.Kind)
}

type TestSetStatus string

// constants for testSet status
const (
	TestSetStatusRunning      TestSetStatus = "RUNNING"
	TestSetStatusFailed       TestSetStatus = "FAILED"
	TestSetStatusPassed       TestSetStatus = "PASSED"
	TestSetStatusAppHalted    TestSetStatus = "APP_HALTED"
	TestSetStatusUserAbort    TestSetStatus = "USER_ABORT"
	TestSetStatusFaultUserApp TestSetStatus = "APP_FAULT"
	TestSetStatusInternalErr  TestSetStatus = "INTERNAL_ERR"
	TestSetStatusFaultScript  TestSetStatus = "SCRIPT_FAULT"
	TestSetStatusIgnored      TestSetStatus = "IGNORED"
	TestSetStatusNoTestsToRun TestSetStatus = "NO_TESTS_TO_RUN"
)

// SetEnd is how keploy test ended one run of a test set (each attempt, when
// maxFlakyChecks or --must-pass runs it again): the status the run ended it
// with, and why, as the set's final report's FailureReason says, or the error
// that ended the set before such a report was written. A set can end neither PASSED nor FAILED
// with only passing results (the app halted after some tests passed, or
// keploy could not judge the set), and FAILED with none failing (a capture
// loss after its last test): its results alone do not say how it ended.
type SetEnd struct {
	Status TestSetStatus `json:"status" yaml:"status"`
	Why    string        `json:"why,omitempty" yaml:"why,omitempty"`
}

func StringToTestSetStatus(s string) (TestSetStatus, error) {
	switch s {
	case "RUNNING":
		return TestSetStatusRunning, nil
	case "FAILED":
		return TestSetStatusFailed, nil
	case "PASSED":
		return TestSetStatusPassed, nil
	case "APP_HALTED":
		return TestSetStatusAppHalted, nil
	case "USER_ABORT":
		return TestSetStatusUserAbort, nil
	case "APP_FAULT":
		return TestSetStatusFaultUserApp, nil
	case "INTERNAL_ERR":
		return TestSetStatusInternalErr, nil
	case "NO_TESTS_TO_RUN":
		return TestSetStatusNoTestsToRun, nil
	default:
		return "", errors.New("invalid TestSetStatus value")
	}
}

type RiskLevel string

const (
	None   RiskLevel = "NONE"
	Low    RiskLevel = "LOW"
	Medium RiskLevel = "MEDIUM"
	High   RiskLevel = "HIGH"
)

type FailureCategory string

const (
	SchemaUnchanged   FailureCategory = "SCHEMA_UNCHANGED"    // schema is identical
	SchemaAdded       FailureCategory = "SCHEMA_ADDED"        // only new fields added; backward compatible
	SchemaBroken      FailureCategory = "SCHEMA_BROKEN"       // removed/changed fields, type mismatch, or entirely different schema
	StatusCodeChanged FailureCategory = "STATUS_CODE_CHANGED" // status code changed
	HeaderChanged     FailureCategory = "HEADER_CHANGED"      // header changed
	InternalFailure   FailureCategory = "INTERNAL_FAILURE"    // internal error in the tool
	// AppConnectionError means the app produced NO response at all (connection
	// refused/reset/EOF/host unreachable) — a transport/availability failure, not
	// a content regression. The synthetic status_code=0 such a failure records
	// must not be read as a STATUS_CODE_CHANGED regression.
	AppConnectionError FailureCategory = "APP_CONNECTION_ERROR"
	// DependencyMissing means a per-test dependency the recording says this
	// test exercised (a mapped, non-reusable-tier mock) went unconsumed during
	// replay — the app stopped making an outgoing call it used to make. Set by
	// the replayer's DepResult writer alongside the DepResult rows themselves
	// (keploy-consumer-design-v2.md §5, false-pass row 0). Attached only to
	// FAILED/OBSOLETE tests so a passing test never grows a failure label.
	DependencyMissing FailureCategory = "DEPENDENCY_MISSING"
	// CallUnmatched: a call the test made got no recorded answer (a miss),
	// which fails it under KEPLOY_PROVEN=1. The calls are in
	// FailureInfo.UnmatchedCalls.
	CallUnmatched FailureCategory = "UNMATCHED_CALL"
	// CaptureIncompleteCategory: keploy's agent could not capture every call
	// while this test or one before it in its test set ran, which fails it
	// under KEPLOY_PROVEN=1. The losses are in FailureInfo.CaptureIncomplete.
	CaptureIncompleteCategory FailureCategory = "CAPTURE_INCOMPLETE"
	// TemplateVariable means a template variable place of the expected
	// response was not satisfied by the answer (a {{capture ...}} that found
	// no value of its type, or a use that did not hold its key's value), or
	// that the test case was refused when its test set loaded, for a
	// placeholder it cannot run with, and never sent.
	// TestResult.TemplateFailures names each place and why.
	TemplateVariable FailureCategory = "TEMPLATE_VARIABLE"
	// TestSetRefused means keploy refused the test case's whole test set and
	// ran none of it: an interrupted keploy templatize left the set
	// half-written. The set's failure reason, and the test case's refusal in
	// TestResult.TemplateFailures, say which files and what to do.
	TestSetRefused FailureCategory = "TEST_SET_REFUSED"
)

// RejectionReason classifies why a test case was marked unreplayable during autoreplay.
// Used by k8s-proxy to populate TestCaseFailureDetails.Reason for the platform API.
type RejectionReason string

const (
	RejectionObsolete       RejectionReason = "OBSOLETE"          // mock mapping mismatch — mocks changed between recordings
	RejectionHighRisk       RejectionReason = "HIGH_RISK_FAILURE" // response structure changed (status code, schema, headers)
	RejectionLowRiskNoNoise RejectionReason = "LOW_RISK_NO_NOISE" // minor diffs that can't be auto-suppressed as noise
)

// ReasonLoopbackOutsideRun is the reason the agent logs when keploy passes a
// call through to a listener owned by the run's own process tree (a self-call)
// instead of serving it from a mock — so the log shows that a self-call reached
// the real handler, not a recording; a broken self-handler fails the run rather
// than passing on a stale mock.
const ReasonLoopbackOutsideRun = "loopback_outside_run"

// NoiseFailureReason explains why automatic noise extraction failed for a LOW_RISK_NO_NOISE test case.
type NoiseFailureReason string

const (
	NoiseFailureNonJSONBody    NoiseFailureReason = "NON_JSON_BODY"         // body is not JSON, can't do field-level diff
	NoiseFailureJSONParseError NoiseFailureReason = "JSON_PARSE_ERROR"      // JSON parse failed on expected or actual body
	NoiseFailureNoDiffsFound   NoiseFailureReason = "NO_DIFFS_FOUND"        // no specific differing fields identified
	NoiseFailureRootLevel      NoiseFailureReason = "ROOT_LEVEL_CHANGE"     // entire response value changed at root level
	NoiseFailureEmptyHeaderKey NoiseFailureReason = "HEADER_ONLY_EMPTY_KEY" // header diff but field name is empty

	// The reasons below are refusals rather than failures: the difference was
	// identified precisely, and suppressing it would delete an assertion instead
	// of tolerating nondeterminism. The first five carry a matcher.NoiseSkipReason
	// through; the last two are the producer's own and have no matcher analogue.
	NoiseFailureStructuralChange  NoiseFailureReason = "STRUCTURAL_CHANGE"    // field appeared, vanished, or changed shape
	NoiseFailureArrayLengthChange NoiseFailureReason = "ARRAY_LENGTH_CHANGE"  // array grew or shrank; only a subtree-wide entry excuses it
	NoiseFailureUnrepresentable   NoiseFailureReason = "UNREPRESENTABLE_PATH" // a JSON key contains ".", so no path names it alone
	NoiseFailureOverBroadPath     NoiseFailureReason = "OVER_BROAD_PATH"      // the path would also silence a field that did not drift
	NoiseFailureOrderAmbiguous    NoiseFailureReason = "ORDER_AMBIGUOUS"      // ignoreOrdering: which array element drifted is unknowable
	NoiseFailureBodyDerivedHeader NoiseFailureReason = "BODY_DERIVED_HEADER"  // e.g. Content-Length; fix the body diff instead
	NoiseFailureTooManyDiffs      NoiseFailureReason = "TOO_MANY_DIFFS"       // a broken endpoint, not a noisy one
	// NoiseFailurePatternTooNarrow — the field already carries a noise entry, but
	// a regex-guarded one whose pattern does not describe the replayed value.
	// Only the author of that pattern can say whether the new value belongs in
	// it, so auto-noise reports it rather than quietly widening the assertion
	// someone deliberately narrowed.
	NoiseFailurePatternTooNarrow NoiseFailureReason = "PATTERN_TOO_NARROW"
	// NoiseFailureUnresolved — the derived entries do not make the matcher accept
	// the response. It is the backstop: the matcher is asked directly rather than
	// modelled, so a difference can never be reported as handled when the next
	// replay would fail on it again.
	NoiseFailureUnresolved NoiseFailureReason = "UNRESOLVED"
	// NoiseFailureUnstableOrder — an array came back in another order (the order
	// rule, matcher.ArrayReordered), and carries matcher.NoiseSkipUnstableOrder
	// through. A refusal: noise on the array's fields would only hide the
	// reorder, so the case stays failed and is judged as unstable order.
	NoiseFailureUnstableOrder NoiseFailureReason = "UNSTABLE_ORDER"
)

// FailureAssessment contains JSON structural analysis of response body differences.
// Populated by the matcher's classifyJSONDifferences function.
type FailureAssessment struct {
	Category      []FailureCategory `json:"category,omitempty" yaml:"category,omitempty"`
	Risk          RiskLevel         `json:"risk,omitempty" yaml:"risk,omitempty"`
	AddedFields   []string          `json:"added_fields,omitempty" yaml:"added_fields,omitempty"`
	RemovedFields []string          `json:"removed_fields,omitempty" yaml:"removed_fields,omitempty"`
	TypeChanges   []string          `json:"type_changes,omitempty" yaml:"type_changes,omitempty"`
	ValueChanges  []string          `json:"value_changes,omitempty" yaml:"value_changes,omitempty"`
	Reasons       []string          `json:"reasons,omitempty" yaml:"reasons,omitempty"`
}

type Result struct {
	StatusCode     IntResult      `json:"status_code" bson:"status_code" yaml:"status_code"`
	FailureInfo    FailureInfo    `json:"-" yaml:"-"`
	HeadersResult  []HeaderResult `json:"headers_result" bson:"headers_result" yaml:"headers_result"`
	BodyResult     []BodyResult   `json:"body_result" bson:"body_result" yaml:"body_result"`
	BodySizeResult IntResult      `json:"body_size_result,omitempty" bson:"body_size_result,omitempty" yaml:"body_size_result,omitempty"` // used when body was skipped (>1MB)
	// DepResult carries per-test DEPENDENCY assertions: ONE ROW PER OUTGOING
	// DEPENDENCY THAT WENT MISSING — a call the recording says this test
	// exercised that was NOT observed during the test's window
	// (Meta[i].Normal == false). Written by the replayer's standard
	// (non-streaming) per-test result block for every test — passed, failed or
	// obsolete — from the existing expected-vs-consumed mock bookkeeping.
	//
	// EMPTY IS NOT "NOT CHECKED": it means nothing went missing OR the
	// assertion never ran, and the two are told apart by DepsChecked below,
	// never by len(DepResult). Read it through Result.DependenciesChecked().
	//
	// SCOPE, so an absent row is not misread as coverage: only mocks recorded
	// as PER-TEST tier are asserted. Session/connection-tier mocks and DNS are
	// excluded, and an untagged HTTP / HTTP2 / Postgres / MySQL / Generic mock
	// is classified session-tier by Mock.DeriveLifetime — so an ordinary
	// recorded outgoing call is NOT necessarily covered, and a recording whose
	// mocks carry no tier tag produces no rows and DepsChecked=false for every
	// test.
	//
	// The dependencies that WERE exercised are NOT persisted here, in any
	// mode: they collapse into the DepsConsumed count. Measured through
	// reportdb.InsertReport, one row per consumed dependency costs ~190 bytes
	// of YAML each, and a Postgres-chatty suite consumes 50-200 mocks per
	// test — 3-11 MB per test-set report of `consumed/consumed` boilerplate no
	// consumer reads. The identities are already persisted by
	// TestResult.MatchedCalls.
	//
	// COMPATIBILITY: statuses, exit codes and every other field are unchanged
	// when nothing is missing and --assert-dependencies is off, and a test
	// that loses nothing serializes `dep_result: []` exactly as it did before
	// this field had a writer — the only added bytes are the two scalars
	// below.
	//
	// Deliberately carries NO omitempty — every report written before this
	// field had a writer already serializes `dep_result: []`, and adding
	// omitempty would change every existing report on disk.
	//
	// See keploy-consumer-design-v2.md §2 and §7 slice 4.
	DepResult []DepResult `json:"dep_result" bson:"dep_result" yaml:"dep_result"`

	// DepsChecked is the persisted proof that the per-test dependency
	// assertion RAN for this test, over AT LEAST ONE ELIGIBLE DEPENDENCY. It
	// is the ONE BIT that makes `dep_result: []` unambiguous: without it a
	// test that lost nothing and a test whose assertion never executed (a
	// --base-path / remote-agent run, --disable-mapping, a test set with no
	// usable mappings.yaml, a failed per-test consumed-mock fetch, the
	// deferred streaming path, a test the mapping records no dependency for, a
	// test whose every mapped dependency was ineligible, or any report written
	// before this field had a writer) are byte-identical on disk — and a
	// consumer applying the documented "any(matched == false)" rule reports
	// "no dependency regressions" for a run in which the question was never
	// asked.
	//
	// ELIGIBLE means recorded as PER-TEST tier. Session/connection-tier mocks
	// are excluded from the assertion (shared across every test, so not
	// attributable to one test's window) and so is DNS. Because
	// Mock.DeriveLifetime classifies an untagged HTTP / HTTP2 / Postgres /
	// MySQL / Generic mock as session-tier, a recording whose mocks carry no
	// per-test tier tag has nothing eligible and this bit is false for every
	// one of its tests — which is the honest answer, not a clean one.
	//
	// omitempty on purpose: false is the pre-slice-4 value, so an unchecked
	// test and every report already on disk serialize exactly as before. A
	// checked test costs one short key instead of the ~224-byte aggregate
	// DepResult row an earlier revision of this slice wrote for the same bit.
	//
	// Read it through Result.DependenciesChecked().
	DepsChecked bool `json:"deps_checked,omitempty" bson:"deps_checked,omitempty" yaml:"deps_checked,omitempty"`

	// DepsConsumed is how many recorded per-test dependencies WERE observed
	// during this test's window. Meaningful only when DepsChecked is true;
	// omitempty keeps it off the wire for an unchecked test and for a checked
	// test that consumed nothing.
	//
	// It replaces the per-dependency matched rows (see DepResult above): the
	// magnitude is what a report consumer uses — "12 of 12 exercised" versus
	// "0 of 12" is the difference between a healthy test and an app that never
	// reached its database — while the identities live in
	// TestResult.MatchedCalls.
	DepsConsumed int `json:"deps_consumed,omitempty" bson:"deps_consumed,omitempty" yaml:"deps_consumed,omitempty"`

	TrailerResult []HeaderResult `json:"trailer_result,omitempty" bson:"trailer_result,omitempty" yaml:"trailer_result,omitempty"`
}

type DepResult struct {
	Name string          `json:"name" bson:"name" yaml:"name"`
	Type string          `json:"type" bson:"type" yaml:"type"`
	Meta []DepMetaResult `json:"meta" bson:"meta" yaml:"meta"`
}

type DepMetaResult struct {
	Normal   bool   `json:"normal" bson:"normal" yaml:"normal"`
	Key      string `json:"key" bson:"key" yaml:"key"`
	Expected string `json:"expected" bson:"expected" yaml:"expected"`
	Actual   string `json:"actual" bson:"actual" yaml:"actual"`
}

type IntResult struct {
	Normal   bool `json:"normal" bson:"normal" yaml:"normal"`
	Expected int  `json:"expected" bson:"expected" yaml:"expected"`
	Actual   int  `json:"actual" bson:"actual" yaml:"actual"`
}

type HeaderResult struct {
	Normal   bool   `json:"normal" bson:"normal" yaml:"normal"`
	Expected Header `json:"expected" bson:"expected" yaml:"expected"`
	Actual   Header `json:"actual" bson:"actual" yaml:"actual"`
}

type Header struct {
	Key   string   `json:"key" bson:"key" yaml:"key"`
	Value []string `json:"value" bson:"value" yaml:"value"`
}

type BodyResult struct {
	Normal   bool     `json:"normal" bson:"normal" yaml:"normal"`
	Type     BodyType `json:"type" bson:"type" yaml:"type"`
	Expected string   `json:"expected" bson:"expected" yaml:"expected"`
	Actual   string   `json:"actual" bson:"actual" yaml:"actual"`
}

type TestStatus string

// constants for test status
const (
	TestStatusPending  TestStatus = "PENDING"
	TestStatusRunning  TestStatus = "RUNNING"
	TestStatusFailed   TestStatus = "FAILED"
	TestStatusPassed   TestStatus = "PASSED"
	TestStatusIgnored  TestStatus = "IGNORED"
	TestStatusObsolete TestStatus = "OBSOLETE"
)

type (
	Noise        map[string][]string
	GlobalNoise  map[string]map[string][]string
	TestsetNoise map[string]map[string]map[string][]string
)
