package models

import (
	"errors"
	"fmt"
	"strings"
)

type AppError struct {
	AppErrorType AppErrorType
	Err          error
	AppLogs      string
	// ExitCode is the wrapped application/test-runner's process exit code
	// when it is known (a Runtime exit carrying an *exec.ExitError). It is
	// -1 when no exit code is available (the command never started, was
	// killed by a signal, or exited cleanly with code 0 via ErrAppStopped).
	// The mock record/replay flows propagate this as keploy's own exit code
	// so a wrapped `pytest`/`go test` failure fails the keploy process too.
	ExitCode int
}

type AppErrorType string

func (e AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.AppErrorType, e.Err)
	}
	return string(e.AppErrorType)
}

// AppErrorType is a type of error that can be returned by the application
const (
	ErrCommandError   AppErrorType = "exited due to command error"
	ErrUnExpected     AppErrorType = "an unexpected error occurred"
	ErrInternal       AppErrorType = "an internal error occurred"
	ErrAppStopped     AppErrorType = "app stopped"
	ErrCtxCanceled    AppErrorType = "context canceled"
	ErrTestBinStopped AppErrorType = "test binary stopped"
)

// MockFieldDiffKind classifies a single field-level divergence between a live
// outgoing request and the closest recorded mock.
type MockFieldDiffKind string

const (
	DiffKindValueChanged  MockFieldDiffKind = "value_changed"   // same field, different value
	DiffKindTypeChanged   MockFieldDiffKind = "type_changed"    // same field, different JSON type
	DiffKindMissingInLive MockFieldDiffKind = "missing_in_live" // recorded mock has it, live request doesn't
	DiffKindMissingInMock MockFieldDiffKind = "missing_in_mock" // live request has it, recorded mock doesn't
)

// MockFieldDiff is one field-level difference between the live request and the
// closest candidate mock. Path uses the same vocabulary as the noise
// configuration (pkg/matcher): "body.<dotted.json.path>", "header.<name>",
// "query.<name>", plus the pseudo-fields "method" and "path". This shared
// grammar is deliberate: a user can copy Path straight into
// test.globalNoise / spec.assertions.noise.
type MockFieldDiff struct {
	Path     string            `json:"path" yaml:"path"`
	Kind     MockFieldDiffKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	Expected string            `json:"expected,omitempty" yaml:"expected,omitempty"` // recorded (mock) value
	Actual   string            `json:"actual,omitempty" yaml:"actual,omitempty"`     // live request value
}

// Match-cascade phases recorded on MockMismatchReport.MatchPhase. They tell
// the user how far the matcher got before giving up, which determines the
// right remediation (re-record vs add noise vs fix candidate selection).
const (
	MatchPhaseNoMocks    = "no_mocks"             // no mocks were available to compare for this protocol: the pool is empty, or none survived filtering
	MatchPhaseSchema     = "no_schema_candidates" // nothing matched method/path/header-keys/query-keys
	MatchPhaseBody       = "body_mismatch"        // schema candidates existed, request body ruled them all out
	MatchPhaseStrict     = "strict_noise_reject"  // candidates rejected by strict req-body-noise enforcement
	MatchPhaseExhausted  = "no_match"             // full cascade ran and nothing matched
	MatchPhaseProtoError = "protocol_error"       // matching aborted on a protocol/decode error
)

