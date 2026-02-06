package models

import "time"

// ReplayOptions contains configuration for the mock replay session.
type ReplayOptions struct {
	// Command is the application command to execute.
	Command string
	// Path is the base path where mocks are stored.
	Path string
	// ProxyPort is the proxy port (optional, uses default if 0).
	ProxyPort uint32
	// DNSPort is the DNS port (optional, uses default if 0).
	DNSPort uint32
	// Timeout is the maximum duration for the replay (optional).
	Timeout time.Duration
	// FallBackOnMiss indicates whether to fall back to real calls on mock miss.
	FallBackOnMiss bool
}

// ReplayResult contains the result of a replay session.
type ReplayResult struct {
	// Success indicates overall success of the replay.
	Success bool
	// MocksReplayed is the number of mocks that were replayed.
	MocksReplayed int
	// MocksMissed is the number of unmatched calls.
	MocksMissed int
	// AppExitCode is the application exit code.
	AppExitCode int
	// Output is the application stdout/stderr combined.
	Output string
	// ConsumedMocks contains the state of consumed mocks.
	ConsumedMocks []MockState
}
