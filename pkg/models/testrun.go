package models

import (
	"errors"
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
	Logs          string       `json:"logs,omitempty" yaml:"logs,omitempty"`
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
}

// MockMismatchMock identifies a mock in the expected/actual mock sets for OBSOLETE test cases.
type MockMismatchMock struct {
	Name string `json:"name" yaml:"name"`
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty"`
}

// MockMismatchInfo records the expected vs actual mock sets when a test case becomes obsolete
// due to mock mapping divergence (mocks were added/removed between recording and replay).
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
	ClosestMock   string `json:"closest_mock,omitempty" yaml:"closest_mock,omitempty"`     // internal mock reference for View Closest
	Diff          string `json:"diff,omitempty" yaml:"diff,omitempty"`
	NextSteps     string `json:"next_steps,omitempty" yaml:"next_steps,omitempty"` // actionable remediation guidance from the matcher
}

// MockSummaryFromSpec builds a protocol-generic summary string from a mock's spec.
func MockSummaryFromSpec(mock *Mock) string {
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
	if len(mock.Spec.RedisRequests) > 0 {
		return "Redis"
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
)

// RejectionReason classifies why a test case was marked unreplayable during autoreplay.
// Used by k8s-proxy to populate TestCaseFailureDetails.Reason for the platform API.
type RejectionReason string

const (
	RejectionObsolete       RejectionReason = "OBSOLETE"          // mock mapping mismatch — mocks changed between recordings
	RejectionHighRisk       RejectionReason = "HIGH_RISK_FAILURE" // response structure changed (status code, schema, headers)
	RejectionLowRiskNoNoise RejectionReason = "LOW_RISK_NO_NOISE" // minor diffs that can't be auto-suppressed as noise
)

// NoiseFailureReason explains why automatic noise extraction failed for a LOW_RISK_NO_NOISE test case.
type NoiseFailureReason string

const (
	NoiseFailureNonJSONBody    NoiseFailureReason = "NON_JSON_BODY"         // body is not JSON, can't do field-level diff
	NoiseFailureJSONParseError NoiseFailureReason = "JSON_PARSE_ERROR"      // JSON parse failed on expected or actual body
	NoiseFailureNoDiffsFound   NoiseFailureReason = "NO_DIFFS_FOUND"        // no specific differing fields identified
	NoiseFailureRootLevel      NoiseFailureReason = "ROOT_LEVEL_CHANGE"     // entire response value changed at root level
	NoiseFailureEmptyHeaderKey NoiseFailureReason = "HEADER_ONLY_EMPTY_KEY" // header diff but field name is empty
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
	DepResult      []DepResult    `json:"dep_result" bson:"dep_result" yaml:"dep_result"`
	TrailerResult  []HeaderResult `json:"trailer_result,omitempty" bson:"trailer_result,omitempty" yaml:"trailer_result,omitempty"`
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
