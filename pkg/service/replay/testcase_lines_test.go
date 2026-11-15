package replay

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy test's line for a test's request whose host it replaces names the
// request by its shape and the test case, which the report keeps the request
// whole under: the shape alone names no call a reader can find.
func TestReplacingATestsHostNamesTheTestCase(t *testing.T) {
	const s = logleak.Canary
	logger, logs := logleak.Observe()
	r := &Replayer{logger: logger}
	for _, tc := range []*models.TestCase{
		{Name: "test-3", Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://recorded.example:8080/v1/reset/" + s}},
		{Name: "test-4", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": "recorded.example:9090"}}}},
	} {
		if err := r.replaceHostInTestCase(tc, "app", "the app's host"); err != nil {
			t.Fatalf("%s: %v", tc.Name, err)
		}
	}
	said := logs.All()
	if len(said) != 2 || said[0].ContextMap()["testcase"] != "test-3" || said[1].ContextMap()["testcase"] != "test-4" {
		t.Fatalf("the lines are %+v, want one per test, each naming its test case", said)
	}
	logleak.Check(t, logs, s, "replaceHostInTestCase")
}

// A call keploy record's full round charges a test with (concurrent_shares)
// is tagged in the run's session (models.LogKeys of its context), the key
// its lines and report name it by, never the process's: a program that
// embeds the replayer runs many runs.
func TestASharedCallsChargeIsTaggedInTheRunsSession(t *testing.T) {
	keys := models.NewLogKeySession()
	s := &settlement{keys: keys, sp: &sharePlan{}, lookup: map[string]mockDisplayInfo{"mock-1": {protocol: "HTTP", summary: "POST /v1/orders/42"}}}
	c := s.call("mock-1", models.MissConcurrentAmbiguous, nil, "its attempts were answered by different recordings")
	if c.PathTag != keys.PathTag("POST", "/v1/orders/42") || c.PathTag == models.PathTag(context.Background(), "POST", "/v1/orders/42") {
		t.Fatalf("the charge carries tag %q, want POST /v1/orders/42's in the run's session, %s", c.PathTag, keys.PathTag("POST", "/v1/orders/42"))
	}
}
