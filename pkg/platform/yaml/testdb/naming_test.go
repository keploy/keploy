package testdb

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
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

func TestBuildTestCaseSlug_NilSafe(t *testing.T) {
	if got := BuildTestCaseSlug(nil); got != fallbackTC {
		t.Fatalf("nil input got=%q want=%q", got, fallbackTC)
	}
}

func TestBuildTestCaseSlug_UnsupportedKindFallback(t *testing.T) {
	// A testcase with a non-HTTP, non-gRPC Kind and no HTTPReq
	// should land on a stable, kind-tagged fallback rather than
	// silently slugging an empty URL.
	tc := &models.TestCase{Kind: models.REDIS}
	got := BuildTestCaseSlug(tc)
	if got != "test-redis" {
		t.Fatalf("redis kind got=%q want=test-redis", got)
	}
}

func TestBuildTestCaseSlug_UnsupportedKindButHTTPReq(t *testing.T) {
	// Unknown Kind but HTTPReq is populated — we still produce a
	// useful slug from the request rather than falling back.
	tc := &models.TestCase{
		Kind: "Unknown",
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    "http://api.test/users",
		},
	}
	if got := BuildTestCaseSlug(tc); got != "get-users" {
		t.Fatalf("got=%q want=get-users", got)
	}
}

