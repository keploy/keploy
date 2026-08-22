//go:build !(windows && amd64)

package http

import "go.keploy.io/server/v3/pkg/models"

// prepareNativeInterception is a no-op off 64-bit Windows.
//
// The unprivileged Windows backend is the only platform that needs client-side
// preparation at process-creation time; Linux attaches eBPF programs from the
// agent and macOS injects its shim through the environment. See
// native_windows.go.
func (a *AgentClient) prepareNativeInterception(_ models.SetupOptions) {}
