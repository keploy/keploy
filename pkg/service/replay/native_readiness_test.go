package replay

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
)

func grpcTestCase(authority string) *models.TestCase {
	return &models.TestCase{Kind: models.GRPC_EXPORT, GrpcReq: models.GrpcReq{
		Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":authority": authority}}}}
}

func httpTestCase(url string) *models.TestCase {
	tc := &models.TestCase{Kind: models.HTTP}
	tc.HTTPReq.URL = url
	return tc
}

func TestReadinessProbeTarget(t *testing.T) {
	for _, tc := range []struct {
		name        string
		commandType utils.CmdType
		testCases   []*models.TestCase
		wantOK      bool
		wantAddr    string
		wantHTTP    bool
	}{
		{"a native gRPC set is gated on the address its first test dials", utils.Native, []*models.TestCase{nil, grpcTestCase(""), grpcTestCase("localhost:50051")}, true, "localhost:50051", false},
		{"a native set with an HTTP test keeps its HTTP target", utils.Native, []*models.TestCase{grpcTestCase("localhost:50051"), httpTestCase("http://localhost:8080/health")}, true, "localhost:8080", true},
		{"a native set with nothing to dial is not gated", utils.Native, []*models.TestCase{grpcTestCase("")}, false, ":", false},
		{"a docker gRPC set keeps the docker gates", utils.DockerRun, []*models.TestCase{grpcTestCase("localhost:50051")}, false, ":", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Replayer{logger: zap.NewNop(), config: &config.Config{CommandType: string(tc.commandType)}}
			got := r.readinessProbeTarget(context.Background(), tc.testCases, "test-set-0")
			require.Equal(t, tc.wantOK, got.ok)
			require.Equal(t, tc.wantAddr, net.JoinHostPort(got.host, got.port))
			require.Equal(t, tc.wantHTTP, got.servesHTTP())
		})
	}
}

func TestWaitForAppReady_NativeGRPCAppWaitsForItsPortToListen(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)

	listenAfter := 1500 * time.Millisecond
	listening := make(chan net.Listener, 1)
	time.AfterFunc(listenAfter, func() {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			listening <- nil
			return
		}
		listening <- l
	})

	cfg := gateCfg(10 * time.Second)
	cfg.Command = "./grpc-server"
	cfg.Test.Delay = 0

	start := time.Now()
	ready := waitForAppReady(context.Background(), zap.NewNop(), cfg, httpProbeTarget{host: host, port: port, ok: true})
	elapsed := time.Since(start)

	l := <-listening
	require.NotNil(t, l)
	t.Cleanup(func() { _ = l.Close() })
	require.True(t, ready)
	require.GreaterOrEqual(t, elapsed, listenAfter)
	require.Less(t, elapsed, 5*time.Second)
}

func TestWaitForAppReady_NativeGRPCAppIsReadyOnAccept(t *testing.T) {
	host, port := acceptOnlyListener(t)
	cfg := gateCfg(10 * time.Second)
	cfg.Command = "./grpc-server"
	cfg.Test.Delay = 0

	start := time.Now()
	require.True(t, waitForAppReady(context.Background(), zap.NewNop(), cfg, httpProbeTarget{host: host, port: port, ok: true}))
	require.Less(t, time.Since(start), time.Second)
}

func TestWaitForAppReady_NativeGRPCAppStopsWaitingWhenTheRunEnds(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	host, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, ln.Close())

	cfg := gateCfg(time.Minute)
	cfg.Command = "./grpc-server"
	cfg.Test.Delay = 0

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.False(t, waitForAppReady(ctx, zap.NewNop(), cfg, httpProbeTarget{host: host, port: port, ok: true}))
	require.Less(t, time.Since(start), 5*time.Second)
}
