package regression

import (
	"errors"
	"net/http"

	"go.keploy.io/server/pkg/models"
)

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
