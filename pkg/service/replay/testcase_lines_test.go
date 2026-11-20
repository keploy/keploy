package replay

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A reset test request's lines: the log's names the replay target's host by
// its labels' shape beside its tag in the run's session, never whole; the
// console's names it whole beside the same tag, so the user finds the log's
// line by it. An IP address is written whole in both, with no tag.
func TestTheResetWaitNamesTheTargetByItsShapeInTheLogAndWholeOnTheConsole(t *testing.T) {
	const s = logleak.Canary
	ctx := models.StartLogKeySession(context.Background())
	addr := "app-" + s + ".internal:8080"
	logLine, consoleLine := resetWaitLines(ctx, addr)
	tag := models.HostTag(ctx, addr)
	if tag == "" || strings.Contains(logLine, s) || !strings.Contains(logLine, "{}.{}:8080 "+tag) {
		t.Errorf("the log's line does not name the target by its shape and tag %q alone: %q", tag, logLine)
	}
	if !strings.Contains(consoleLine, addr+" "+models.HostTagLabel(tag)) {
		t.Errorf("the console's line does not name the target whole beside its tag: %q", consoleLine)
	}
	logLine, consoleLine = resetWaitLines(ctx, "10.0.0.7:8080")
	if !strings.Contains(logLine, "reset at 10.0.0.7:8080 before") || !strings.Contains(consoleLine, "reset at 10.0.0.7:8080 before") {
		t.Errorf("an IP address is not written whole: %q / %q", logLine, consoleLine)
	}
}

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

// A gRPC test's new :authority is named by its host's shape, as the HTTP
// line names the new URL's host: a host name is traffic (models.HostShape),
// and a program that embeds the replay (k8s-proxy, which replaces it with
// the replay Pod's address or Service name) writes the line into its log.
func TestReplacingAGRPCTestsHostNamesTheHostByItsShape(t *testing.T) {
	const s = logleak.Canary
	logger, logs := logleak.Observe()
	r := &Replayer{logger: logger}
	tc := &models.TestCase{Name: "test-5", Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": "recorded.example:9090"}}}}
	if err := r.replaceHostInTestCase(tc, "app-"+s+".svc", "the app's host"); err != nil {
		t.Fatal(err)
	}
	if got := tc.GrpcReq.Headers.PseudoHeaders[":authority"]; got != "app-"+s+".svc:9090" {
		t.Fatalf("precondition: the :authority is %q after the host was replaced", got)
	}
	if logs.FilterField(zap.String("replaced the app's host", "{}.{}:9090")).Len() != 1 {
		t.Fatalf("the lines are %+v, want one naming the new host by its shape", logs.All())
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
