package testdb

import (
	"io"
	"os"
	"strings"
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

// A JSON value Flatten cannot read is said on stdout by its kind, not the
// value, which is the application's, unless KEPLOY_LOG_CONTENT is set.
func TestFlatten_AValueItCannotReadIsSaidByItsKind(t *testing.T) {
	const canary = "kp-canary-testdb-flatten-3c1a"
	logtest.Each(t, func(t *testing.T, on bool) {
		out := stdoutOf(t, func() { Flatten(map[string]interface{}{"note": struct{ Body string }{canary}}) })
		if !strings.Contains(out, "found invalid value in json") || !strings.Contains(out, "struct") {
			t.Fatalf("Flatten said %q, want the value's kind", out)
		}
		if strings.Contains(out, canary) != on {
			t.Fatalf("with the switch on=%v, Flatten said %q", on, out)
		}
	})
}

// stdoutOf is what f writes to os.Stdout.
func stdoutOf(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	os.Stdout = prev
	_ = w.Close()
	return <-done
}
