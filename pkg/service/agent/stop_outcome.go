package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	kdocker "go.keploy.io/server/v3/pkg/platform/docker"
	"go.uber.org/zap"
)

// stopOutcomeTimeout bounds the write leaveStopOutcome makes as the agent is
// stopped. It runs on the signal goroutine AHEAD of the agent's shutdown
// (utils.RegisterPreCancelHook), so a proxy that never answered would hold
// that shutdown until compose kills the container -- which loses the account
// anyway, and the agent's own teardown with it. Well inside the 10s compose
// gives a service to stop before it kills it.
const stopOutcomeTimeout = 3 * time.Second

// stopOutcomePath is where this agent leaves its replay's outcome as it is
// stopped, or "" where it leaves none.
//
// Only a containerised agent serving a mock replay leaves one. Under docker
// compose the agent is a service in the project, and compose stops every
// service the moment the app exits -- before the CLI can ask the agent what it
// served and missed, which is the whole of what a replay proves. So the agent
// says it on its way out, into its own container, where the CLI reads it back
// before its teardown removes the container (pkg/client/app). A native agent
// is asked over HTTP before it is stopped, and must not write into the host's
// /tmp.
func stopOutcomePath(opts models.SetupOptions) string {
	if opts.IsDocker && opts.MockMode && opts.Mode == models.MODE_TEST {
		return kdocker.AgentOutcomeFile
	}
	return ""
}

// leaveStopOutcome writes the replay's outcome to path, giving up after
// stopOutcomeTimeout. A failure is only logged: the CLI that finds no file
// reports the outcome as unknown, which is the truth.
func (a *Agent) leaveStopOutcome(path string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopOutcomeTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.writeStopOutcome(ctx, path) }()
	select {
	case err := <-done:
		if err != nil {
			a.logger.Warn("could not leave what this replay served and missed for keploy to read",
				zap.String("path", path), zap.Error(err))
			return
		}
		a.logger.Debug("left what this replay served and missed for keploy to read", zap.String("path", path))
	case <-ctx.Done():
		a.logger.Warn("gave up leaving what this replay served and missed for keploy to read",
			zap.String("path", path), zap.Duration("after", stopOutcomeTimeout))
	}
}

// writeStopOutcome reads what the replay served and missed and writes it to
// path as a models.MockOutcome. Both reads drain, exactly as the CLI's own
// end-of-run reads do: this is the last time they are made.
//
// Written to a temporary file and renamed into place, so an agent killed
// halfway leaves no file rather than half of one. A missing file reads as
// "unknown"; a truncated one could only read as a parse error, or worse.
func (a *Agent) writeStopOutcome(ctx context.Context, path string) error {
	consumed, err := a.GetConsumedMocks(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the mocks this replay served: %w", err)
	}
	missed, err := a.GetMockErrors(ctx)
	if err != nil {
		return fmt.Errorf("failed to read the calls this replay could not match: %w", err)
	}
	body, err := json.Marshal(models.MockOutcome{Consumed: consumed, Missed: missed})
	if err != nil {
		return fmt.Errorf("failed to encode the replay outcome: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, werr := tmp.Write(body)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		return werr
	}
	committed = true
	return nil
}
