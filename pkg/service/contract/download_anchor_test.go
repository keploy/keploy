package contract

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// mkfile writes body at path, making its folders.
func mkfile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mklink makes at a link to target, making at's folder.
func mklink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

// requireAbsent fails when path is there.
func requireAbsent(t *testing.T, what, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: %s is there (%v)", what, path, err)
	}
}

// contractIn is a contract service for a project at base/project, the working
// directory, with mappings, whose other services are checked out at
// base/VirtualCPR.
func contractIn(t *testing.T, base string, mappings config.Mappings) *contract {
	t.Helper()
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	cfg := &config.Config{Path: filepath.Join(project, "keploy")}
	cfg.Contract.Mappings = mappings
	return New(zap.NewNop(), nil, nil, nil, cfg).(*contract)
}

// keploy/schema is a folder of the recording: one that a cloned repository
// carries as a link out of keploy takes no serviceMappings.yaml, and the
// write is refused with why.
func TestServiceMappingsAreWrittenOnlyInsideTheRecording(t *testing.T) {
	base := t.TempDir()
	keploy := filepath.Join(base, "project", "keploy")
	if err := os.MkdirAll(filepath.Join(base, "outside"), 0o755); err != nil {
		t.Fatal(err)
	}
	mklink(t, filepath.Join("..", "..", "outside"), filepath.Join(keploy, "schema"))
	err := saveServiceMappings(config.Mappings{Self: "svc", ServicesMapping: map[string][]string{"a": {"b"}}}, keploy, filepath.Join(keploy, "schema"))
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("saveServiceMappings: %v, want it refused as outside the recording", err)
	}
	requireAbsent(t, "serviceMappings.yaml written through keploy/schema", filepath.Join(base, "outside", "serviceMappings.yaml"))

	// The keploy folder itself, as an absolute link out of the project: the
	// recording is opened from the project, so the write is refused too.
	base = t.TempDir()
	keploy = filepath.Join(base, "project", "keploy")
	if err := os.MkdirAll(filepath.Join(base, "outside", "schema"), 0o755); err != nil {
		t.Fatal(err)
	}
	mklink(t, filepath.Join(base, "outside"), keploy)
	err = saveServiceMappings(config.Mappings{Self: "svc", ServicesMapping: map[string][]string{"a": {"b"}}}, keploy, filepath.Join(keploy, "schema"))
	if err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("saveServiceMappings through a keploy link out of the project: %v, want it refused", err)
	}
	requireAbsent(t, "serviceMappings.yaml written through the keploy link", filepath.Join(base, "outside", "schema", "serviceMappings.yaml"))
}

// A download reads another service's files only inside that service's
// checkout: a test set's tests folder that links out of it is refused, and
// nothing it leads to is copied into Download.
func TestDownloadTestsCopiesNothingFromOutsideTheCheckout(t *testing.T) {
	base := t.TempDir()
	mkfile(t, filepath.Join(base, "outside", "id_rsa"), "SECRET-OUTSIDE")
	svc := filepath.Join(base, "VirtualCPR", "svc", "keploy")
	if err := os.MkdirAll(filepath.Join(svc, "schema", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	mklink(t, filepath.Join("..", "..", "..", "..", "outside"), filepath.Join(svc, "test-set-0", "tests"))
	s := contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"svc": {"http://svc"}}})
	err := s.DownloadTests("")
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("DownloadTests: %v, want the linked tests folder refused", err)
	}
	requireAbsent(t, "DownloadTests copied the file outside", filepath.Join(base, "project", "Download", "Tests", "svc", "schema", "test-set-0", "id_rsa"))
	requireAbsent(t, "DownloadTests made a folder for the linked set", filepath.Join(base, "project", "Download", "Tests", "svc", "schema", "test-set-0"))

	// The service's schema folder as a link out of its checkout: its tests
	// are not copied from where it leads.
	base = t.TempDir()
	mkfile(t, filepath.Join(base, "outside", "tests", "id_rsa"), "SECRET-OUTSIDE")
	mklink(t, filepath.Join("..", "..", "..", "outside"), filepath.Join(base, "VirtualCPR", "svc", "keploy", "schema"))
	s = contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"svc": {"http://svc"}}})
	if err := s.DownloadTests(""); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("DownloadTests with a schema folder that links out of the checkout: %v, want it refused", err)
	}
	requireAbsent(t, "DownloadTests copied the file outside", filepath.Join(base, "project", "Download", "Tests", "svc", "id_rsa"))

	// A service named in keploy.yml is one folder: one that climbs out is
	// refused before anything is read or made.
	base = t.TempDir()
	s = contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{filepath.Join("..", "planted"): {"http://svc"}}})
	if err := s.DownloadTests(""); err == nil || !strings.Contains(err.Error(), "must name one folder") {
		t.Fatalf("DownloadTests for a service that names a path: %v, want it refused", err)
	}
	requireAbsent(t, "a service name that climbs out", filepath.Join(base, "project", "Download", "planted"))
}

