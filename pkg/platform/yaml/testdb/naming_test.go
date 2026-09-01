package testdb

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "go.keploy.io/server/v3/pkg/platform/yaml"
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

func TestUpsert_RejectsTraversalNameComponents(t *testing.T) {
	// upsert receives testSetID and tc.Name as raw components that
	// later flow into filepath.Join. filepath.Join calls Clean which
	// silently collapses ".." segments, so the post-Join ValidatePath
	// can't catch escapes — the rejection has to happen on the raw
	// strings before the join.
	parent := t.TempDir()
	ts := NewWithNaming(zap.NewNop(), parent, NamingDescriptive)
	tc := httpTC("GET", "http://api.test/users")

	badIDs := []string{"../etc", "..", "foo/bar", `foo\bar`, "."}
	for _, id := range badIDs {
		if _, err := ts.upsert(t.Context(), id, tc); err == nil {
			t.Errorf("expected upsert to reject testSetID %q, got nil", id)
		}
	}

	badNames := []string{"../escape", "foo/bar", `foo\bar`, "..", "."}
	for _, name := range badNames {
		named := *tc
		named.Name = name
		if _, err := ts.upsert(t.Context(), "test-set-0", &named); err == nil {
			t.Errorf("expected upsert to reject tc.Name %q, got nil", name)
		}
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

// TestInsertTestCase_PropagatesAutoAssignedName guards the contract that
// callers of InsertTestCase observe the on-disk name on tc.Name when they
// did not pre-name the case themselves. Wrappers (e.g. k8s-proxy's
// testDBWrapper) key per-session dedupe on tc.Name; if the auto-assigned
// name is not propagated back, every nameless capture in a test set
// collapses into one entry and the live recording UI loses visibility
// into the actual capture count until the session stops.
func TestInsertTestCase_PropagatesAutoAssignedName(t *testing.T) {
	parent := t.TempDir()
	ts := NewWithNaming(zap.NewNop(), parent, NamingDescriptive)
	testSetID := "propagation"

	tc1 := httpTC("GET", "http://api.test/users")
	if err := ts.InsertTestCase(t.Context(), tc1, testSetID, false); err != nil {
		t.Fatalf("insert tc1: %v", err)
	}
	if tc1.Name != "get-users-1" {
		t.Fatalf("tc1.Name not propagated; got=%q want=get-users-1", tc1.Name)
	}

	tc2 := httpTC("GET", "http://api.test/users")
	if err := ts.InsertTestCase(t.Context(), tc2, testSetID, false); err != nil {
		t.Fatalf("insert tc2: %v", err)
	}
	if tc2.Name != "get-users-2" {
		t.Fatalf("tc2.Name not propagated; got=%q want=get-users-2", tc2.Name)
	}
	if tc1.Name == tc2.Name {
		t.Fatalf("expected distinct names across two captures, got both %q", tc1.Name)
	}

	// Pre-named cases (e.g. via the Keploy-Test-Name header path) must
	// be left unchanged so callers can still drive deterministic naming.
	tc3 := httpTC("GET", "http://api.test/users")
	tc3.Name = "custom-name"
	if err := ts.InsertTestCase(t.Context(), tc3, testSetID, false); err != nil {
		t.Fatalf("insert tc3: %v", err)
	}
	if tc3.Name != "custom-name" {
		t.Fatalf("explicit tc.Name overwritten; got=%q want=custom-name", tc3.Name)
	}
}

// TestInsertTestCase_DoesNotPropagateOnFailure guards the second half of
// the propagation contract: if upsert fails after claimName but before the
// final rename, tc.Name must be left untouched so the caller does not
// observe a name that no .yaml file backs. Otherwise a retry would either
// skip claimName (because tc.Name is now non-empty and treated as
// caller-supplied) and try to write directly to a name whose placeholder
// the deferred cleanup just removed, or the wrapper's per-session dedupe
// would record an event for a capture that was never persisted.
func TestInsertTestCase_DoesNotPropagateOnFailure(t *testing.T) {
	parent := t.TempDir()
	ts := NewWithNaming(zap.NewNop(), parent, NamingDescriptive)
	testSetID := "no-propagation-on-failure"
	testSetDir := filepath.Join(parent, testSetID)
	if err := os.MkdirAll(testSetDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Block saveAssets the same way TestUpsert_PlaceholderCleanedUpOnError
	// does: a regular file sitting where the assets directory should be
	// makes saveAssets' MkdirAll fail with ENOTDIR, and a >LargeBodyThreshold
	// body forces saveAssets to actually try the write.
	if err := os.WriteFile(filepath.Join(testSetDir, "assets"), []byte("blocked"), 0o644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	bigBody := strings.Repeat("x", LargeBodyThreshold+1)
	tc := &models.TestCase{
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "GET",
			URL:    "http://api.test/users",
			Body:   bigBody,
		},
	}

	if err := ts.InsertTestCase(t.Context(), tc, testSetID, false); err == nil {
		t.Fatalf("expected InsertTestCase to fail when assets dir is blocked")
	}
	if tc.Name != "" {
		t.Fatalf("tc.Name leaked on failure; got=%q want=\"\" (no yaml was persisted)", tc.Name)
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

// The production defect this pins: descriptive names were minted from a disk
// scan alone, and auto-replay deletes every executed test's file — so a later
// capture of the same endpoint re-minted an earlier name, aliasing two
// different requests under one identity (corrupted name-keyed mappings and
// verdicts; same-named files collapsing on download). Numbering must be
// monotonic per (test set, slug) regardless of what happens to the files.
func TestDescriptiveNamesNeverReuseAfterFileDeletion(t *testing.T) {
	dir := t.TempDir()
	ts := NewWithFormatAndNaming(zap.NewNop(), dir, yamlLib.FormatYAML, NamingDescriptive)
	tc := &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://app:8080/k8s-proxy/mappings"}}

	n1, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, n1+".yaml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	n2, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatal(err)
	}

	// Auto-replay deletes executed test files.
	if err := os.Remove(filepath.Join(dir, n1+".yaml")); err != nil {
		t.Fatal(err)
	}

	n3, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatal(err)
	}
	if n1 == n3 || n2 == n3 {
		t.Fatalf("name re-minted after deletion: %s, %s, %s", n1, n2, n3)
	}
	if n1 != "get-k8s-proxy-mappings-1" || n2 != "get-k8s-proxy-mappings-2" || n3 != "get-k8s-proxy-mappings-3" {
		t.Fatalf("unexpected sequence: %s, %s, %s", n1, n2, n3)
	}
}

// First mint on a directory that already holds files continues from the disk
// maximum — the counter seeds from the scan, it does not fight it.
func TestDescriptiveNamesSeedFromExistingFiles(t *testing.T) {
	dir := t.TempDir()
	ts := NewWithFormatAndNaming(zap.NewNop(), dir, yamlLib.FormatYAML, NamingDescriptive)
	for _, n := range []string{"post-pay-1.yaml", "post-pay-7.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tc := &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", URL: "http://app:8080/pay"}}
	n, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatal(err)
	}
	if n != "post-pay-8" {
		t.Fatalf("got %s, want post-pay-8 (continue from disk max)", n)
	}
}

// A recorder adopting an in-flight test set has an EMPTY directory; seeding
// from the server-side maxima must prevent the takeover from re-minting the
// previous owner's names.
func TestSeedSlugIndexesProtectsTakeover(t *testing.T) {
	root := t.TempDir()
	ts := NewWithFormatAndNaming(zap.NewNop(), root, yamlLib.FormatYAML, NamingDescriptive)
	if err := ts.SeedSlugIndexes("set-1", map[string]int{"post-pay": 41, "get-users": 7}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "set-1", "tests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "POST", URL: "http://app:8080/pay"}}
	n, err := ts.generateName(dir, tc)
	if err != nil {
		t.Fatal(err)
	}
	if n != "post-pay-42" {
		t.Fatalf("got %s, want post-pay-42 (seeded from server max 41 despite empty dir)", n)
	}
	// Seeding lower than the live counter is a no-op.
	if err := ts.SeedSlugIndexes("set-1", map[string]int{"post-pay": 3}); err != nil {
		t.Fatal(err)
	}
	n2, _ := ts.generateName(dir, tc)
	if n2 != "post-pay-43" {
		t.Fatalf("got %s, want post-pay-43 (lower seed must not rewind)", n2)
	}
}

// Concurrent mints for the same slug must produce unique names.
func TestDescriptiveNamesConcurrentMintsUnique(t *testing.T) {
	dir := t.TempDir()
	ts := NewWithFormatAndNaming(zap.NewNop(), dir, yamlLib.FormatYAML, NamingDescriptive)
	tc := &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Method: "GET", URL: "http://app:8080/users"}}

	const n = 32
	names := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, err := ts.generateName(dir, tc)
			if err == nil {
				names <- name
			}
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for name := range names {
		if seen[name] {
			t.Fatalf("duplicate name minted concurrently: %s", name)
		}
		seen[name] = true
	}
	if len(seen) != n {
		t.Fatalf("minted %d unique names, want %d", len(seen), n)
	}
}

