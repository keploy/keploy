//go:build !linux

package synhold

import (
	"context"
	"errors"

	"go.uber.org/zap"
)

// Holder exists on Linux only; elsewhere the proxy accepts handshakes at once.
type Holder struct{}

// Start reports that handshakes cannot be held on this platform.
func Start(context.Context, *zap.Logger, uint16, Decide) (*Holder, error) {
	return nil, errors.New("holding handshakes needs Linux nf_tables")
}

// Close does nothing.
func (*Holder) Close() error { return nil }
