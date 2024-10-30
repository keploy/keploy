//go:build windows

package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/core"
	windows_comm "go.keploy.io/server/v2/pkg/core/hooks/ipc/windows"
	"go.keploy.io/server/v2/pkg/core/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func (h *Hooks) load(ctx context.Context, opts core.HookCfg) error {

	pipe := windows_comm.Pipe{
		Name:   `\\.\pipe\keploy-windows`,
		Logger: h.logger,
	}

	go func() {
		conn, err := pipe.Start(ctx)
		if err != nil {
			h.logger.Error("Unable to start commuciation with redirector", zap.Error(err))
			return
		}
		h.conn = conn

		clientInfo := structs.ClientInfo{
			KeployClientNsPid: uint32(os.Getpid()),
		}

		h.SendClientInfo(opts.AppID, clientInfo)
		h.ReadDestInfo(ctx)
	}()

	exePath := filepath.Join("windows", "windows-redirector.exe")
	exePath = filepath.Clean(exePath)
	fmt.Printf("Using executable path: %s\n", exePath)

	cmd := exec.CommandContext(ctx, exePath, `\\.\pipe\keploy-windows`)

	// Optional: Capture output
	// cmd.Stdout = os.Stdout
	// cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start the executable: %w", err)
	}

	return nil
}

func (h *Hooks) unLoad(_ context.Context) {
}

func (h *Hooks) Record(ctx context.Context, _ uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	// TODO use the session to get the app id
	// and then use the app id to get the test cases chan
	// and pass that to eBPF consumers/listeners
	return nil, nil
}
