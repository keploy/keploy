package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	coreagent "go.keploy.io/server/v3/pkg/agent"
)

func TestAppIDMiddlewareStampsKey(t *testing.T) {
	var got coreagent.AppKey
	var ok bool
	h := appIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, ok = coreagent.AppKeyFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/agent/health", nil)
	req.Header.Set(AppIDHeader, "ns/dep/ts-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !ok || got != coreagent.AppKey("ns/dep/ts-1") {
		t.Fatalf("expected key ns/dep/ts-1 on ctx, got (%q, %v)", got, ok)
	}
}

func TestAppIDMiddlewareNoHeaderIsDefault(t *testing.T) {
	var present bool
	h := appIDMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, present = coreagent.AppKeyFromContext(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/agent/health", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if present {
		t.Fatalf("expected no app key on ctx when header absent (DefaultAppKey path)")
	}
}
