package models

import "context"

type TestMock struct {
	ID       string                     `json:"id" bson:"_id"`
	Created  int64                      `json:"created" bson:"created,omitempty"`
	Updated  int64                      `json:"updated" bson:"updated,omitempty"`
	AppID    string                     `json:"app_id" bson:"app_id,omitempty"`
	TestName string                     `json:"test_name" bson:"test_name,omitempty"`
	Deps     []map[string]FetchResponse `json:"deps" bson:"deps,omitempty"`
}

type FetchResponse struct {
	Status       int               `json:"status" bson:"status,omitempty"`
	Headers      map[string]string `json:"headers" bson:"headers,omitempty"`
	Body         interface{}       `json:"body" bson:"body,omitempty"`
	ResponseType string            `json:"response_type" bson;"response_type"`
}

type TestMockDB interface {
	Insert(context.Context, TestMock) error
	Get(ctx context.Context, app string, testName string) ([]TestMock, error)
	CountDocs(ctx context.Context, app string, testName string) (int64, error)
	UpdateArr(ctx context.Context, app string, testName string, doc TestMock) error
}
