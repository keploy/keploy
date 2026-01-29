//go:build windows

package utils

import (
	"context"

	"go.uber.org/zap"
)

// CheckKeployFolderPermissions is a no-op on Windows.
// Windows doesn't use the Unix permission model, so we don't need to check for permission issues.
func CheckKeployFolderPermissions(_ *zap.Logger, _ string) ([]PermissionError, error) {
	return nil, nil
}

// GetPermissionFixCommand returns empty string on Windows.
// Permission fixes using sudo/chown are not applicable on Windows.
func GetPermissionFixCommand(_ string) string {
	return ""
}

// FixKeployFolderPermissions is a no-op on Windows.
// Permission fixes using sudo/chown are not applicable on Windows.
func FixKeployFolderPermissions(_ context.Context, _ *zap.Logger, _ string, _ []PermissionError) error {
	return nil
}

// AreSudoCredentialsCached always returns true on Windows.
// Sudo is not used on Windows, so we treat it as if credentials are always available.
func AreSudoCredentialsCached() bool {
	return true
}

// CacheSudoCredentials is a no-op on Windows.
// Sudo is not used on Windows.
func CacheSudoCredentials(_ context.Context, _ *zap.Logger) error {
	return nil
}

// FixFilePermission is a no-op on Windows.
// Permission fixes using sudo/chown are not applicable on Windows.
func FixFilePermission(_ context.Context, _ *zap.Logger, _ string) error {
	return nil
}

// EnsureKeployFolderPermissions is a no-op on Windows.
// Permission fixes using sudo/chown are not applicable on Windows.
func EnsureKeployFolderPermissions(_ context.Context, _ *zap.Logger, _ string) error {
	return nil
}

// RestoreKeployFolderOwnership is a no-op on Windows.
// Ownership restoration using chown is not applicable on Windows.
func RestoreKeployFolderOwnership(_ *zap.Logger, _ string) {
	// No-op on Windows
}
