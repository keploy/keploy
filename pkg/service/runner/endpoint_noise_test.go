package integration

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type ruleInstr struct{ stepInstr }

func (*ruleInstr) Setup(context.Context, string, models.SetupOptions) error { return nil }

func TestTheRunnerAppliesAnEndpointRuleOnlyToItsEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"version":"b","name":"app"}`)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{}
	cfg.Test.GlobalNoise.Endpoints = []config.EndpointNoise{{Method: "GET", Path: "/version", Body: config.Noise{"version": {}}}}

	at := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		path string
		pass bool
	}{
		{"/version", true},
		{"/other", false},
	} {
		t.Run(c.path, func(t *testing.T) {
			tc := &models.TestCase{
				Version: models.GetVersion(),
				Kind:    models.HTTP,
				Name:    "test-1",
				HTTPReq: models.HTTPReq{
					Method:     models.Method(http.MethodGet),
					ProtoMajor: 1,
					ProtoMinor: 1,
					URL:        "http://localhost:8080" + c.path,
					Timestamp:  at,
				},
				HTTPResp: models.HTTPResp{
					StatusCode: 200,
					Header:     map[string]string{"Content-Type": "application/json", "Content-Length": "28", "Date": "Wed, 25 Nov 2026 10:00:00 GMT"},
					Body:       `{"version":"a","name":"app"}`,
					Timestamp:  at.Add(time.Millisecond),
				},
			}
			r := New(zap.NewNop(), storedCaseDB{tc: tc}, &ruleInstr{}, &skippingMockDB{},
				&stubMappingDB{mappings: map[string][]models.MockEntry{}}, cfg, models.GlobalNoise{"header": {"date": {}}})
			res := r.RunTest(context.Background(), RunTestOpts{TestSetID: "test-set-0", TestStepID: "test-1", ServiceURL: srv.URL})
			if res.Error != "" {
				t.Fatalf("RunTest: %s", res.Error)
			}
			if res.Passed != c.pass {
				t.Fatalf("GET %s passed %v; want %v (diff %s)", c.path, res.Passed, c.pass, res.Diff)
			}
			if !c.pass && !strings.Contains(res.Diff, `"body_result":{"normal":false`) {
				t.Fatalf("GET %s failed on something other than its body: %s", c.path, res.Diff)
			}
		})
	}
}

func TestTheRunnerSaysAnEndpointRuleThatIgnoresNothing(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	cfg := &config.Config{}
	cfg.Test.GlobalNoise.Endpoints = []config.EndpointNoise{{Path: "/version"}}
	New(zap.New(core), storedCaseDB{}, &ruleInstr{}, &skippingMockDB{}, &stubMappingDB{}, cfg, nil)
	if logs.FilterMessage("an endpoint noise rule ignores nothing").Len() != 1 {
		t.Fatal("the runner did not say the rule with no fields ignores nothing")
	}
}
