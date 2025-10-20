//go:build !linux

// Package others provides hooks implementation for non-Linux platforms.
package others

import (
	"context"
	"errors"
	"sync"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// Hooks implements the agent.Hooks interface for non-Linux platforms.
type Hooks struct {
	// Common fields shared across all platforms
	logger    *zap.Logger
	sess      *agent.Sessions
	proxyIP4  string
	proxyIP6  [4]uint32
	proxyPort uint32
	dnsPort   uint32
	conf      *config.Config
	m         sync.Mutex
}

// NewHooks creates a new hooks instance for non-Linux platforms.
func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		logger:    logger,
		sess:      agent.NewSessions(),
		m:         sync.Mutex{},
		proxyIP4:  "127.0.0.1",
		proxyIP6:  [4]uint32{0000, 0000, 0000, 0001},
		proxyPort: cfg.ProxyPort,
		dnsPort:   cfg.DNSPort,
		conf:      cfg,
	}
}

// Load implements the Load method for non-Linux platforms.
// Since eBPF is not available on non-Linux platforms, this returns an error.
func (h *Hooks) Load(ctx context.Context, opts agent.HookCfg, setupOpts config.Agent) error {
	h.logger.Error("eBPF hooks are not supported on this platform")
	return errors.New("eBPF hooks are not supported on non-Linux platforms")
}

// Record implements the Record method for non-Linux platforms.
// Since hooks are not available, this returns an error.
func (h *Hooks) Record(ctx context.Context, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	h.logger.Error("Recording is not supported on this platform")
	return nil, errors.New("recording is not supported on non-Linux platforms")
}

// WatchBindEvents implements the WatchBindEvents method for non-Linux platforms.
// Since eBPF is not available, this returns an error.
func (h *Hooks) WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error) {
	h.logger.Error("Bind event watching is not supported on this platform")
	return nil, errors.New("bind event watching is not supported on non-Linux platforms")
}

// Get implements the DestInfo.Get method for non-Linux platforms.
func (h *Hooks) Get(ctx context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	h.logger.Error("Network address lookup is not supported on this platform")
	return nil, errors.New("network address lookup is not supported on non-Linux platforms")
}

// Delete implements the DestInfo.Delete method for non-Linux platforms.
func (h *Hooks) Delete(ctx context.Context, srcPort uint16) error {
	h.logger.Error("Network address deletion is not supported on this platform")
	return errors.New("network address deletion is not supported on non-Linux platforms")
}
