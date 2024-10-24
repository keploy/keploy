// Package hooks provides functionality for managing hooks.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		logger:    logger,
		sess:      core.NewSessions(),
		m:         sync.Mutex{},
		proxyIP4:  "127.0.0.1",
		proxyIP6:  [4]uint32{0000, 0000, 0000, 0001},
		proxyPort: cfg.ProxyPort,
		dnsPort:   cfg.DNSPort,
	}
}

type Hooks struct {
	logger    *zap.Logger
	sess      *core.Sessions
	proxyIP4  string
	proxyIP6  [4]uint32
	proxyPort uint32
	dnsPort   uint32

	m     sync.Mutex
	appID uint64
}

func (h *Hooks) Load(ctx context.Context, id uint64, opts core.HookCfg) error {

	h.sess.Set(id, &core.Session{
		ID: id,
	})

	err := h.load(ctx, opts)
	if err != nil {
		return err
	}

	g, ok := ctx.Value(models.ErrGroupKey).(*errgroup.Group)
	if !ok {
		return errors.New("failed to get the error group from the context")
	}

	g.Go(func() error {
		defer utils.Recover(h.logger)
		<-ctx.Done()
		h.unLoad(ctx)
		h.sess.Delete(id)
		return nil
	})

	return nil
}

func (h *Hooks) load(ctx context.Context, opts core.HookCfg) error {
	// Create the command using the provided executable path
	cmd := exec.CommandContext(ctx, `C:\Users\yashk\OneDrive\Desktop\windows_native\mitmproxy_rs\target\debug\windows-redirector.exe`, `\\.\pipe\mitmproxy-transparent-proxy-1`)

	// Optional: Capture output
	cmd.Stdout = os.Stdout // You can set this to os.Stdout to see output in console
	cmd.Stderr = os.Stderr // You can set this to os.Stderr to see errors in console

	// Run the command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start the executable: %w", err)
	}

	// Wait for the command to complete
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("executable exited with an error: %w", err)
	}

	fmt.Println("Executable ran successfully")
	return nil
}

func (h *Hooks) Record(ctx context.Context, _ uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	// TODO use the session to get the app id
	// and then use the app id to get the test cases chan
	// and pass that to eBPF consumers/listeners
	return nil, nil
}

func (h *Hooks) unLoad(_ context.Context) {
	// stop name pipe
	// stop windivert
}
