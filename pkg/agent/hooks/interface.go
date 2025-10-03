// Package hooks provides a unified architecture for platform-specific hook implementations.
// This package defines common interfaces and base functionality that can be extended
// by platform-specific implementations.
package hooks

// Architecture Overview:
//
// Platform implementations:
// - Linux: Uses eBPF for kernel-level network interception
// - Others (Darwin, Windows, etc.): Provides stubs that return appropriate errors
//
// Design:
// 1. BaseHooks (in common package): Common fields and functionality
// 2. Platform-specific implementations: Embed BaseHooks and implement agent.Hooks interface
// 3. Build-constrained factory functions: Create appropriate implementation based on platform

// Implementation notes:
//
// Linux Implementation (pkg/agent/hooks/linux):
// - Embeds common.BaseHooks for shared functionality
// - Implements full eBPF-based network interception
// - Uses kernel hooks for socket monitoring, connection tracking, etc.
// - Supports both IPv4 and IPv6
// - Manages eBPF programs, maps, and link attachments
//
// Non-Linux Implementation (pkg/agent/hooks/others):
// - Embeds common.BaseHooks for shared functionality
// - Provides stub implementations that log warnings/errors
// - Returns appropriate error messages for unsupported operations
// - Maintains interface compatibility without functionality
//
// Common Base (pkg/agent/hooks/common):
// - BaseHooks struct with shared fields (logger, sessions, config, etc.)
// - Common utility methods
// - Thread-safe operations with proper synchronization
// - Standardized lifecycle management (load/unload signaling)
//
// Factory Pattern:
// - hooks_linux.go: Build constraint //go:build linux
// - hooks_others.go: Build constraint //go:build !linux
// - Both provide New(logger, cfg) agent.Hooks factory function
// - Compile-time platform selection ensures appropriate implementation
