package models

type TestReport struct {
	Version Version      `json:"version" yaml:"version"`
	Name    string       `json:"name" yaml:"name"`
	Status  string       `json:"status" yaml:"status"`
	Success int          `json:"success" yaml:"success"`
	Failure int          `json:"failure" yaml:"failure"`
	Total   int          `json:"total" yaml:"total"`
	Tests   []TestResult `json:"tests" yaml:"tests,omitempty"`
	TestSet string       `json:"testSet" yaml:"test_set"`
}

func (tr *TestReport) GetKind() string {
	return "TestReport"
}

type TestResult struct {
	Kind         Kind       `json:"kind" yaml:"kind"`
	Name         string     `json:"name" yaml:"name"`
	Status       TestStatus `json:"status" yaml:"status"`
	Started      int64      `json:"started" yaml:"started"`
	Completed    int64      `json:"completed" yaml:"completed"`
	TestCasePath string     `json:"testCasePath" yaml:"test_case_path"`
	MockPath     string     `json:"mockPath" yaml:"mock_path"`
	TestCaseID   string     `json:"testCaseID" yaml:"test_case_id"`
	Req          HttpReq    `json:"req" yaml:"req,omitempty"`
	Res          HttpResp   `json:"resp" yaml:"resp,omitempty"`
	Noise        Noise      `json:"noise" yaml:"noise,omitempty"`
	Result       Result     `json:"result" yaml:"result"`
}

func (tr *TestResult) GetKind() string {
	return string(tr.Kind)
}

type TestRunStatus string

const (
	TestRunStatusRunning      TestRunStatus = "RUNNING"
	TestRunStatusFailed       TestRunStatus = "FAILED"
	TestRunStatusPassed       TestRunStatus = "PASSED"
	TestRunStatusAppHalted    TestRunStatus = "APP_HALTED"
	TestRunStatusUserAbort    TestRunStatus = "USER_ABORT"
	TestRunStatusFaultUserApp TestRunStatus = "APP_FAULT"
)

type Result struct {
	StatusCode    IntResult      `json:"status_code" bson:"status_code" yaml:"status_code"`
	HeadersResult []HeaderResult `json:"headers_result" bson:"headers_result" yaml:"headers_result"`
	BodyResult    []BodyResult   `json:"body_result" bson:"body_result" yaml:"body_result"`
	DepResult     []DepResult    `json:"dep_result" bson:"dep_result" yaml:"dep_result"`
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
	Expected string   `json:"expected" bson:"expected" yaml:"expected"`
	Actual   string   `json:"actual" bson:"actual" yaml:"actual"`
	Type     BodyType `json:"type" bson:"type" yaml:"type"`
}

type TestStatus string

const (
	TestStatusPending TestStatus = "PENDING"
	TestStatusRunning TestStatus = "RUNNING"
	TestStatusFailed  TestStatus = "FAILED"
	TestStatusPassed  TestStatus = "PASSED"
)
