package mock

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestReplayedCaseIsComparedWithEndpointNoise(t *testing.T) {
	cfg := &config.Config{}
	cfg.Test.GlobalNoise.Endpoints = []config.EndpointNoise{{Method: "GET", Path: "/version", Body: config.Noise{"version": {}}}}
	m := &mockService{config: cfg, logger: zap.NewNop()}
	tc := &models.TestCase{
		HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://api/version"},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"version":"a"}`},
	}
	if pass, _ := m.compareCase(tc, &models.HTTPResp{StatusCode: 200, Body: `{"version":"b"}`}); !pass {
		t.Fatal("the endpoint rule was not applied")
	}
	tc.HTTPReq.URL = "http://api/charts"
	if pass, _ := m.compareCase(tc, &models.HTTPResp{StatusCode: 200, Body: `{"version":"b"}`}); pass {
		t.Fatal("another endpoint's version was ignored")
	}
}
