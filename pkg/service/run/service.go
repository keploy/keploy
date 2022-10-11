package run

import (
	"context"
	"time"

	"go.keploy.io/server/pkg/models"
)

type Service interface {
	Get(ctx context.Context, summary bool, cid string, user, app, id *string, from, to *time.Time, offset *int, limit *int) ([]*TestRun, error)
	Put(ctx context.Context, run TestRun, testExport bool, testReportPath string) error
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
}

type TestRun struct {
	ID      string               `json:"id" bson:"_id"`
	Created int64                `json:"created" bson:"created,omitempty"`
	Updated int64                `json:"updated" bson:"updated,omitempty"`
	Status  models.TestRunStatus `json:"status" bson:"status"`
	CID     string               `json:"cid" bson:"cid,omitempty"`
	App     string               `json:"app" bson:"app,omitempty"`
	User    string               `json:"user" bson:"user,omitempty"`
	Success int                  `json:"success" bson:"success,omitempty"`
	Failure int                  `json:"failure" bson:"failure,omitempty"`
	Total   int                  `json:"total" bson:"total,omitempty"`
	Tests   []Test               `json:"tests" bson:"-"`
}

type Test struct {
	ID         string              `json:"id" bson:"_id"`
	Status     models.TestStatus   `json:"status" bson:"status"`
	Started    int64               `json:"started" bson:"started"`
	Completed  int64               `json:"completed" bson:"completed"`
	RunID      string              `json:"run_id" bson:"run_id"`
	TestCaseID string              `json:"testCaseID" bson:"test_case_id"`
	URI        string              `json:"uri" bson:"uri"`
	Req        models.HttpReq      `json:"req" bson:"req"`
	Dep        []models.Dependency `json:"dep" bson:"dep"`
	Resp       models.HttpResp     `json:"http_resp" bson:"http_resp,omitempty"`
	Noise      []string            `json:"noise" bson:"noise"`
	Result     models.Result       `json:"result" bson:"result"`
}
