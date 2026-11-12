//go:build linux

package http

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// Through the relay on real sockets, under the proven switch: every call the
// proxy records names the relay's clock (models.Origin.Clock), the proxy's,
// which stamped its request as the relay read it. So does one that ended in
// a fault, with no answer or after the head of one, which is recorded with
// the fault itself.
func TestRecordV2_ThroughTheRelayACallNamesTheProxysClock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	for _, c := range []struct {
		name   string
		answer func(t *testing.T, c *relayedConn)
		fault  *models.Fault
	}{
		{"a whole answer", func(t *testing.T, c *relayedConn) {
			write(t, c.dest, c.dst, string(canonicalResponse))
			relayed(t, c.app, string(canonicalResponse))
		}, nil},
		{"no answer: the upstream closed", func(t *testing.T, c *relayedConn) {
			_ = c.dest.Close()
			_, _ = appRead(t, c.app)
		}, &models.Fault{Kind: models.FaultEOF}},
		{"the upstream reset after the head", func(t *testing.T, c *relayedConn) {
			write(t, c.dest, c.dst, head+"01234")
			relayed(t, c.app, head+"01234")
			resetConn(t, c.dest)
			<-c.relayDone
			_ = c.src.Close()
			_, _ = appRead(t, c.app)
		}, &models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 5}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rc := newRelayedConn(t)
			rc.start(t)
			write(t, rc.app, rc.src, faultReq)
			relayed(t, rc.dest, faultReq)
			c.answer(t, rc)
			mocks, err := rc.finish(t)
			m := oneMock(t, mocks, err)
			if c.fault != nil {
				wantFault(t, m, *c.fault)
			} else if m.Spec.Fault != nil {
				t.Fatalf("a whole answer recorded with fault %s", m.Spec.Fault)
			}
			want := fakeconn.ClockID(fakeconn.ClockProxy)
			if want == "" {
				t.Fatal("precondition: the relay named no proxy clock")
			}
			if m.Origin == nil || m.Origin.Clock != want {
				t.Fatalf("the recorded call's origin is %+v; want it on the relay's clock %q", m.Origin, want)
			}
		})
	}
}
