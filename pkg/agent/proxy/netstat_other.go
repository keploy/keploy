//go:build !linux

package proxy

import (
	"context"
	"net"

	"go.uber.org/zap"
)

// Non-Linux stubs: kernel TCP_INFO byte counters are a Linux facility, so the
// proxy network-I/O accounting is a no-op elsewhere (the agent runs on Linux in
// production).

// SetNetworkIOSink is a no-op on non-Linux.
func SetNetworkIOSink(_ func(rx, tx uint64)) {}

// RecordConnNetworkIO is a no-op on non-Linux.
func RecordConnNetworkIO(_ net.Conn) {}

// StartKernelNetioDrain is a no-op on non-Linux (the eBPF counter is Linux-only).
func StartKernelNetioDrain(_ context.Context, _ *zap.Logger) {}
