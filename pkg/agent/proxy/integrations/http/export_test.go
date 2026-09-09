package http

import "go.keploy.io/server/v3/pkg/models"

// BuildMockResponseBytesForTest exposes buildMockResponseBytes to the EXTERNAL
// http_test package. That package exists only because the spilled-response
// regression has to drive the real proxy.DiskMocks store, and pkg/agent/proxy
// blank-imports this package (parsers.go), so an internal `package http` test
// importing it would be an import cycle.
func (h *HTTP) BuildMockResponseBytesForTest(stub *models.Mock) ([]byte, error) {
	return h.buildMockResponseBytes(stub)
}

// NewForTest builds a logger-only HTTP integration, mirroring newHTTP() in
// match_test.go, for the external test package.
func NewForTest() *HTTP { return newHTTP() }
