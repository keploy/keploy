//go:build windows

package hooks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/conn"
	windows_comm "go.keploy.io/server/v2/pkg/core/hooks/ipc/windows"
	"go.keploy.io/server/v2/pkg/core/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func (h *Hooks) load(ctx context.Context, opts core.HookCfg) error {

	unixSocket := windows_comm.UnixSocket{
		Path:   `C:\my.sock`,
		Logger: h.logger,
	}

	connChan := make(chan net.Conn, 1)
	errChan := make(chan error, 1)

	go func() {
		conn, err := unixSocket.Start(ctx)
		if err != nil {
			h.logger.Error("Unable to start commuciation with redirector", zap.Error(err))
			errChan <- err
			return
		}
		connChan <- conn
	}()

	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return errors.New("unable to get the current filename")
	}
	dirname := filepath.Dir(filename)

	// Join the current directory with the relative path to the executable
	exePath := filepath.Join(dirname, "windows", "windows-redirector.exe")
	exePath = filepath.Clean(exePath)

	fmt.Println("came here")

	cmd := exec.CommandContext(ctx, exePath, `C:\my.sock`)

	fmt.Println("not stuck")

	// Optional: Capture output
	if h.logger.Level() == zapcore.DebugLevel {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start the executable: %w", err)
	}

	select {
	case conn := <-connChan:
		h.conn = conn
		clientInfo := structs.ClientInfo{
			KeployClientNsPid: uint32(os.Getpid()),
		}
		err := h.SendClientInfo(opts.AppID, clientInfo)
		if err != nil {
			h.logger.Error("failed to send client info", zap.Error(err))
			return fmt.Errorf("failed to load hooks")
		}
	case <-errChan:
		return fmt.Errorf("failed to load hooks")
	}

	go func() {
		h.GetEvents(ctx)
	}()
	return nil
}

func (h *Hooks) unLoad(_ context.Context) {
}

func (h *Hooks) Record(ctx context.Context, _ uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	return conn.ListenSocket(ctx, h.logger, h.openEventChan, h.dataEventChan, h.closeEventChan, opts)
}
