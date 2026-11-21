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

func grpcTestCase(name, authority string, appPort uint16) *models.TestCase {
	return &models.TestCase{
		Kind:    models.GRPC_EXPORT,
		Name:    name,
		GrpcReq: models.GrpcReq{Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": authority}}},
		AppPort: appPort,
	}
}

func httpTestCase(name, rawURL string, appPort uint16) *models.TestCase {
	return &models.TestCase{
		Kind:    models.HTTP,
		Name:    name,
		HTTPReq: models.HTTPReq{URL: rawURL},
		AppPort: appPort,
	}
}

// A set whose tests are HTTP is gated on a round-trip against the address the
// first of them resolves to. A set whose tests are not HTTP (a gRPC set) has
// no round-trip to gate on, so its gate is the address its recorded authority
// names accepting a connection: without it the first test fires when --delay
// expires, ready or not, and the app answers "connection refused" as though
// the test had failed.
func TestTheProbeTargetOfASetIsItsDialledAddressWhateverItsTestsKind(t *testing.T) {
	const grpcPort = 50051
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(grpcPort))
	cfg := config.Test{Host: "127.0.0.1", GRPCPort: grpcPort}

	t.Run("grpc", func(t *testing.T) {
		cases := []*models.TestCase{grpcTestCase("TestGreeterSayHello", addr, grpcPort)}
		probe := probeTargetForSet(context.Background(), cfg, cases, "test-set-0", zap.NewNop(), nil, noPublishedPort)
		if !probe.ok {
			t.Fatalf("a gRPC set resolves no probe target, so its first test waits for nothing but --delay")
		}
		if !probe.tcpOnly {
			t.Errorf("the gate would wait for a serving path on a gRPC app, want the address's accept alone")
		}
		if probe.host != "127.0.0.1" || probe.port != strconv.Itoa(grpcPort) || probe.appPort != grpcPort {
			t.Errorf("gated on %s:%s (appPort=%d), want 127.0.0.1:%d", probe.host, probe.port, probe.appPort, grpcPort)
		}
	})

	t.Run("grpc with default config uses recorded authority port", func(t *testing.T) {
		cases := []*models.TestCase{grpcTestCase("TestDefaultCfg", "127.0.0.1:50052", 50052)}
		probe := probeTargetForSet(context.Background(), config.Test{}, cases, "test-set-0", zap.NewNop(), nil, noPublishedPort)
		if !probe.ok || !probe.tcpOnly {
			t.Fatalf("probe = %+v, want ok=true tcpOnly=true", probe)
		}
		if probe.host != "localhost" || probe.port != "50052" || probe.appPort != 50052 {
			t.Errorf("gated on %s:%s (appPort=%d), want localhost:50052 (appPort=50052)", probe.host, probe.port, probe.appPort)
		}
	})

	t.Run("grpc respects protocol.grpc.port override", func(t *testing.T) {
		cases := []*models.TestCase{grpcTestCase("TestProtoOverride", "127.0.0.1:50051", 50051)}
		protoCfg := config.Test{
			Host:     "127.0.0.1",
			GRPCPort: 50051,
			Protocol: map[string]config.ProtocolSettings{"grpc": {Port: 50099}},
		}
		probe := probeTargetForSet(context.Background(), protoCfg, cases, "test-set-0", zap.NewNop(), nil, noPublishedPort)
		if !probe.ok || !probe.tcpOnly || probe.port != "50099" {
			t.Fatalf("probe = %+v, want tcpOnly target on overridden port 50099", probe)
		}
	})

	t.Run("grpc with dynamic published port remapping", func(t *testing.T) {
		cases := []*models.TestCase{grpcTestCase("TestPublished", "127.0.0.1:50051", 50051)}
		pub := func(_ context.Context, _ string, appPort uint16) uint16 {
			if appPort == 50051 {
				return 32051
			}
			return 0
		}
		probe := probeTargetForSet(context.Background(), config.Test{Host: "127.0.0.1"}, cases, "test-set-0", zap.NewNop(), nil, pub)
		if !probe.ok || !probe.tcpOnly {
			t.Fatalf("probe = %+v, want ok=true tcpOnly=true", probe)
		}
		if probe.host != "127.0.0.1" || probe.port != "32051" || probe.appPort != 50051 {
			t.Errorf("gated on %s:%s (appPort=%d), want 127.0.0.1:32051 (appPort=50051)", probe.host, probe.port, probe.appPort)
		}
	})

	t.Run("http", func(t *testing.T) {
		cases := []*models.TestCase{httpTestCase("TestGet", "http://"+addr+"/health", grpcPort)}
		probe := probeTargetForSet(context.Background(), cfg, cases, "test-set-0", zap.NewNop(), nil, noPublishedPort)
		if !probe.ok {
			t.Fatalf("an HTTP set resolves no probe target")
		}
		if probe.tcpOnly {
			t.Errorf("an HTTP set is gated on its address's accept alone, want the serving-path round-trip")
		}
	})

	t.Run("unreachable gRPC target is skipped to avoid hanging waitForAccept", func(t *testing.T) {
		reach := &portReach{unreachable: map[uint16]bool{50051: true}, asked: map[string]int{}}
		cases := []*models.TestCase{
			grpcTestCase("TestUnpublished1", "127.0.0.1:50051", 50051),
			grpcTestCase("TestUnpublishedDup", "127.0.0.1:50051", 50051),
		}
		probe := probeTargetForSet(context.Background(), config.Test{Host: "127.0.0.1"}, cases, "test-set-0", zap.NewNop(), reach, noPublishedPort)
		if probe.ok {
			t.Fatalf("probe = %+v, want ok=false when every gRPC target is unreachable", probe)
		}
		if got := reach.asked["127.0.0.1:50051"]; got != 1 {
			t.Errorf("reach asked %d times for duplicate address, want 1", got)
		}
	})

	t.Run("unreachable first gRPC test falls through to reachable second gRPC test", func(t *testing.T) {
		reach := &portReach{unreachable: map[uint16]bool{50051: true}, asked: map[string]int{}}
		cases := []*models.TestCase{
			grpcTestCase("TestInternal", "127.0.0.1:50051", 50051),
			grpcTestCase("TestPublic", "127.0.0.1:50052", 50052),
		}
		probe := probeTargetForSet(context.Background(), config.Test{Host: "127.0.0.1"}, cases, "test-set-0", zap.NewNop(), reach, noPublishedPort)
		if !probe.ok || !probe.tcpOnly || probe.port != "50052" {
			t.Fatalf("probe = %+v, want reachable tcpOnly target on 50052", probe)
		}
	})

	t.Run("unreachable HTTP-only set returns ok=false", func(t *testing.T) {
		reach := &portReach{unreachable: map[uint16]bool{8080: true}, asked: map[string]int{}}
		cases := []*models.TestCase{httpTestCase("TestUnpublishedHTTP", "http://127.0.0.1:8080/health", 8080)}
		probe := probeTargetForSet(context.Background(), config.Test{Host: "127.0.0.1"}, cases, "test-set-0", zap.NewNop(), reach, noPublishedPort)
		if probe.ok {
			t.Fatalf("probe = %+v, want ok=false for unreachable HTTP-only set", probe)
		}
	})

	t.Run("mixed HTTP+gRPC prefers reachable HTTP", func(t *testing.T) {
		cases := []*models.TestCase{
			grpcTestCase("TestGRPC", "127.0.0.1:50051", 50051),
			httpTestCase("TestHTTP", "http://127.0.0.1:8080/health", 8080),
		}
		probe := probeTargetForSet(context.Background(), config.Test{Host: "127.0.0.1"}, cases, "test-set-0", zap.NewNop(), nil, noPublishedPort)
		if !probe.ok || probe.tcpOnly || probe.port != "8080" {
			t.Fatalf("probe = %+v, want HTTP target on 8080 with tcpOnly=false", probe)
		}
	})

	t.Run("mixed HTTP+gRPC falls back to reachable gRPC when HTTP is unreachable", func(t *testing.T) {
		reach := &portReach{unreachable: map[uint16]bool{8080: true}, asked: map[string]int{}}
		cases := []*models.TestCase{
			httpTestCase("TestUnpublishedHTTP", "http://127.0.0.1:8080/health", 8080),
			grpcTestCase("TestPublicGRPC", "127.0.0.1:50051", 50051),
		}
		probe := probeTargetForSet(context.Background(), config.Test{Host: "127.0.0.1"}, cases, "test-set-0", zap.NewNop(), reach, noPublishedPort)
		if !probe.ok || !probe.tcpOnly || probe.port != "50051" {
			t.Fatalf("probe = %+v, want fallback tcpOnly gRPC target on 50051", probe)
		}
	})

	t.Run("empty, nil, and missing-authority cases return ok=false", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			cases []*models.TestCase
		}{
			{"empty", nil},
			{"nil entry", []*models.TestCase{nil}},
			{"empty authority", []*models.TestCase{grpcTestCase("NoAuth", "", 50051)}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				if probe := probeTargetForSet(context.Background(), config.Test{}, tc.cases, "test-set-0", zap.NewNop(), nil, noPublishedPort); probe.ok {
					t.Fatalf("probe = %+v, want ok=false", probe)
				}
			})
		}
	})
}

// The wait before a gRPC set's first test ends when the app's address accepts a
// connection, not before: a listener that opens late is what the wait is for,
// and a run that fires into the closed port reports connection errors as test
// failures. Exercises Replayer.appReady end-to-end with KEPLOY_PROVEN off so
// the wiring through probeTargetForSet and gateOnAppAddress is pinned too.
func TestAGRPCSetWaitsForItsRecordedAddressToAccept(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "")
	port := testnet.SharedPort(t)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	cfg := &config.Config{}
	cfg.Test.Host = "127.0.0.1"
	cfg.Test.HealthPollTimeout = 60 * time.Second

	ctx, cancel := context.WithCancel(pkg.EndsWithApp(context.Background()))
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		<-done
	})

	replayer := &Replayer{logger: zap.NewNop(), config: cfg}
	cases := []*models.TestCase{grpcTestCase("TestGreeterSayHello", addr, uint16(port))}

	ready := make(chan bool, 1)
	go func() {
		defer close(done)
		ok, _ := replayer.appReady(ctx, cases, "test-set-0", true, false)
		ready <- ok
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
