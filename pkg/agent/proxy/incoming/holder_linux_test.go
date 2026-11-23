//go:build linux

package proxy

import "go.keploy.io/server/v3/pkg/agent/hooks/linux"

// The agent's Linux hooks are the ingress manager's port holder (New
// type-asserts them): without it, hooks that steer the app's port would steer
// it to a park nothing serves, and nothing would be recorded. This makes a
// method missing from them a build error.
var _ IngressPortHolder = (*linux.Hooks)(nil)
