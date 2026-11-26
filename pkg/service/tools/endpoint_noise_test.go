package tools

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestTemplatizeLeavesAUseAnEndpointRuleCovers(t *testing.T) {
	cfg := &config.Config{}
	cfg.Test.IgnoreOrdering = true
	cfg.Test.GlobalNoise.Endpoints = []config.EndpointNoise{{Method: "GET", Path: "/orders", Body: config.Noise{"items.id": {}}}}
	tl := &Tools{logger: zap.NewNop(), config: cfg}
	tcs := []*models.TestCase{
		{Name: "get-orders", HTTPReq: models.HTTPReq{Method: "GET", URL: "http://api/orders"}},
		{Name: "get-carts", HTTPReq: models.HTTPReq{Method: "GET", URL: "http://api/carts"}},
	}
	untied := tl.untiedInArray("set-1", tcs)
	if why := untied(&location{tc: 0, part: ResponseBody, inArray: true, keys: []string{"items", "id"}}); !strings.Contains(why, "globalNoise.endpoints") {
		t.Fatalf("a use the rule covers would be written, and keploy test would refuse it: %q", why)
	}
	if why := untied(&location{tc: 1, part: ResponseBody, inArray: true, keys: []string{"items", "id"}}); why != "" {
		t.Fatalf("another endpoint's use is left as recorded: %q", why)
	}
}
