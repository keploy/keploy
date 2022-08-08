package mocks

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.keploy.io/server/pkg/models"
)

type TestMocksReq struct {
	ID       string                            `json:"id" bson:"_id"`
	Created  int64                             `json:"created" bson:"created"`
	Updated  int64                             `json:"updated" bson:"updated"`
	AppID    string                            `json:"app_id" bson:"app_id,omitempty"`
	TestName string                            `json:"test_name" bson:"test_name,omitempty"`
	Deps     []map[string]models.FetchResponse `json:"deps" bson:"deps,omitempty"`
}

func (req *TestMocksReq) Bind(r *http.Request) error {
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
