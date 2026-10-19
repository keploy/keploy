package testdb

import (
	"testing"

	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// A gRPC test case written is logged with its headers' names, not their
// values, unless KEPLOY_LOG_CONTENT is set.
func TestEncodeTestcase_AGRPCCasesHeadersAreLoggedByName(t *testing.T) {
	const canary = "kp-canary-testdb-grpc-1d8e"
	logtest.Each(t, func(t *testing.T, on bool) {
		tc := grpcTestCase()
		tc.GrpcReq.Headers.OrdinaryHeaders = map[string]string{"authorization": "Bearer " + canary}
		tc.GrpcResp.Headers.OrdinaryHeaders = map[string]string{"x-note": canary}
		logger, logs := logtest.Observe()
		if _, err := EncodeTestcase(tc, logger); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "gRPC schema created")
	})
}
