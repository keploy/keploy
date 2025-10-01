//go:build !linux

// Package others provides hooks implementation for non-Linux platforms.
package others

import (
	"context"
	"errors"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/agent"
	"go.keploy.io/server/v2/pkg/agent/hooks/common"
	"go.keploy.io/server/v2/pkg/agent/hooks/structs"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

// Hooks implements the agent.Hooks interface for non-Linux platforms.
// It embeds BaseHooks to inherit common functionality.
type Hooks struct {
	*common.BaseHooks
}

// NewHooks creates a new hooks instance for non-Linux platforms.
func NewHooks(logger *zap.Logger, cfg *config.Config) *Hooks {
	return &Hooks{
		BaseHooks: common.NewBaseHooks(logger, cfg),
	}
}

// Load implements the Load method for non-Linux platforms.
// Since eBPF is not available on non-Linux platforms, this returns an error.
func (h *Hooks) Load(ctx context.Context, id uint64, opts agent.HookCfg) error {
	h.Logger.Error("eBPF hooks are not supported on this platform")
	return errors.New("eBPF hooks are not supported on non-Linux platforms")
}

// Record implements the Record method for non-Linux platforms.
// Since hooks are not available, this returns an error.
func (h *Hooks) Record(ctx context.Context, id uint64, opts models.IncomingOptions) (<-chan *models.TestCase, error) {
	h.Logger.Error("Recording is not supported on this platform")
	return nil, errors.New("recording is not supported on non-Linux platforms")
}

// WatchBindEvents implements the WatchBindEvents method for non-Linux platforms.
// Since eBPF is not available, this returns an error.
func (h *Hooks) WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error) {
	h.Logger.Error("Bind event watching is not supported on this platform")
	return nil, errors.New("bind event watching is not supported on non-Linux platforms")
}

// Get implements the DestInfo.Get method for non-Linux platforms.
func (h *Hooks) Get(ctx context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	h.Logger.Error("Network address lookup is not supported on this platform")
	return nil, errors.New("network address lookup is not supported on non-Linux platforms")
}

// Delete implements the DestInfo.Delete method for non-Linux platforms.
func (h *Hooks) Delete(ctx context.Context, srcPort uint16) error {
	h.Logger.Error("Network address deletion is not supported on this platform")
	return errors.New("network address deletion is not supported on non-Linux platforms")
}

// SendKeployClientInfo implements the SendKeployClientInfo method for non-Linux platforms.
func (h *Hooks) SendKeployClientInfo(clientInfo structs.ClientInfo) error {
	h.Logger.Warn("SendKeployClientInfo not implemented for non-Linux platforms")
	return nil
}

// DeleteKeployClientInfo implements the DeleteKeployClientInfo method for non-Linux platforms.
func (h *Hooks) DeleteKeployClientInfo(clientID uint64) error {
	h.Logger.Warn("DeleteKeployClientInfo not implemented for non-Linux platforms")
	return nil
}

// SendClientProxyInfo implements the SendClientProxyInfo method for non-Linux platforms.
func (h *Hooks) SendClientProxyInfo(clientID uint64, proxyInfo structs.ProxyInfo) error {
	h.Logger.Warn("SendClientProxyInfo not implemented for non-Linux platforms")
	return nil
}