// Destination-scope verdicts recorded on MockMismatchReport.DestinationScope.
// They answer a question the match cascade cannot: did any of the mocks this
// miss was actually compared against target the same upstream as the live
// call? A "no" is the signature of a call that was never in scope for the
// compared set — in Kubernetes typically a sibling container's egress, which
// replay intercepts pod-wide while record arms only the one container the
// user named, but equally endpoint/config drift between the recording and
// replay environments, or a per-test mock window that excluded the mock this
// call needed. See OutOfScopeDestinationCauses for the user-facing wording.
//
// The claim is deliberately LOCAL. Keploy cannot say from here that a
// destination was "never recorded": per-test mocks are consumed on match and
// stripped from every later pool, so a host whose mocks have all been served
// is missing from the pool while having been recorded all along. The verdict
// therefore speaks only for the compared set, which is evidence the report
// builder holds in its hand.
//
// Deliberately a three-state field, not a bool. "We checked and the
// destination was in the compared set" and "we could not check" are different
// facts, and collapsing them would make every protocol that never supplies
// destination evidence (Mongo, MySQL, Generic, ...) read as "checked, and it
// WAS there" — an unchecked negative asserted as a fact. The scope verdict
// travels ALONGSIDE MatchPhase, never on top of it: MatchPhase keeps
// reporting where the cascade actually stopped, which is the triage
// information a report reader needs next.
const (
	// DestinationScopeUnknown — no verdict. Either the protocol supplied no
	// destination evidence, or the evidence was not conclusive. Rendered as
	// an absent field everywhere; never as a negative.
	DestinationScopeUnknown = ""
	// DestinationScopeInComparedSet — at least one mock the matcher compared
	// against targets this destination, so the miss is a genuine
	// drift/candidate problem.
	DestinationScopeInComparedSet = "in_compared_set"
	// DestinationScopeNotInComparedSet — no mock in the compared set targets
	// this destination. It is NOT a claim that the destination was never
	// recorded; it is the strongest statement local evidence supports.
	DestinationScopeNotInComparedSet = "not_in_compared_set"
)

// OutOfScopeDestinationCauses explains a DestinationScopeNotInComparedSet
// miss: why nothing the matcher compared against targeted that upstream, and
// what to do about it.
//
// It is STATIC text and renderers MUST emit it ONCE PER TEST, never once per
// missed call. A single out-of-scope container produces one such miss per
// outgoing call it makes — 23 of 28 unmatched calls in the recording this
// diagnostic was built from — and repeating a paragraph per call buries the
// per-call facts (which upstream, which cascade stop) under kilobytes of
// identical prose. The per-call NextSteps therefore carries only the sentence
// that is specific to that call; everything shared lives here.
//
// Deliberately MODE-NEUTRAL. Keploy runs natively ("keploy record -c 'go run
// .'"), under docker, and in Kubernetes; only the Kubernetes mode has a pod
// whose sibling containers replay intercepts while record armed just one. The
// pod model is named as a Kubernetes specific rather than asserted at every
// user, so a native or docker user is not handed guidance that cannot apply
// to them.
//
// Cause (3) is keploy's own failure mode and has to be listed: the compared
// set is GetPerTestMocksInWindow() + GetSessionMocks(), so a per-test mock
// recorded for this very dependency but timestamped outside the current
// test's window is not in it and never was — a miss that is neither a
// container-scope mistake, nor endpoint drift, nor ignorable, and that the
// other three causes would send the user off to re-record for nothing.
//
// Wrapped at 100 columns; renderers indent it, they do not re-wrap it.
const OutOfScopeDestinationCauses = `Out-of-scope destinations: no mock in the compared set targeted the upstream these calls went to.
Likely causes:
  (1) The call came from a process or container Keploy was not recording. In Kubernetes, Keploy
      records ONE container per session — the one named when recording started — while replay
      intercepts the whole pod, so a sibling container's egress arrives with no mock behind it.
      Re-record with that container selected.
  (2) The replay environment points the application at a different endpoint than the recording
      environment did (config/env drift: another service address or port). Compare this host with
      the one recorded for the same dependency and align the replay configuration, or re-record
      against the new endpoint.
  (3) The mock for this dependency was recorded but fell outside this test's per-test mock window,
      so it was never in the set this call was compared against. Check the test set's mock
      timestamps and windowing before re-recording.
  (4) The call belongs to a sidecar or agent that is not part of the application under test. That
      miss is expected and can be ignored.
Mocks already served earlier in this run, or recorded outside this test's mock window, are no
longer in the compared set — this describes what was compared for these calls, not the whole
recording.`

