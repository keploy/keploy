package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A gRPC call the ingress records is a test case whose origin names the
// proxy's clock, the clock the ingress stamped its request on before it wrote
// it on to the app, under the proven switch only.
func TestRecordIncoming_ATestCaseNamesItsWindowsClock(t *testing.T) {
	for _, on := range []bool{true, false} {
		t.Run(map[bool]string{true: "switch on", false: "switch off"}[on], func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[bool]string{true: "1", false: ""}[on])
			g := startGRPCIngress(t)
			g.call(t)
			var tc *models.TestCase
			select {
			case tc = <-g.tcCh:
			case <-time.After(30 * time.Second):
				t.Fatal("no test case came of the call")
			}
			if !on {
				if tc.Origin != nil {
					t.Fatalf("without the switch the test case got origin %+v", tc.Origin)
				}
				return
			}
			want := fakeconn.ClockID(fakeconn.ClockProxy)
			if tc.Origin == nil || tc.Origin.Clock == "" || tc.Origin.Clock != want || !strings.HasPrefix(want, "proxy:") {
				t.Fatalf("the test case's origin is %+v; want the proxy's clock %q", tc.Origin, want)
			}
		})
	}
}
