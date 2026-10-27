package replay

import (
	"context"
	"errors"
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

func TestAppNotReadyStatus(t *testing.T) {
	exited := models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 3"), ExitCode: 3}
	failed := models.AppError{AppErrorType: models.ErrCommandError, Err: errors.New("exec: \"./grpc-server\": file not found")}
	for _, tc := range []struct {
		name       string
		appErr     models.AppError
		wantStatus models.TestSetStatus
		wantErr    error
	}{
		{"no app error is the user's abort", models.AppError{}, models.TestSetStatusUserAbort, context.Canceled},
		{"a cancelled app is the user's abort", models.AppError{AppErrorType: models.ErrCtxCanceled, Err: context.Canceled}, models.TestSetStatusUserAbort, context.Canceled},
		{"an app that exited halts the set with its exit", exited, models.TestSetStatusAppHalted, exited},
		{"an app that failed to start is the app's fault", failed, models.TestSetStatusFaultUserApp, failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, err := appNotReadyStatus(tc.appErr)
			require.Equal(t, tc.wantStatus, status)
			require.Equal(t, tc.wantErr, err)
		})
	}
}

type exitingApp struct{ *prInstr }

func (exitingApp) Run(context.Context, models.RunOptions) models.AppError {
	return models.AppError{AppErrorType: models.ErrUnExpected, Err: errors.New("exit status 3"), ExitCode: 3}
}

func TestRunTestSet_AnAppThatExitsBeforeItIsReadyFailsTheSetWithItsExit(t *testing.T) {
	provenOn(t, false)
	h := newPartialRunHarness(t, 2, 0)
	h.replayer.instrumentation = exitingApp{h.instr}
	h.replayer.config.Test.Delay = 30

	ctx, cancel := context.WithTimeout(context.Background(), prTimeout)
	defer cancel()
	start := time.Now()
	status, err := h.replayer.RunTestSet(ctx, "test-set-0", "test-run-0", false)

	require.Less(t, time.Since(start), 10*time.Second)
	require.Equal(t, models.TestSetStatusAppHalted, status)
	require.ErrorContains(t, err, "exit status 3")
}
