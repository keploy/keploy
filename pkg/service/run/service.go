package run

import (
	"context"
	"time"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Get(ctx context.Context, summary bool, cid string, user, app, id *string, from, to *time.Time, offset *int, limit *int) ([]*TestRun, error)
	Put(ctx context.Context, run TestRun) error
	Normalize(ctx context.Context, cid, id string) error
}

type DB interface {
	Read(ctx context.Context, cid string, user, app, id *string, from, to *time.Time, offset int, limit int) ([]*TestRun, error)
	Upsert(ctx context.Context, run TestRun) error
	ReadOne(ctx context.Context, id string) (*TestRun, error)
	ReadTest(ctx context.Context, id string) (Test, error)
	ReadTests(ctx context.Context, runID string) ([]Test, error)
	PutTest(ctx context.Context, t Test) error
	Increment(ctx context.Context, success, failure bool, id string) error
	//Grpc
	PutTestGrpc(ctx context.Context, t TestGrpc) error
}

type TestRun struct {
	ID      string        `json:"id" bson:"_id"`
	Created int64         `json:"created" bson:"created,omitempty"`
	Updated int64         `json:"updated" bson:"updated,omitempty"`
	Status  TestRunStatus `json:"status" bson:"status"`
	CID     string        `json:"cid" bson:"cid,omitempty"`
	App     string        `json:"app" bson:"app,omitempty"`
	User    string        `json:"user" bson:"user,omitempty"`
	Success int           `json:"success" bson:"success,omitempty"`
	Failure int           `json:"failure" bson:"failure,omitempty"`
	Total   int           `json:"total" bson:"total,omitempty"`
	Tests   []Test        `json:"tests" bson:"-"`
}

type TestRunStatus string

const (
	TestRunStatusRunning TestRunStatus = "RUNNING"
	TestRunStatusFailed  TestRunStatus = "FAILED"
	TestRunStatusPassed  TestRunStatus = "PASSED"
)

type Test struct {
	ID         string              `json:"id" bson:"_id"`
	Status     TestStatus          `json:"status" bson:"status"`
	Started    int64               `json:"started" bson:"started"`
	Completed  int64               `json:"completed" bson:"completed"`
	RunID      string              `json:"run_id" bson:"run_id"`
	TestCaseID string              `json:"testCaseID" bson:"test_case_id"`
	URI        string              `json:"uri" bson:"uri"`
	Req        models.HttpReq      `json:"req" bson:"req"`
	Dep        []models.Dependency `json:"dep" bson:"dep"`
	Resp       models.HttpResp     `json:"http_resp" bson:"http_resp,omitempty"`
	Noise      []string            `json:"noise" bson:"noise"`
	Result     Result              `json:"result" bson:"result"`
}

type TestGrpc struct {
	ID         string              `json:"id" bson:"_id"`
	Status     TestStatus          `json:"status" bson:"status"`
	Started    int64               `json:"started" bson:"started"`
	Completed  int64               `json:"completed" bson:"completed"`
	RunID      string              `json:"run_id" bson:"run_id"`
	TestCaseID string              `json:"testCaseID" bson:"test_case_id"`
	Method     string              `json:"method" bson:"method"`
	Req        string              `json:"req" bson:"req"`
	Dep        []models.Dependency `json:"dep" bson:"dep"`
	Resp       string              `json:"http_resp" bson:"http_resp,omitempty"`
	Noise      []string            `json:"noise" bson:"noise"`
	Result     ResultGrpc          `json:"result" bson:"result"`
}

type Result struct {
	StatusCode    IntResult      `json:"status_code" bson:"status_code"`
	HeadersResult []HeaderResult `json:"headers_result" bson:"headers_result"`
	BodyResult    BodyResult     `json:"body_result" bson:"body_result"`
	DepResult     []DepResult    `json:"dep_result" bson:"dep_result"`
}

type ResultGrpc struct {
	BodyResult BodyResult  `json:"body_result" bson:"body_result"`
	DepResult  []DepResult `json:"dep_result" bson:"dep_result"`
}

type DepResult struct {
	Name string                `json:"name" bson:"name"`
	Type models.DependencyType `json:"type" bson:"type"`
	Meta []DepMetaResult       `json:"meta" bson:"meta"`
}

type DepMetaResult struct {
	Normal   bool   `json:"normal" bson:"normal"`
	Key      string `json:"key" bson:"key"`
	Expected string `json:"expected" bson:"expected"`
	Actual   string `json:"actual" bson:"actual"`
}

type IntResult struct {
	Normal   bool `json:"normal" bson:"normal"`
	Expected int  `json:"expected" bson:"expected"`
	Actual   int  `json:"actual" bson:"actual"`
}

type HeaderResult struct {
	Normal   bool   `json:"normal" bson:"normal"`
	Expected Header `json:"expected" bson:"expected"`
	Actual   Header `json:"actual" bson:"actual"`
}

type Header struct {
	Key   string   `json:"key" bson:"key"`
	Value []string `json:"value" bson:"value"`
}

type BodyResult struct {
	Normal   bool     `json:"normal" bson:"normal"`
	Type     BodyType `json:"type" bson:"type"`
	Expected string   `json:"expected" bson:"expected"`
	Actual   string   `json:"actual" bson:"actual"`
}

type BodyType string

const (
	BodyTypePlain BodyType = "PLAIN"
	BodyTypeJSON  BodyType = "JSON"
)

type TestStatus string

const (
	TestStatusPending TestStatus = "PENDING"
	TestStatusRunning TestStatus = "RUNNING"
	TestStatusFailed  TestStatus = "FAILED"
	TestStatusPassed  TestStatus = "PASSED"
)
