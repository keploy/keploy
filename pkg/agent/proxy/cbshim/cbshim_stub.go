//go:build !linux || !(amd64 || arm64)

package cbshim

import (
	"context"
	"crypto/x509"

	"go.uber.org/zap"
)

// Stub implementation for platforms without BPF support. Compiles to
// no-ops so callers (proxy.go, proxy_v2.go) don't need build tags —
// SCRAM-SHA-256-PLUS channel binding via uprobe is wired on linux/amd64
// and linux/arm64. Other platforms (darwin, windows, linux 386, etc.)
// fall back to the legacy behaviour (PLUS auth fails closed if the
// client requires it).

type CBShim struct{}

type LibpqRange struct {
	Start uint64
	End   uint64
}

type Counters struct {
	TotalFires  uint64
	TGIDMatched uint64
	LibpqFires  uint64
	LookupHit   uint64
	LookupMiss  uint64
	WriteOK     uint64
	WriteFail   uint64
}

func New(_ *zap.Logger) (*CBShim, error)                                     { return &CBShim{}, nil }
func (c *CBShim) Close() error                                               { return nil }
func (c *CBShim) RegisterPID(_ uint32) error                                 { return nil }
func (c *CBShim) UnregisterPID(_ uint32)                                     {}
func (c *CBShim) IsPIDRegistered(_ uint32) bool                              { return false }
func (c *CBShim) RegisterLibpqRanges(_ uint32, _ []LibpqRange) error         { return nil }
func (c *CBShim) AttachToLibcrypto(_ string) error                           { return nil }
func (c *CBShim) AttachToProcess(_ int) error                                { return nil }
func (c *CBShim) AttachToProcessTree(_ int) error                            { return nil }
func (c *CBShim) WatchProcessTree(_ context.Context, _ int)                  {}
func (c *CBShim) DetachFromProcessTree(_ int)                                {}
func (c *CBShim) RegisterMITM(_ string, _ []byte)                            {}
func (c *CBShim) RegisterReal(_ string, _ []byte, _ x509.SignatureAlgorithm) {}
func (c *CBShim) CleanupConnection(_ string)                                 {}
func (c *CBShim) Publish(_, _ []byte, _ x509.SignatureAlgorithm) error       { return nil }
func (c *CBShim) Counters() Counters                                         { return Counters{} }
