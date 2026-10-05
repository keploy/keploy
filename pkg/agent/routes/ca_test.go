package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestCACert_ServiceUnavailableWithoutCA verifies the endpoint reports 503 — not
// an empty 200 — when no CA has been established yet, so the client can tell
// "not ready" from "transport failure". In this test binary SetupCA never runs,
// so the active CA is nil.
func TestCACert_ServiceUnavailableWithoutCA(t *testing.T) {
	a := &Agent{logger: zap.NewNop()}
	rec := httptest.NewRecorder()
	a.CACert(rec, httptest.NewRequest(http.MethodGet, "/agent/ca", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"CACert must return 503 before a CA is established, not an empty 200")
}
