// Package hooks provides functionality for managing hooks across different platforms.
package hooks

import (
	"context"
	"sync"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/agent"
	"go.keploy.io/server/v2/pkg/agent/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// HookManager defines the interface that all platform-specific hook implementations must satisfy.
// This interface extends agent.Hooks to ensure compatibility with existing code.
type HookManager interface {
	agent.Hooks

	// Platform-specific methods that may need different implementations
	WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error)
}

// BaseHooks contains common fields and functionality shared across all platforms.
// Platform-specific implementations should embed this struct.
type BaseHooks struct {
	logger    *zap.Logger
	sess      *agent.Sessions
	proxyIP4  string
	proxyIP6  [4]uint32
	proxyPort uint32
	dnsPort   uint32
	conf      *config.Config
	m         sync.Mutex

	// Channel to signal when unload is complete
	unloadDone      chan struct{}
	unloadDoneMutex sync.Mutex
}

// NewBaseHooks creates a new base hooks instance with common configuration.
func NewBaseHooks(logger *zap.Logger, cfg *config.Config) *BaseHooks {
	return &BaseHooks{
		logger:     logger,
		sess:       agent.NewSessions(),
		m:          sync.Mutex{},
		proxyIP4:   "127.0.0.1",
		proxyIP6:   [4]uint32{0000, 0000, 0000, 0001},
		proxyPort:  cfg.ProxyPort,
		dnsPort:    cfg.DNSPort,
		conf:       cfg,
		unloadDone: make(chan struct{}),
	}
}

// GetLogger returns the logger instance.
func (b *BaseHooks) GetLogger() *zap.Logger {
	return b.logger
}

// GetSessions returns the sessions manager.
func (b *BaseHooks) GetSessions() *agent.Sessions {
	return b.sess
}

// GetConfig returns the configuration.
func (b *BaseHooks) GetConfig() *config.Config {
	return b.conf
}

// GetProxyInfo returns proxy configuration information.
func (b *BaseHooks) GetProxyInfo() (string, [4]uint32, uint32, uint32) {
	return b.proxyIP4, b.proxyIP6, b.proxyPort, b.dnsPort
}

// GetUnloadDone returns a channel that signals when unload is complete.
func (b *BaseHooks) GetUnloadDone() <-chan struct{} {
	return b.unloadDone
}

// Lock provides thread-safe access to the hooks instance.
func (b *BaseHooks) Lock() {
	b.m.Lock()
}

// Unlock releases the lock on the hooks instance.
func (b *BaseHooks) Unlock() {
	b.m.Unlock()
}

// SignalUnloadDone signals that unload is complete.
func (b *BaseHooks) SignalUnloadDone() {
	b.unloadDoneMutex.Lock()
	defer b.unloadDoneMutex.Unlock()

	select {
	case <-b.unloadDone:
		// Channel already closed
	default:
		close(b.unloadDone)
	}
}

// DefaultImplementations provides default implementations for methods that
// might not be supported on all platforms.

// DeleteKeployClientInfo provides a default implementation that may be overridden.
func (b *BaseHooks) DeleteKeployClientInfo(id uint64) error {
	b.logger.Warn("DeleteKeployClientInfo not implemented for this platform")
	return nil
}

// SendClientProxyInfo provides a default implementation that may be overridden.
func (b *BaseHooks) SendClientProxyInfo(clientID uint64, proxyInfo structs.ProxyInfo) error {
	b.logger.Warn("SendClientProxyInfo not implemented for this platform")
	return nil
}

// SendKeployClientInfo provides a default implementation that may be overridden.
func (b *BaseHooks) SendKeployClientInfo(clientID uint64, clientInfo structs.ClientInfo) error {
	b.logger.Warn("SendKeployClientInfo not implemented for this platform")
	return nil
}
