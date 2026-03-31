package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/mockreplay"
	"go.keploy.io/server/v3/pkg/service/replay"
	sandboxsvc "go.keploy.io/server/v3/pkg/service/sandbox"
	"go.uber.org/zap"
)

type testServiceFactory struct {
	service interface{}
	err     error
}

func (t *testServiceFactory) GetService(_ context.Context, _ string) (interface{}, error) {
	return t.service, t.err
}

type testCmdConfigurator struct{}

func (t *testCmdConfigurator) AddFlags(_ *cobra.Command) error {
	return nil
}

func (t *testCmdConfigurator) ValidateFlags(_ context.Context, _ *cobra.Command) error {
	return nil
}

func (t *testCmdConfigurator) Validate(_ context.Context, _ *cobra.Command) error {
	return nil
}

type testRuntime struct{}

func (t *testRuntime) Logger() *zap.Logger {
	return zap.NewNop()
}

func (t *testRuntime) Config() *config.Config {
	return &config.Config{}
}

func (t *testRuntime) Instrumentation() replay.Instrumentation {
	return nil
}

func (t *testRuntime) MockDB() replay.MockDB {
	return nil
}

type testMockReplayService struct {
	result *models.ReplayResult
	err    error
	calls  int
}

func (t *testMockReplayService) Replay(_ context.Context, _ models.ReplayOptions) (*models.ReplayResult, error) {
	t.calls++
	return t.result, t.err
}

func (t *testMockReplayService) ListMockSets(_ context.Context) ([]string, error) {
	return nil, nil
}

type testSandboxService struct {
	syncErr     error
	uploadErr   error
	syncCalls   int
	uploadCalls int
}

func (t *testSandboxService) Upload(_ context.Context, _ string, _ string) error {
	t.uploadCalls++
	return t.uploadErr
}

func (t *testSandboxService) Sync(_ context.Context, _ string, _ string) error {
	t.syncCalls++
	return t.syncErr
}

func setupReplayDeps(t *testing.T, sbSvc sandboxsvc.Service, replaySvc mockreplay.Service) {
	t.Helper()

	oldToken := getSandboxJWTTokenFunc
	oldSandboxSvc := newSandboxServiceFunc
	oldReplaySvc := newMockReplayServiceFunc

	getSandboxJWTTokenFunc = func(_ context.Context, _ *zap.Logger, _ *config.Config) (string, error) {
		return "token", nil
	}
	newSandboxServiceFunc = func(_ string, _ string, _ *zap.Logger) sandboxsvc.Service {
		return sbSvc
	}
	newMockReplayServiceFunc = func(_ *zap.Logger, _ *config.Config, _ mockreplay.Runtime) mockreplay.Service {
		return replaySvc
	}

	t.Cleanup(func() {
		getSandboxJWTTokenFunc = oldToken
		newSandboxServiceFunc = oldSandboxSvc
		newMockReplayServiceFunc = oldReplaySvc
	})
}

func setupReplayCommand(t *testing.T, cfg *config.Config) *cobra.Command {
	t.Helper()

	cmd := SandboxReplay(context.Background(), zap.NewNop(), cfg, &testServiceFactory{service: &testRuntime{}}, &testCmdConfigurator{})
	cmd.Flags().Bool("local", false, "")
	cmd.Flags().String("name", "main_test", "")
	return cmd
}

func TestSandboxReplayMissingManifestUploadsAfterSuccess(t *testing.T) {
	sandboxService := &testSandboxService{
		syncErr: fmt.Errorf("%w for ref %q", sandboxsvc.ErrManifestNotFound, "owner/service:v1.2.3"),
	}
	replayService := &testMockReplayService{
		result: &models.ReplayResult{Success: true},
	}
	setupReplayDeps(t, sandboxService, replayService)

	cfg := &config.Config{
		Command:      "go test -v",
		Path:         ".",
		APIServerURL: "http://api.server",
		Sandbox: config.Sandbox{
			Ref: "owner/service:v1.2.3",
		},
	}
	cmd := setupReplayCommand(t, cfg)
	err := cmd.RunE(cmd, nil)
	if err != nil {
		t.Fatalf("expected replay success, got error: %v", err)
	}
	if sandboxService.syncCalls != 1 {
		t.Fatalf("expected 1 sync call, got %d", sandboxService.syncCalls)
	}
	if replayService.calls != 1 {
		t.Fatalf("expected 1 replay call, got %d", replayService.calls)
	}
	if sandboxService.uploadCalls != 1 {
		t.Fatalf("expected 1 upload call, got %d", sandboxService.uploadCalls)
	}
}

func TestSandboxReplayMissingManifestUploadFailureFailsCommand(t *testing.T) {
	sandboxService := &testSandboxService{
		syncErr:   fmt.Errorf("%w for ref %q", sandboxsvc.ErrManifestNotFound, "owner/service:v1.2.3"),
		uploadErr: errors.New("upload failed"),
	}
	replayService := &testMockReplayService{
		result: &models.ReplayResult{Success: true},
	}
	setupReplayDeps(t, sandboxService, replayService)

	cfg := &config.Config{
		Command:      "go test -v",
		Path:         ".",
		APIServerURL: "http://api.server",
		Sandbox: config.Sandbox{
			Ref: "owner/service:v1.2.3",
		},
	}
	cmd := setupReplayCommand(t, cfg)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected replay command to fail on upload error")
	}
	if !strings.Contains(err.Error(), "sandbox upload after replay failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if replayService.calls != 1 {
		t.Fatalf("expected 1 replay call, got %d", replayService.calls)
	}
	if sandboxService.uploadCalls != 1 {
		t.Fatalf("expected 1 upload call, got %d", sandboxService.uploadCalls)
	}
}

func TestSandboxReplayNonManifestSyncErrorFailsFast(t *testing.T) {
	sandboxService := &testSandboxService{
		syncErr: errors.New("network error"),
	}
	replayService := &testMockReplayService{
		result: &models.ReplayResult{Success: true},
	}
	setupReplayDeps(t, sandboxService, replayService)

	cfg := &config.Config{
		Command:      "go test -v",
		Path:         ".",
		APIServerURL: "http://api.server",
		Sandbox: config.Sandbox{
			Ref: "owner/service:v1.2.3",
		},
	}
	cmd := setupReplayCommand(t, cfg)
	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected replay command to fail on sync error")
	}
	if !strings.Contains(err.Error(), "sandbox sync failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	if replayService.calls != 0 {
		t.Fatalf("expected replay not to run, got %d calls", replayService.calls)
	}
	if sandboxService.uploadCalls != 0 {
		t.Fatalf("expected upload not to run, got %d calls", sandboxService.uploadCalls)
	}
}
