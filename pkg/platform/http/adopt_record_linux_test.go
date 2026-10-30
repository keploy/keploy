//go:build linux

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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// keploy record's stop of the app, under the proven switch, says when the
// stop of what the app left behind had to kill one that made no progress on
// its way out: what it would have done as it stopped is not in the recording
// (P101-b-k). A replay's teardown says nothing.
func TestARecordingsStopSaysItKilledWhatTheAppLeftStopping(t *testing.T) {
	if !inOwnProcess(t) {
		return
	}
	t.Setenv(provenswitch.Env, "1")
	stop, err := startAdopting(zap.NewNop())
	if err != nil {
		t.Fatalf("startAdopting: %v", err)
	}
	defer stop()
	stopCheck, stopStallChecks = 20*time.Millisecond, 10
	for _, tc := range []struct {
		mode   models.Mode
		killed bool
	}{
		{models.MODE_RECORD, true},
		{models.MODE_TEST, false},
	} {
		dir := t.TempDir()
		ready := filepath.Join(dir, "ready")
		cfg := &config.Config{}
		cfg.Agent.AgentURI = "http://127.0.0.1:1/agent"
		client := New(zap.NewNop(), nil, cfg)
		client.apps.Store(uint64(0), app.NewApp(zap.NewNop(),
			deafApp(ready), nil,
			models.SetupOptions{CommandType: string(utils.Native), Mode: tc.mode}))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan models.AppError, 1)
		go func() { done <- client.Run(ctx, models.RunOptions{}) }()
		await(t, string(tc.mode)+": the app ready", func() bool { _, err := os.Stat(ready); return err == nil })
		cancel()
		appErr := <-done
		said := strings.Contains(appErr.StopKilled, "keploy stopped the application at the recording's end, and") &&
			strings.Contains(appErr.StopKilled, "made no progress on its way out")
		if said != tc.killed || (!tc.killed && appErr.StopKilled != "") {
			t.Fatalf("%s: run ended %+v; want the recording's kill said %v", tc.mode, appErr, tc.killed)
		}
	}
}
