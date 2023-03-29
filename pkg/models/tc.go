package models

import (
	"context"
	"errors"
	"net/http"
	"strings"

	proto "go.keploy.io/server/grpc/regression"
)

type TestCase struct {
	ID       string `json:"id" bson:"_id"`
	Created  int64  `json:"created" bson:"created,omitempty"`
	Updated  int64  `json:"updated" bson:"updated,omitempty"`
	Captured int64  `json:"captured" bson:"captured,omitempty"`
	CID      string `json:"cid" bson:"cid,omitempty"`
	AppID    string `json:"app_id" bson:"app_id,omitempty"`
	URI      string `json:"uri" bson:"uri,omitempty"`
	// GrpcMethod string              `json:"grpc_method" bson:"grpc_method,omitempty"`
	HttpReq  HttpReq             `json:"http_req" bson:"http_req,omitempty"`
	HttpResp HttpResp            `json:"http_resp" bson:"http_resp,omitempty"`
	GrpcReq  GrpcReq             `json:"grpc_req" bson:"grpc_req,omitempty"`
	GrpcResp GrpcResp            `json:"grpc_resp" bson:"grpc_resp,omitempty"`
	Deps     []Dependency        `json:"deps" bson:"deps,omitempty"`
	AllKeys  map[string][]string `json:"all_keys" bson:"all_keys,omitempty"`
	Anchors  map[string][]string `json:"anchors" bson:"anchors,omitempty"`
	Noise    []string            `json:"noise" bson:"noise,omitempty"`
	Mocks    []*proto.Mock       `json:"mocks"`
	Type     string              `json:"type" bson:"type,omitempty"`
}

type TestCaseDB interface {
	Upsert(context.Context, TestCase) error
	UpdateTC(context.Context, TestCase) error
	Get(ctx context.Context, cid, id string) (TestCase, error)
	Delete(ctx context.Context, id string) error
	GetAll(ctx context.Context, cid, app, tcsType string, anchors bool, offset int, limit int) ([]TestCase, error)
	GetKeys(ctx context.Context, cid, app, uri, tcsType string) ([]TestCase, error)
	//Exists(context.Context, TestCase) (bool, error)
	DeleteByAnchor(ctx context.Context, cid, app, uri, tcsType string, filterKeys map[string][]string) error
	GetApps(ctx context.Context, cid string) ([]string, error)
}

// TestCaseReq is a struct for Http API request JSON body
type TestCaseReq struct {
	Captured     int64             `json:"captured" bson:"captured"`
	AppID        string            `json:"app_id" bson:"app_id"`
	URI          string            `json:"uri" bson:"uri"`
	HttpReq      HttpReq           `json:"http_req" bson:"http_req"`
	HttpResp     HttpResp          `json:"http_resp" bson:"http_resp"`
	GrpcReq      GrpcReq           `json:"grpc_req" bson:"grpc_req"`
	GrpcResp     GrpcResp          `json:"grpc_resp" bson:"grpc_resp"`
	Deps         []Dependency      `json:"deps" bson:"deps"`
	TestCasePath string            `json:"test_case_path" bson:"test_case_path"`
	MockPath     string            `json:"mock_path" bson:"mock_path"`
	Mocks        []*proto.Mock     `json:"mocks" bson:"mocks"`
	Type         Kind              `json:"type" bson:"type"`
	Remove       []string          `json:"remove" bson:"remove"`
	Replace      map[string]string `json:"replace" bson:"replace"`
}

func (req *TestCaseReq) Bind(r *http.Request) error {
	if req.Captured == 0 {
		return errors.New("captured timestamp cant be empty")
	}

	if req.AppID == "" {
		return errors.New("app id needs to be declared")
	}

	if strings.Contains(req.TestCasePath, "../") || strings.Contains(req.MockPath, "../") || strings.HasPrefix(req.TestCasePath, "/etc/passwd") || strings.HasPrefix(req.MockPath, "/etc/passwd") {
		return errors.New("file path should be absolute")
	}
	return nil
}

// TestReq is a struct for Http API request JSON body
type TestReq struct {
	ID           string   `json:"id" bson:"_id"`
	AppID        string   `json:"app_id" bson:"app_id"`
	RunID        string   `json:"run_id" bson:"run_id"`
	Resp         HttpResp `json:"resp" bson:"resp"`
	GrpcResp     GrpcResp `json:"grpc_resp" bson:"grpc_resp"`
	TestCasePath string   `json:"test_case_path" bson:"test_case_path"`
	MockPath     string   `json:"mock_path" bson:"mock_path"`
	Type         Kind     `json:"type" bson:"type"`
}

func (req *TestReq) Bind(r *http.Request) error {
	if req.ID == "" {
		return errors.New("id is required")
	}

	if req.AppID == "" {
		return errors.New("app id is required")
	}

	if strings.Contains(req.TestCasePath, "../") || strings.Contains(req.MockPath, "../") || strings.HasPrefix(req.TestCasePath, "/etc/passwd") || strings.HasPrefix(req.MockPath, "/etc/passwd") {
		return errors.New("file path should be absolute")
	}
	return nil
}
