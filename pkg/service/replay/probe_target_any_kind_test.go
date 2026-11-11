package replay

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// noPublishedPort is a published-port reader for a set nothing publishes: the
// address the test dials is the app's.
func noPublishedPort(context.Context, string, uint16) uint16 { return 0 }

// A set whose tests are HTTP is gated on a round-trip against the address the
// first of them resolves to. A set whose tests are not HTTP (a gRPC set) has
// no round-trip to gate on, so its gate is the address its recorded authority
// names accepting a connection: without it the first test fires when --delay
// expires, ready or not, and the app answers "connection refused" as though
// the test had failed.
func TestTheProbeTargetOfASetIsItsDialledAddressWhateverItsTestsKind(t *testing.T) {
	port := testnet.SharedPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	cfg := config.Test{Host: "127.0.0.1", GRPCPort: uint32(port)}

	t.Run("grpc", func(t *testing.T) {
		cases := []*models.TestCase{{
			Kind:    models.GRPC_EXPORT,
			Name:    "TestGreeterSayHello",
			GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": addr}}},
			AppPort: uint16(port),
		}}
		probe := probeTargetForSet(context.Background(), cfg, cases, "test-set-0", zap.NewNop(), nil, noPublishedPort)
		if !probe.ok {
			t.Fatalf("a gRPC set resolves no probe target, so its first test waits for nothing but --delay")
		}
		if !probe.tcpOnly {
			t.Errorf("the gate would wait for a serving path on a gRPC app, want the address's accept alone")
		}
		if probe.host != "127.0.0.1" || probe.port != strconv.Itoa(port) {
			t.Errorf("gated on %s:%s, want the address the test dials, 127.0.0.1:%d", probe.host, probe.port, port)
		}
	})

	t.Run("http", func(t *testing.T) {
		cases := []*models.TestCase{{
			Kind:    models.HTTP,
			Name:    "TestGet",
			HTTPReq: models.HTTPReq{URL: "http://" + addr + "/health"},
		}}
		probe := probeTargetForSet(context.Background(), cfg, cases, "test-set-0", zap.NewNop(), nil, noPublishedPort)
		if !probe.ok {
			t.Fatalf("an HTTP set resolves no probe target")
		}
		if probe.tcpOnly {
			t.Errorf("an HTTP set is gated on its address's accept alone, want the serving-path round-trip")
		}
	})
}

// The wait before a gRPC set's first test ends when the app's address accepts a
// connection, not before: a listener that opens late is what the wait is for,
// and a run that fires into the closed port reports connection errors as test
// failures.
func TestAGRPCSetWaitsForItsRecordedAddressToAccept(t *testing.T) {
	port := testnet.SharedPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	cfg := &config.Config{}
	cfg.Test.HealthPollTimeout = 60 * time.Second
	ctx := pkg.EndsWithApp(context.Background())

	ready := make(chan bool, 1)
	go func() {
		ready <- waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{
			host: "127.0.0.1", port: strconv.Itoa(port), ok: true, tcpOnly: true,
		})
	}()

	select {
	case <-ready:
		t.Fatal("the wait before the first test ended with nothing listening on the address the tests dial")
	case <-time.After(300 * time.Millisecond):
	}

	testnet.ListenShared(t, "tcp", addr)

	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the wait reported a stopped run once the address accepted a connection")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait did not end once the address accepted a connection")
	}
}
