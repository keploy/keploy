package replay

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestReplayAppliesEndpointNoiseOnlyToItsEndpoint(t *testing.T) {
	cfg := &config.Config{}
	cfg.Test.GlobalNoise.Global = config.GlobalNoise{"body": {"at": {}}}
	cfg.Test.GlobalNoise.Endpoints = []config.EndpointNoise{{Method: "GET", Path: "/version", Body: config.Noise{"version": {}}}}
	r := &Replayer{logger: zap.NewNop(), config: cfg}
	check := func(url, recorded, live string) bool {
		tc := &models.TestCase{
			Name:     url,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: url},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: recorded},
		}
		viaReplay, _ := r.compareHTTPRespForReplay(tc, &models.HTTPResp{StatusCode: 200, Body: live}, "set", false)
		direct, _ := r.compareHTTPResp(tc, &models.HTTPResp{StatusCode: 200, Body: live}, "set", false)
		if viaReplay != direct {
			t.Fatalf("%s: the two compares disagree (%v, %v)", url, viaReplay, direct)
		}
		return direct
	}
	if !check("http://api:8080/version", `{"version":"a1b2c3d4","at":1}`, `{"version":"pr-42","at":2}`) {
		t.Fatal("GET /version failed on its noised field")
	}
	if check("http://api:8080/charts/details", `{"version":"3.8.18","at":1}`, `{"version":"3.8.19","at":2}`) {
		t.Fatal("another endpoint's version was ignored")
	}
	if !check("http://api:8080/charts/details", `{"version":"3.8.18","at":1}`, `{"version":"3.8.18","at":2}`) {
		t.Fatal("global noise stopped applying")
	}
	if got := r.httpNoiseConfig("set", nil); len(got["body"]) != 1 {
		t.Fatalf("noise without a test case holds endpoint fields: %v", got)
	}
}
