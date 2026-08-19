package contract

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestHTTPDocToOpenAPIAllowsTopLevelArrayResponse_Repro(t *testing.T) {
	svc := &contract{}

	openapi, err := svc.HTTPDocToOpenAPI(zap.NewNop(), models.HTTPDoc{
		Version: "api.keploy.io/v1beta1",
		Kind:    "Http",
		Name:    "array-response",
		Spec: models.HTTPSchema{
			Request: models.HTTPReq{
				Method: "GET",
				URL:    "http://example.com/users",
			},
			Response: models.HTTPResp{
				StatusCode:    200,
				StatusMessage: "OK",
				Body:          `[{"id":1,"name":"Alice"}]`,
			},
		},
	})

	if err != nil {
		t.Fatalf("top-level JSON array response should be valid OpenAPI input, got error: %v", err)
	}

	schema := openapi.Paths["/users"].Get.Responses["200"].Content["application/json"].Schema
	if schema.Type != "array" {
		t.Fatalf("expected response schema type array, got %q", schema.Type)
	}
	if schema.Items == nil || schema.Items.Type != "object" {
		t.Fatalf("expected response schema items to be object, got %#v", schema.Items)
	}
	if got := schema.Items.Properties["id"]["type"]; got != "number" {
		t.Fatalf("expected response item id field to be number, got %v", got)
	}
}