// The name of the folder a mock download writes into comes from the other
// service's serviceMappings.yaml: one that is not a single folder name is
// refused, with why, and nothing is written; and the file itself is read only
// inside that service's checkout.
func TestDownloadMocksWritesOnlyUnderDownload(t *testing.T) {
	ctx := context.Background()
	for _, self := range []string{filepath.Join("..", "..", "..", "planted"), "..", ".", "a/b", `a\b`, ""} {
		base := t.TempDir()
		other := filepath.Join(base, "VirtualCPR", "other", "keploy", "schema")
		mkfile(t, filepath.Join(other, "serviceMappings.yaml"), "self: '"+self+"'\nservicesMapping:\n  me:\n    - http://me\n")
		mkfile(t, filepath.Join(other, "mocks", "me", "test-set-0", "mocks.yaml"), "openapi: 3.0.0\n")
		s := contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"other": {"http://other"}}})
		err := s.DownloadMocks(ctx, "")
		if err == nil || !strings.Contains(err.Error(), "must name one folder") {
			t.Fatalf("self %q: DownloadMocks: %v, want it refused", self, err)
		}
		requireAbsent(t, "a self that climbs out", filepath.Join(base, "planted"))
		entries, err := os.ReadDir(filepath.Join(base, "project", "Download", "Mocks"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("self %q: Download/Mocks holds %v (%v), want nothing", self, entries, err)
		}
	}

	base := t.TempDir()
	mkfile(t, filepath.Join(base, "outside", "serviceMappings.yaml"), "self: other\nservicesMapping:\n  me:\n    - http://me\n")
	mklink(t, filepath.Join("..", "..", "..", "..", "outside", "serviceMappings.yaml"), filepath.Join(base, "VirtualCPR", "other", "keploy", "schema", "serviceMappings.yaml"))
	s := contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"other": {"http://other"}}})
	if err := s.DownloadMocks(ctx, ""); err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("DownloadMocks with a linked serviceMappings.yaml: %v, want it refused", err)
	}

	// The other service's keploy folder as a link out of its checkout: its
	// serviceMappings.yaml is not read from where the link leads.
	base = t.TempDir()
	// Read, it would name a folder no download takes, and be refused for that.
	mkfile(t, filepath.Join(base, "outside", "schema", "serviceMappings.yaml"), "self: x/y\nservicesMapping:\n  me:\n    - http://me\n")
	mkfile(t, filepath.Join(base, "outside", "schema", "mocks", "me", "test-set-0", "mocks.yaml"), "openapi: 3.0.0\n")
	mklink(t, filepath.Join("..", "..", "outside"), filepath.Join(base, "VirtualCPR", "other", "keploy"))
	s = contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"other": {"http://other"}}})
	if err := s.DownloadMocks(ctx, ""); err == nil || !strings.Contains(err.Error(), "only inside") || strings.Contains(err.Error(), "must name one folder") {
		t.Fatalf("DownloadMocks with a keploy folder that links out of the checkout: %v, want its serviceMappings.yaml refused unread", err)
	}
	requireAbsent(t, "mocks copied from outside the checkout", filepath.Join(base, "project", "Download", "Mocks", "other"))

	// The other service's mocks folder as a link out of its checkout: no
	// mock is copied from where it leads.
	base = t.TempDir()
	mkfile(t, filepath.Join(base, "VirtualCPR", "other", "keploy", "schema", "serviceMappings.yaml"), "self: other\nservicesMapping:\n  me:\n    - http://me\n")
	mkfile(t, filepath.Join(base, "outside", "me", "test-set-0", "mocks.yaml"), "openapi: 3.0.0\n")
	mklink(t, filepath.Join("..", "..", "..", "..", "outside"), filepath.Join(base, "VirtualCPR", "other", "keploy", "schema", "mocks"))
	s = contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"other": {"http://other"}}})
	if err := s.DownloadMocks(ctx, ""); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("DownloadMocks with a mocks folder that links out of the checkout: %v, want it refused", err)
	}
	requireAbsent(t, "a mock copied from outside the checkout", filepath.Join(base, "project", "Download", "Mocks", "other", "test-set-0"))
}

// The Download folder keploy fills is made in the project only: one the
// repository carries as a link out of the project is refused, with why, and
// no folder, not even an empty one, is made where it leads.
func TestDownloadMakesNoFolderOutsideTheProject(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	s := contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"other": {"http://other"}}})
	mklink(t, filepath.Join("..", "outside"), filepath.Join(base, "project", "Download"))
	if err := s.DownloadTests(""); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("DownloadTests with Download linking out: %v, want it refused", err)
	}
	if err := s.DownloadMocks(ctx, ""); err == nil || !strings.Contains(err.Error(), "only inside") {
		t.Fatalf("DownloadMocks with Download linking out: %v, want it refused", err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("made %d entries where Download leads", len(entries))
	}
}

// Download/Tests is the folder a download writes into, the project's own: a
// link in it that stays inside it is followed, as every link that stays
// inside its folder is. Here one service's schema folder leads to another
// folder in Download/Tests, and its HTTP tests are copied there.
func TestDownloadTestsFollowsALinkThatStaysInsideDownloadTests(t *testing.T) {
	base := t.TempDir()
	mkfile(t, filepath.Join(base, "VirtualCPR", "svc", "keploy", "test-set-0", "tests", "test-1.yaml"), "kind: Http\n")
	if err := os.MkdirAll(filepath.Join(base, "VirtualCPR", "svc", "keploy", "schema", "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := contractIn(t, base, config.Mappings{Self: "me", ServicesMapping: map[string][]string{"svc": {"http://svc"}}})
	tests := filepath.Join(base, "project", "Download", "Tests")
	if err := os.MkdirAll(filepath.Join(tests, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	mklink(t, filepath.Join("..", "shared"), filepath.Join(tests, "svc", "schema"))
	if err := s.DownloadTests(""); err != nil {
		t.Fatalf("DownloadTests through a link inside Download/Tests: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(tests, "shared", "test-set-0", "test-1.yaml"))
	if err != nil || string(got) != "kind: Http\n" {
		t.Fatalf("the HTTP test where the link leads = %q, %v; want it copied there", got, err)
	}
}
