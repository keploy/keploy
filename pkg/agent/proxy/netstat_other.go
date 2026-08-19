//go:build !linux

package proxy

import (
	"context"

	"go.uber.org/zap"
)

// Non-Linux stubs: the kernel eBPF network-I/O counter is a Linux facility, so
// the proxy network-I/O accounting is a no-op elsewhere (the agent runs on Linux
// in production).

// SetNetworkIOSink is a no-op on non-Linux.
func SetNetworkIOSink(_ func(rx, tx uint64)) {}

// StartKernelNetioDrain is a no-op on non-Linux (the eBPF counter is Linux-only).
func StartKernelNetioDrain(_ context.Context, _ *zap.Logger) {}
