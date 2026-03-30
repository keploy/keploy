package models

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	yamlLib "gopkg.in/yaml.v3"
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

func (tr TestReport) MarshalYAML() (interface{}, error) {
	type alias TestReport

	node := &yamlLib.Node{}
	if err := node.Encode(alias(tr)); err != nil {
		return nil, err
	}
	sanitizeReportYAMLNode(node)
	setLiteralStyleForTopLevelField(node, "app_logs")
	setLiteralStyleForTopLevelField(node, "failure_reason")
	setLiteralStyleForTopLevelField(node, "cmdUsed")

	return node, nil
}

func sanitizeReportYAMLNode(node *yamlLib.Node) {
	if node == nil {
		return
	}

	if node.Kind == yamlLib.ScalarNode && node.Tag == "!!str" {
		node.Value = normalizeReportYAMLText(node.Value)
	}

	for _, child := range node.Content {
		sanitizeReportYAMLNode(child)
	}
}

func setLiteralStyleForTopLevelField(node *yamlLib.Node, fieldName string) {
	mapping := findTopLevelMappingNode(node)
	if mapping == nil {
		return
	}

	for i := 0; i < len(mapping.Content)-1; i += 2 {
		key := mapping.Content[i]
		value := mapping.Content[i+1]
		if key.Value != fieldName {
			continue
		}
		if value.Kind == yamlLib.ScalarNode && value.Tag == "!!str" && strings.Contains(value.Value, "\n") {
			value.Style = yamlLib.LiteralStyle
		}
		return
	}
}

func findTopLevelMappingNode(node *yamlLib.Node) *yamlLib.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yamlLib.MappingNode {
		return node
	}
	if len(node.Content) == 0 {
		return nil
	}
	return findTopLevelMappingNode(node.Content[0])
}

func normalizeReportYAMLText(value string) string {
	if value == "" {
		return ""
	}

	value = strings.ToValidUTF8(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\t", "  ")

	var builder strings.Builder
	builder.Grow(len(value))
	for _, r := range value {
		if r == '\n' {
			builder.WriteRune(r)
			continue
		}
		if r == utf8.RuneError || unicode.IsControl(r) {
			continue
		}
		builder.WriteRune(r)
	}

	return builder.String()
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

type FailureInfo struct {
	Risk     RiskLevel         `json:"risk,omitempty" yaml:"risk,omitempty"`
	Category []FailureCategory `json:"category,omitempty" yaml:"category,omitempty"`
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
