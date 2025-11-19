//go:build windows

// Package hooks provides functionality for managing hooks.
package windows

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sync/errgroup"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

//go:embed assets/WinDivert.dll
var windivertDLL []byte

//go:embed assets/WinDivert64.sys
var windivert64DLL []byte

//go:embed assets
var _ embed.FS

func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		logger:    logger,
		sess:      agent.NewSessions(),
		m:         sync.Mutex{},
		proxyIP4:  "127.0.0.1",
		proxyIP6:  [4]uint32{0000, 0000, 0000, 0001},
		proxyPort: cfg.ProxyPort,
		dnsPort:   cfg.DNSPort,
	}
}

type Hooks struct {
	logger    *zap.Logger
	sess      *agent.Sessions
	proxyIP4  string
	proxyIP6  [4]uint32
	proxyPort uint32
	dnsPort   uint32
	m         sync.Mutex
}

func (h *Hooks) Load(ctx context.Context, cfg agent.HookCfg, setupOpts config.Agent) error {

	h.sess.Set(uint64(0), &agent.Session{
		ID: uint64(0), // need to check this one
	})

	err := h.load(ctx, setupOpts)
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

		//deleting in order to free the memory in case of rerecord.
		h.sess.Delete(uint64(0))
		return nil
	})

	return nil
}

func (h *Hooks) load(_ context.Context, setupOpts config.Agent) error {
	// Ensure the WinDivert artifacts are present under $HOME/.keploy.
	if err := h.ensureWinDivertAssets(); err != nil {
		// Log and return the error so load fails fast if writing assets fails.
		h.logger.Error("failed to ensure windivert assets", zap.Error(err))
		return err
	}

	clientPID := uint32(setupOpts.ClientNSPID)
	agentPID := uint32(os.Getpid())

	var mode uint32

	switch setupOpts.Mode {
	case models.MODE_TEST:
		mode = 2
	case models.MODE_RECORD:
		mode = 1
	default:
		mode = 0
	}

	err := StartRedirector(clientPID, agentPID, h.proxyPort, h.dnsPort, uint32(3000), "C:\\Users\\keploy\\ayush_work\\keploy\\pkg\\agent\\hooks\\windows\\assets\\WinDivert.dll", mode)
	if err != nil {
		h.logger.Error("failed to start redirector", zap.Error(err))
		return err
	}

	return nil
}

// ensureWinDivertAssets checks $HOME/.keploy and writes the embedded
// WinDivert DLL/SYS files if they are missing.
func (h *Hooks) ensureWinDivertAssets() error {
	home, err := os.UserHomeDir()
	if err != nil {
		h.logger.Warn("unable to determine user home directory, skipping writing windivert files", zap.Error(err))
		return nil
	}

	keployDir := filepath.Join(home, ".keploy")
	if err := os.MkdirAll(keployDir, 0o755); err != nil {
		h.logger.Error("failed to create .keploy dir", zap.Error(err))
		return fmt.Errorf("failed to create keploy dir: %w", err)
	}

	dllPath := filepath.Join(keployDir, "WinDivert.dll")
	if _, err := os.Stat(dllPath); errors.Is(err, os.ErrNotExist) {
		h.logger.Info("writing WinDivert.dll", zap.String("path", dllPath))
		if err := os.WriteFile(dllPath, windivertDLL, 0o644); err != nil {
			h.logger.Error("failed to write WinDivert.dll", zap.Error(err))
			return fmt.Errorf("failed to write WinDivert.dll: %w", err)
		}
	}

	sysPath := filepath.Join(keployDir, "WinDivert64.sys")
	if _, err := os.Stat(sysPath); errors.Is(err, os.ErrNotExist) {
		h.logger.Info("writing WinDivert64.sys", zap.String("path", sysPath))
		if err := os.WriteFile(sysPath, windivert64DLL, 0o644); err != nil {
			h.logger.Error("failed to write WinDivert64.sys", zap.Error(err))
			return fmt.Errorf("failed to write WinDivert64.sys: %w", err)
		}
	}

	return nil
}

func (h *Hooks) unLoad(_ context.Context) {
	err := StopRedirector()
	if err != nil {
		h.logger.Error("failed to stop redirector", zap.Error(err))
	}
}

func (h *Hooks) Record(ctx context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	return nil, nil
}

func (h *Hooks) WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error) {
	ch := make(chan models.IngressEvent, 1024)

	ch <- models.IngressEvent{
		OrigAppPort: 3000,
		NewAppPort:  0000,
	}

	return ch, nil
}
