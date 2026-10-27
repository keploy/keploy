package http

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/client/app"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// The run of an app the set's end had to kill says why, through the agent
// client's Run, once the app's runner has returned: the replayer reads it
// there (models.AppError.StopKilled).
func TestRunSaysWhyTheSetEndHadToKillTheApp(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Agent.AgentURI = "http://127.0.0.1:1/agent"
	client := New(zap.NewNop(), nil, cfg)
	client.apps.Store(uint64(0), app.NewApp(zap.NewNop(),
		"cd "+dir+` && trap "" INT; touch started; exec sleep 600`, nil,
		models.SetupOptions{CommandType: string(utils.Native)}))

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	done := make(chan models.AppError, 1)
	go func() { done <- client.Run(ctx, models.RunOptions{}) }()
	for {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		select {
		case appErr := <-done:
			t.Fatalf("the app ended before it started: %+v", appErr)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel(models.ErrAppStoppedForSetEnd)
	if appErr := <-done; appErr.AppErrorType != models.ErrCtxCanceled || !strings.Contains(appErr.StopKilled, "so keploy killed it") {
		t.Fatalf("Run ended %+v; want the run's end, saying the set's end killed the app", appErr)
	}
}
