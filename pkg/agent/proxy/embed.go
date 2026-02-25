package proxy

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"

	"go.uber.org/zap"
)

//go:embed bin/*
var embeddedFiles embed.FS

// ExtractRustProxy extracts the embedded Rust proxy binary to the temp directory
// and returns the execution path.
func ExtractRustProxy(logger *zap.Logger) (string, error) {
	// Destination in the temp directory
	destPath := filepath.Join(os.TempDir(), "keploy-rust-proxy")

	// Read from embedded filesystem
	data, err := fs.ReadFile(embeddedFiles, "bin/keploy-rust-proxy")
	if err != nil {
		logger.Warn("Embedded keploy-rust-proxy binary not found, ensuring you have compiled it and placed into pkg/agent/proxy/bin/", zap.Error(err))
		return "keploy-rust-proxy", nil // Fallback to searching in PATH
	}

	// Write to temp file
	if err := os.WriteFile(destPath, data, 0755); err != nil {
		logger.Error("Failed to write embedded rust proxy to temp dir", zap.Error(err))
		return "keploy-rust-proxy", err
	}

	logger.Debug("Successfully extracted rust proxy binary", zap.String("path", destPath))
	return destPath, nil
}
