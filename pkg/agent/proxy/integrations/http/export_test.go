package http

import (
	"net/url"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

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

// ServeOnePassThroughMockForTest exposes serveOnePassThroughMock to the
// EXTERNAL http_test package, for the same import-cycle reason as above. The
// req struct is unexported, so the method/URL are taken as plain values and
// assembled here.
func (h *HTTP) ServeOnePassThroughMockForTest(mockDb integrations.MockMemDb, method string, u *url.URL, host string, port uint32, queryKeys []string) []byte {
	return h.serveOnePassThroughMock(mockDb, &req{method: method, url: u}, host, port, queryKeys)
}