// RenderOutOfScopeDestinationCauses returns OutOfScopeDestinationCauses with
// prefix prepended to every line, so the CLI and the file report can each
// indent the block to their own surrounding style without either of them
// re-wrapping (and re-drifting) the text.
func RenderOutOfScopeDestinationCauses(prefix string) string {
	lines := strings.Split(OutOfScopeDestinationCauses, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

// MockMismatchReport describes what didn't match when a mock lookup fails.
// It is populated by protocol-specific matching logic and surfaced to the user
// in the CLI mismatch table, the test-report yaml (FailureInfo.UnmatchedCalls)
// and the platform/UI APIs. Protocol parsers should build it via
// pkg/agent/proxy/integrations/mismatch so vocabulary stays uniform.
type MockMismatchReport struct {
	Protocol      string // "HTTP", "MySQL", "PostgreSQL", "MongoDB", "gRPC", "HTTP/2", "Generic", "DNS"
	ActualSummary string // Brief description of the actual request
	Destination   string // outgoing call's destination/domain (HTTP Host, or host:port) — identifies WHICH upstream missed
	ClosestMock   string // Name of the closest mock (empty if none)
	Diff          string // Human-readable diff (protocol-specific)
	NextSteps     string // Actionable suggestion for the user
	MatchPhase    string // how far the match cascade got (MatchPhase* constants)
	// DestinationScope is the verdict on whether any mock the matcher
	// compared against targeted this call's upstream (DestinationScope*
	// constants; empty means the question was never answered). It is a
	// SEPARATE axis from MatchPhase on purpose — a call whose upstream is
	// absent from the compared set still has a real cascade stop, and
	// overwriting the phase with the verdict destroys that triage fact.
	DestinationScope string
	CandidateCount   int             // protocol mocks considered before giving up
	FieldDiffs       []MockFieldDiff // field-level diffs vs the closest mock, noise-vocabulary paths
	// ClosestMockReq / ReceivedReq are the FULL rendered requests for the CLI
	// side-by-side diff (left = recorded mock, right = live request). They are
	// the human-facing complement to FieldDiffs (machine-readable): the
	// renderer shows the whole mock with differing lines highlighted and falls
	// back to the FieldDiffs table when these are empty (older agents /
	// protocols that don't render whole requests). Sensitive/obfuscated values
	// are redacted by the producing parser before they land here.
	ClosestMockReq string // rendered request of the closest mock
	ReceivedReq    string // rendered request the app actually sent
}

// ErrNoMockMatched is the sentinel for a genuine mock miss — an outgoing call
// for which no recorded mock matched. Protocol parsers wrap it (errors.Is)
// when they report a miss, so the proxy can distinguish real misses from
// infrastructure/decode failures when building UnmatchedCalls for reports.
var ErrNoMockMatched = errors.New("no matching mock found")

type ParserError struct {
	ParserErrorType ParserErrorType
	Err             error
	MismatchReport  *MockMismatchReport // nil when no diff is available
}

type ParserErrorType string

func (e ParserError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.ParserErrorType, e.Err)
	}
	return string(e.ParserErrorType)
}

const (
	ErrMockNotFound ParserErrorType = "mock not found"
)

// mockMismatchError wraps an error with a MockMismatchReport so protocol
// decode layers can carry diff information through the error chain to proxy.go.
type mockMismatchError struct {
	err    error
	report *MockMismatchReport
}

func (e *mockMismatchError) Error() string { return e.err.Error() }
func (e *mockMismatchError) Unwrap() error { return e.err }

// MismatchReport returns the attached diff report.
func (e *mockMismatchError) MismatchReport() *MockMismatchReport { return e.report }

// NewMockMismatchError wraps err with a MockMismatchReport for propagation.
func NewMockMismatchError(err error, report *MockMismatchReport) error {
	if report == nil {
		return err
	}
	return &mockMismatchError{err: err, report: report}
}

// ErrMockEncode marks a mock-persist failure caused by THIS mock's own payload
// — an unsupported kind, a malformed spec, a value the encoder cannot represent
// — rather than by the storage environment.
//
// The distinction decides whether a recording survives. The recorder treats a
// failed mock insert as fatal and tears the whole session down, which is right
// for disk-full or storage-gone (every subsequent mock fails too) and badly
// wrong for one unencodable payload. A single gzip response body ended a
// 46-hour production recording that way. Errors wrapped with this sentinel are
// skipped and counted; everything else stays fatal.
var ErrMockEncode = errors.New("mock encode failure")
