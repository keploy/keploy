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

// A test's end read names its test to the agent, escaped; a read that names
// none, and a set's end read, carry no test.
func TestGetMockErrors_ATestsEndReadNamesItsTest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		wantQuery string
	}{
		{"unmarked", context.Background(), ""},
		{"a test's end", models.WithTestEndRead(context.Background(), "test-1"), models.TestEndReadQuery + "=test-1"},
		{"a name to escape", models.WithTestEndRead(context.Background(), "a b&c=d"), models.TestEndReadQuery + "=a+b%26c%3Dd"},
		{"the set's end", models.WithTestEndRead(models.WithSetEndRead(context.Background()), "test-1"), models.SetEndReadQuery + "=1"},
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

// The read of what was consumed says it is the set's end only when marked.
func TestGetConsumedMocks_OnlyASetEndReadSaysSo(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		wantQuery string
	}{
		{"unmarked", context.Background(), ""},
		{"a test's end", models.WithTestEndRead(context.Background(), "test-1"), ""},
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
			if _, err := a.GetConsumedMocks(tc.ctx); err != nil {
				t.Fatal(err)
			}
			if path != "/consumedmocks" || query != tc.wantQuery {
				t.Fatalf("asked %s?%s, want /consumedmocks?%s", path, query, tc.wantQuery)
			}
		})
	}
}
