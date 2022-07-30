package regression

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.keploy.io/server/pkg/models"
)

type InfraDeps struct {
	ID       string                            `json:"id" bson:"_id"`
	Created  int64                             `json:"created" bson:"created"`
	Updated  int64                             `json:"updated" bson:"updated"`
	AppID    string                            `json:"app_id" bson:"app_id,omitempty"`
	TestName string                            `json:"test_name" bson:"test_name,omitempty"`
	Deps     []map[string]models.FetchResponse `json:"deps" bson:"deps,omitempty"`
}

func (req *InfraDeps) Bind(r *http.Request) error {
	if req.ID == "" {
		req.ID = uuid.New().String()
	}
	if req.Created == 0 {
		req.Created = time.Now().Unix()
	}
	if req.Updated == 0 {
		req.Updated = time.Now().Unix()
	}
	return nil
}

type TestCaseReq struct {
	Captured int64               `json:"captured" bson:"captured"`
	AppID    string              `json:"app_id" bson:"app_id"`
	URI      string              `json:"uri" bson:"uri"`
	HttpReq  models.HttpReq      `json:"http_req" bson:"http_req"`
	HttpResp models.HttpResp     `json:"http_resp" bson:"http_resp"`
	Deps     []models.Dependency `json:"deps" bson:"deps"`
}

func (req *TestCaseReq) Bind(r *http.Request) error {
	if req.Captured == 0 {
		return errors.New("captured timestamp cant be empty")
	}

	if req.AppID == "" {
		return errors.New("app id needs to be declared")
	}

	return nil
}

type TestReq struct {
	ID    string          `json:"id" bson:"_id"`
	AppID string          `json:"app_id" bson:"app_id"`
	RunID string          `json:"run_id" bson:"run_id"`
	Resp  models.HttpResp `json:"resp" bson:"resp"`
}

func (req *TestReq) Bind(r *http.Request) error {
	if req.ID == "" {
		return errors.New("id is required")
	}

	if req.AppID == "" {
		return errors.New("app id is required")
	}

	return nil
}
