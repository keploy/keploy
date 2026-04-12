package testdb

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func httpTC(method, url string) *models.TestCase {
	return &models.TestCase{
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: models.Method(method),
			URL:    url,
		},
	}
}

func grpcTC(path string) *models.TestCase {
	tc := &models.TestCase{Kind: models.GRPC_EXPORT}
	tc.GrpcReq.Headers.PseudoHeaders = map[string]string{":path": path}
	return tc
}

func TestBuildTestCaseSlug_HTTP(t *testing.T) {
	cases := []struct {
		name string
		in   *models.TestCase
		want string
	}{
		{"simple get", httpTC("GET", "http://api.test/users"), "get-users"},
		{"nested", httpTC("GET", "http://api.test/users/profile"), "get-users-profile"},
		{"numeric id", httpTC("GET", "http://api.test/users/42"), "get-users-by-id"},
		{"uuid id", httpTC("GET", "http://api.test/users/550e8400-e29b-41d4-a716-446655440000"), "get-users-by-id"},
		{"hex id", httpTC("GET", "http://api.test/objs/507f1f77bcf86cd799439011"), "get-objs-by-id"},
		{"post login", httpTC("POST", "http://api.test/auth/login"), "post-auth-login"},
		{"query string dropped", httpTC("GET", "http://api.test/users?limit=10&q=foo"), "get-users"},
		{"root path", httpTC("GET", "http://api.test/"), "get-root"},
		{"host-only with query", httpTC("GET", "http://api.test?x=1"), "get-root"},
		{"host-only no path", httpTC("POST", "http://api.test"), "post-root"},
		{"fragment only", httpTC("GET", "http://api.test#top"), "get-root"},
		{"bare path", httpTC("DELETE", "/items/7"), "delete-items-by-id"},
		{"trailing slash", httpTC("GET", "http://api.test/users/"), "get-users"},
		{"short non-numeric preserved", httpTC("GET", "http://api.test/users/me"), "get-users-me"},
		{"unicode sanitized", httpTC("GET", "http://api.test/caf\u00e9/menu"), "get-caf-menu"},
		{"empty url", httpTC("GET", ""), "get-root"},
		{"no method", httpTC("", "http://api.test/ping"), "ping"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildTestCaseSlug(c.in)
			if got != c.want {
				t.Fatalf("BuildTestCaseSlug()=%q want %q", got, c.want)
			}
		})
	}
}

func TestBuildTestCaseSlug_HTTP_LongPathTruncated(t *testing.T) {
	tc := httpTC("POST", "http://api.test/a/very/long/path/that/keeps/going/and/going/and/going/forever")
	got := BuildTestCaseSlug(tc)
	if len(got) > maxSlugLen {
		t.Fatalf("slug longer than max: len=%d slug=%q", len(got), got)
	}
	if got == "" || got[len(got)-1] == '-' {
		t.Fatalf("bad truncation: %q", got)
	}
}

func TestBuildTestCaseSlug_GRPC(t *testing.T) {
	cases := []struct {
		name string
		in   *models.TestCase
		want string
	}{
		{"typical", grpcTC("/users.UserService/GetUser"), "grpc-userservice-getuser"},
		{"no leading slash", grpcTC("users.UserService/GetUser"), "grpc-userservice-getuser"},
		{"deep package", grpcTC("/acme.v1.billing.BillingService/Charge"), "grpc-billingservice-charge"},
		{"missing method", grpcTC("/users.UserService/"), "grpc-userservice"},
		{"empty path", grpcTC(""), "grpc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildTestCaseSlug(c.in)
			if got != c.want {
				t.Fatalf("BuildTestCaseSlug()=%q want %q", got, c.want)
			}
		})
	}
}

func TestBuildTestCaseSlug_GRPC_MissingPseudoHeader(t *testing.T) {
	tc := &models.TestCase{Kind: models.GRPC_EXPORT}
	// nil map
	if got := BuildTestCaseSlug(tc); got != "grpc" {
		t.Fatalf("want grpc, got %q", got)
	}
}

func TestSanitizeSlug(t *testing.T) {
	cases := map[string]string{
		"GET-/users":      "get-users",
		"foo__bar":        "foo-bar",
		"  --foo--  ":     "foo",
		"Caf\u00e9 menu!": "caf-menu",
		"":                "",
	}
	for in, want := range cases {
		if got := sanitizeSlug(in); got != want {
			t.Errorf("sanitizeSlug(%q)=%q want %q", in, got, want)
		}
	}
}

func TestIsIDSegment(t *testing.T) {
	ids := []string{
		"1", "42", "0",
		"550e8400-e29b-41d4-a716-446655440000",
		"507f1f77bcf86cd799439011",
	}
	for _, s := range ids {
		if !isIDSegment(s) {
			t.Errorf("expected %q to be an id segment", s)
		}
	}
	nonIDs := []string{"users", "me", "login", "abc", "v1", "user42"}
	for _, s := range nonIDs {
		if isIDSegment(s) {
			t.Errorf("expected %q NOT to be an id segment", s)
		}
	}
}
