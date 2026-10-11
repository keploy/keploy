//go:build !linux

package tls

import (
	"context"
	"os"

	"go.uber.org/zap"
)

// The system-store install these guard is the Linux native path: macOS and
// Windows never write an anchor into a shared directory or run a tool that
// regenerates a trust bundle (see setupNativeDarwin and installWindowsCA). So
// off Linux there is nothing for two agents to contend over, and these keep
// the behaviour those platforms always had. See truststore_linux.go.

// lockTrustStore is a no-op off Linux. The work context is still not cancelled
// with ctx, so a store command that has started runs to its end everywhere.
func lockTrustStore(ctx context.Context, _ *zap.Logger) (context.Context, func(), error) {
	return context.WithoutCancel(ctx), func() {}, nil
}

// claimRun returns a claim that holds nothing.
func claimRun(string) (*runClaim, error) { return &runClaim{}, nil }

// runIsLive reports every run as gone: with no claims, none can be told apart.
func runIsLive(string) (bool, error) { return false, nil }

// sweepRunClaims has no claims to sweep.
func sweepRunClaims(*zap.Logger) {}

// writeAnchor writes the anchor file.
func writeAnchor(path string, certPEM []byte) error {
	return os.WriteFile(path, certPEM, 0o644)
}

// swapP11KitTrust never applies: the p11-kit trust layout is Linux's.
func swapP11KitTrust(context.Context, *zap.Logger, string) (bool, error) { return false, nil }

// systemTrustGenerates is false: no keystore is generated off Linux.
func systemTrustGenerates(string) bool { return false }
