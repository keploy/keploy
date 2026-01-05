package models

import "time"

// RecordOptions contains configuration for the mock recording session.
type RecordOptions struct {
	// Command is the application command to execute.
	Command string
	// Path is the base path for mock storage.
	Path string
	// ProxyPort is the proxy port (optional, uses default if 0).
	ProxyPort uint32
	// DNSPort is the DNS port (optional, uses default if 0).
	DNSPort uint32
	// Duration is the recording duration/timeout.
	Duration time.Duration
}

// RecordResult contains the result of a recording session.
type RecordResult struct {
	// MockFilePath is the path to the generated mock file.
	MockFilePath string
	// Metadata contains extracted metadata for contextual naming.
	Metadata *MockMetadata
	// MockCount is the number of mocks recorded.
	MockCount int
	// AppExitCode is the application exit code.
	AppExitCode int
	// Output is the application stdout/stderr combined.
	Output string
	// Mocks contains the recorded mock objects.
	Mocks []*Mock
}

// MockMetadata contains extracted information from recorded mocks for contextual naming.
type MockMetadata struct {
	// Protocols contains the list of protocols detected (e.g., HTTP, Postgres, Redis).
	Protocols []string
	// Endpoints contains extracted endpoint details.
	Endpoints []EndpointInfo
	// ServiceName is inferred from the command.
	ServiceName string
	// Timestamp is the recording timestamp.
	Timestamp time.Time
}

// EndpointInfo contains information about a recorded endpoint.
type EndpointInfo struct {
	// Protocol is the protocol type (HTTP, gRPC, etc.).
	Protocol string
	// Host is the target host.
	Host string
	// Path is the URL path or method name.
	Path string
	// Method is the HTTP method or RPC method.
	Method string
}
