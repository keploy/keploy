// Package common provides common functionality shared across hook implementations.
package common

import (
	"fmt"
	"sync"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/agent"
	"go.uber.org/zap"
)

// BaseHooks contains common fields and functionality shared across all platforms.
// Platform-specific implementations should embed this struct.
type BaseHooks struct {
	Logger    *zap.Logger
	Sess      *agent.Sessions
	ProxyIP4  string
	ProxyIP6  [4]uint32
	ProxyPort uint32
	DNSPort   uint32
	Conf      *config.Config
	M         sync.Mutex

	// Channel to signal when unload is complete
	UnloadDone      chan struct{}
	UnloadDoneMutex sync.Mutex
}

// NewBaseHooks creates a new base hooks instance with common configuration.
func NewBaseHooks(logger *zap.Logger, cfg *config.Config) *BaseHooks {
	return &BaseHooks{
		Logger:     logger,
		Sess:       agent.NewSessions(),
		M:          sync.Mutex{},
		ProxyIP4:   "127.0.0.1",
		ProxyIP6:   [4]uint32{0000, 0000, 0000, 0001},
		ProxyPort:  cfg.ProxyPort,
		DNSPort:    cfg.DNSPort,
		Conf:       cfg,
		UnloadDone: make(chan struct{}),
	}
}

// GetUnloadDone returns a channel that signals when unload is complete.
func (b *BaseHooks) GetUnloadDone() <-chan struct{} {
	return b.UnloadDone
}

// SignalUnloadDone signals that unload is complete.
func (b *BaseHooks) SignalUnloadDone() {
	b.UnloadDoneMutex.Lock()
	defer b.UnloadDoneMutex.Unlock()

	select {
	case <-b.UnloadDone:
		// Channel already closed
	default:
		close(b.UnloadDone)
	}
}

// GetProxyAddress returns the formatted proxy address string.
func (b *BaseHooks) GetProxyAddress() string {
	return fmt.Sprintf("%s:%d", b.ProxyIP4, b.ProxyPort)
}

// GetDNSAddress returns the formatted DNS address string.
func (b *BaseHooks) GetDNSAddress() string {
	return fmt.Sprintf("%s:%d", b.ProxyIP4, b.DNSPort)
}

// IsValidSession checks if a session with the given ID exists.
func (b *BaseHooks) IsValidSession(id uint64) bool {
	_, exists := b.Sess.Get(id)
	return exists
}