func TestBuildTestCaseSlug_HTTP2Kind(t *testing.T) {
	tc := &models.TestCase{
		Kind: models.HTTP2,
		HTTPReq: models.HTTPReq{
			Method: "POST",
			URL:    "http://api.test/items",
		},
	}
	if got := BuildTestCaseSlug(tc); got != "post-items" {
		t.Fatalf("got=%q want=post-items", got)
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

func TestParseNamingStrategy(t *testing.T) {
	cases := []struct {
		in      string
		want    NamingStrategy
		wantErr bool
	}{
		{"", NamingDescriptive, false},
		{"descriptive", NamingDescriptive, false},
		{"DESCRIPTIVE", NamingDescriptive, false},
		{"  Descriptive  ", NamingDescriptive, false},
		{"sequential", NamingSequential, false},
		{"SEQUENTIAL", NamingSequential, false},
		{"  sequential\n", NamingSequential, false},
		{"unknown", NamingDescriptive, true},
		{"test-N", NamingDescriptive, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseNamingStrategy(c.in)
			if got != c.want {
				t.Fatalf("ParseNamingStrategy(%q)=%q want %q", c.in, got, c.want)
			}
			if (err != nil) != c.wantErr {
				t.Fatalf("ParseNamingStrategy(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			}
		})
	}
}

func TestGenerateName_DescriptiveDisambiguation(t *testing.T) {
	ts := NewWithNaming(zap.NewNop(), "", NamingDescriptive)
	dir := t.TempDir()
	// seed a previously recorded request on the same endpoint
	if err := os.WriteFile(filepath.Join(dir, "get-users-1.yaml"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// and an unrelated file that must not affect numbering
	if err := os.WriteFile(filepath.Join(dir, "post-auth-login-1.yaml"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	tc := httpTC("GET", "http://api.test/users")
	got, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatalf("generateName: %v", err)
	}
	if got != "get-users-2" {
		t.Fatalf("got=%q want=get-users-2", got)
	}

	// first occurrence on a different endpoint starts its own counter
	tc2 := httpTC("GET", "http://api.test/orders/7")
	got, err = ts.generateName(dir, tc2)
	if err != nil {
		t.Fatalf("generateName: %v", err)
	}
	if got != "get-orders-by-id-1" {
		t.Fatalf("got=%q want=get-orders-by-id-1", got)
	}
}

func TestGenerateName_SequentialMode(t *testing.T) {
	ts := NewWithNaming(zap.NewNop(), "", NamingSequential)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test-3.yaml"), []byte("stub"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	tc := httpTC("GET", "http://api.test/users")
	got, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatalf("generateName: %v", err)
	}
	if got != "test-4" {
		t.Fatalf("got=%q want=test-4", got)
	}
}

func TestClaimName_RejectsTraversalPath(t *testing.T) {
	ts := NewWithNaming(zap.NewNop(), "", NamingDescriptive)
	tc := httpTC("GET", "http://api.test/users")
	if _, err := ts.claimName("/tmp/../etc/keploy", tc); err == nil {
		t.Fatalf("expected claimName to reject traversal path")
	}
}

func TestClaimName_Basic(t *testing.T) {
	ts := NewWithNaming(zap.NewNop(), "", NamingDescriptive)
	dir := filepath.Join(t.TempDir(), "tests")
	tc := httpTC("GET", "http://api.test/users")
	name, err := ts.claimName(dir, tc)
	if err != nil {
		t.Fatalf("claimName: %v", err)
	}
	if name != "get-users-1" {
		t.Fatalf("got=%q want=get-users-1", name)
	}
	// placeholder must now exist so the next claim picks a different index
	if _, err := os.Stat(filepath.Join(dir, name+".yaml")); err != nil {
		t.Fatalf("expected placeholder file to exist: %v", err)
	}
	name2, err := ts.claimName(dir, tc)
	if err != nil {
		t.Fatalf("claimName 2: %v", err)
	}
	if name2 != "get-users-2" {
		t.Fatalf("got=%q want=get-users-2", name2)
	}
}

func TestClaimName_ConcurrentCallersGetUniqueNames(t *testing.T) {
	ts := NewWithNaming(zap.NewNop(), "", NamingDescriptive)
	dir := filepath.Join(t.TempDir(), "tests")
	tc := httpTC("GET", "http://api.test/users")

	const workers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	seen := make(map[string]int, workers)
	errs := make([]error, 0)

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			name, err := ts.claimName(dir, tc)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			seen[name]++
		}()
	}
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("claimName errors: %v", errs)
	}
	if len(seen) != workers {
		t.Fatalf("expected %d unique names, got %d: %v", workers, len(seen), seen)
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("name %q claimed %d times", name, count)
		}
	}
}

func TestUpsert_PlaceholderCleanedUpOnError(t *testing.T) {
	// Drive upsert into a failure path by making saveAssets unable
	// to create its assets subdirectory: place a regular file at the
	// path that saveAssets will try to MkdirAll. upsert must then
	// remove the placeholder claimName reserved so the testset
	// directory does not accumulate stale 0-byte files (which would
	// also skew future NextIndexForPrefix scans).
	parent := t.TempDir()
	ts := NewWithNaming(zap.NewNop(), parent, NamingDescriptive)
	testSetID := "leak-check"
	testSetDir := filepath.Join(parent, testSetID)
	tcsDir := filepath.Join(testSetDir, "tests")
	if err := os.MkdirAll(testSetDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Block the assets directory — saveAssets does
	// os.MkdirAll(filepath.Join(ts.TcsPath, testSetID, "assets", tcsName), ...)
	// which fails with ENOTDIR when "assets" is a regular file.
	if err := os.WriteFile(filepath.Join(testSetDir, "assets"), []byte("blocked"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}

	// Request a body large enough that saveAssets actually tries to
	// offload it (>LargeBodyThreshold, which is 1 MiB).
	bigBody := strings.Repeat("x", LargeBodyThreshold+1)
	tc := &models.TestCase{
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    "http://api.test/users",
			Body:   bigBody,
		},
	}
	if _, err := ts.upsert(t.Context(), testSetID, tc); err == nil {
		t.Fatalf("expected upsert to fail when assets dir is blocked")
	}

	entries, err := os.ReadDir(tcsDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			t.Errorf("placeholder leaked: %s", e.Name())
		}
	}
}

func TestGenerateName_NewTestsetDir(t *testing.T) {
	ts := NewWithNaming(zap.NewNop(), "", NamingDescriptive)
	dir := filepath.Join(t.TempDir(), "fresh-testset", "tests")
	tc := httpTC("POST", "http://api.test/auth/login")
	got, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatalf("generateName: %v", err)
	}
	if got != "post-auth-login-1" {
		t.Fatalf("got=%q want=post-auth-login-1", got)
	}
}

func TestIsIDSegment(t *testing.T) {
	ids := []string{
		"1", "42", "0",
		// 64-bit wide integer that overflows int32 — the old
		// strconv.Atoi check would reject it on 32-bit builds and
		// leak the raw ID into the slug.
		"9223372036854775807",
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
