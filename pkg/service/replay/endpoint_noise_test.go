package replay

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

func TestStartSaysAnEndpointRuleThatIgnoresNothing(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	cfg := &config.Config{}
	cfg.Test.GlobalNoise.Endpoints = []config.EndpointNoise{{Path: "/version?x=1", Body: config.Noise{"version": {}}}}
	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })
	r := NewReplayer(zap.New(core), startTestDB{list: func(context.Context) ([]string, error) { return nil, nil }}, nil, nil, nil, nil, startTelemetry{}, startInstrumentation{}, nil, cfg)
	_ = r.Start(context.Background())
	if logs.FilterMessage("an endpoint noise rule ignores nothing").Len() != 1 {
		t.Fatal("keploy test did not say the rule with a query in its path ignores nothing")
	}
}
