package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The client asks the agent for a set's end read only for a marked read: a
// test's end read carries no query.
func TestGetMockErrors_OnlyASetEndReadSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		wantQuery string
	}{
		{"a test's end", context.Background(), ""},
		{"the set's end", models.WithSetEndRead(context.Background()), models.SetEndReadQuery + "=1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path, query string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, query = r.URL.Path, r.URL.RawQuery
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()
			a := New(zap.NewNop(), nil, &config.Config{Agent: config.Agent{AgentURI: srv.URL}})
			if _, err := a.GetMockErrors(tc.ctx); err != nil {
				t.Fatal(err)
			}
			if path != "/mockerrors" || query != tc.wantQuery {
				t.Fatalf("asked %s?%s, want /mockerrors?%s", path, query, tc.wantQuery)
			}
		})
	}
}