// A FOREIGN writer (a cloud-replay download, a low seed) can drop names into
// the tests dir that this recorder's counter never issued. The counter climbs
// one EEXIST per claimName attempt, so without a disk resync a gap wider than
// maxNameClaimAttempts exhausts the retry loop and the capture is lost with an
// error — a regression against the pre-counter code, which re-scanned on every
// mint and so always jumped straight past the gap.
func TestClaimName_ForeignWriteGapBeyondRetryBound(t *testing.T) {
	for _, gap := range []int{10, 255, 300, 1000} {
		dir := t.TempDir()
		ts := NewWithNaming(zap.NewNop(), "", NamingDescriptive)
		tc := httpTC("POST", "http://api.test/pay")

		// Take one name so the counter exists and is low (mints post-pay-1).
		first, err := ts.claimName(dir, tc)
		if err != nil {
			t.Fatalf("gap=%d: first claim: %v", gap, err)
		}
		if first != "post-pay-1" {
			t.Fatalf("gap=%d: first = %q, want post-pay-1", gap, first)
		}

		// Foreign writer fills post-pay-2 .. post-pay-(gap+1) behind the
		// counter's back.
		for i := 2; i <= gap+1; i++ {
			p := filepath.Join(dir, "post-pay-"+itoa(i)+"."+ts.Format.FileExtension())
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatalf("gap=%d: seed file: %v", gap, err)
			}
		}

		got, err := ts.claimName(dir, tc)
		if err != nil {
			t.Fatalf("gap=%d: claim after foreign writes failed (the regression): %v", gap, err)
		}
		want := "post-pay-" + itoa(gap+2)
		if got != want {
			t.Fatalf("gap=%d: got %q, want %q (must jump past the whole gap)", gap, got, want)
		}

		// And the counter must keep climbing from there, not rescan-and-stall.
		next, err := ts.claimName(dir, tc)
		if err != nil {
			t.Fatalf("gap=%d: follow-up claim: %v", gap, err)
		}
		if next != "post-pay-"+itoa(gap+3) {
			t.Fatalf("gap=%d: follow-up = %q, want post-pay-%d", gap, next, gap+3)
		}
	}
}

// A seed that lands BELOW the directory's real contents must not strand the
// counter: the first collision resyncs it past what is on disk.
func TestSeedSlugIndexes_LowSeedRecoversViaResync(t *testing.T) {
	parent := t.TempDir()
	setID := "test-set-0"
	dir := filepath.Join(parent, setID, "tests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := NewWithNaming(zap.NewNop(), parent, NamingDescriptive)
	tc := httpTC("POST", "http://api.test/pay")

	// Directory already holds 1..400 (e.g. a downloaded set).
	for i := 1; i <= 400; i++ {
		p := filepath.Join(dir, "post-pay-"+itoa(i)+"."+ts.Format.FileExtension())
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Seed far too low (server knew only about 5).
	if err := ts.SeedSlugIndexes(setID, map[string]int{"post-pay": 5}); err != nil {
		t.Fatal(err)
	}

	got, err := ts.claimName(dir, tc)
	if err != nil {
		t.Fatalf("claim with a low seed over a full directory failed: %v", err)
	}
	if got != "post-pay-401" {
		t.Fatalf("got %q, want post-pay-401", got)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
