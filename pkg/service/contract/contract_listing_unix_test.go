//go:build unix

package contract

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// recordingMockDB says which test sets DownloadMocks asked for, as
// "<service>/<test set>", and has no mocks.
type recordingMockDB struct{ asked []string }

func (r *recordingMockDB) GetHTTPMocks(_ context.Context, testSetID, mockPath, _ string) ([]*models.HTTPDoc, error) {
	r.asked = append(r.asked, filepath.Base(filepath.Dir(mockPath))+"/"+testSetID)
	return nil, nil
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// virtualCPR makes root/app the working directory, with the other services'
// checkouts in root/VirtualCPR, and returns root.
func virtualCPR(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "app"))
	mkdir(t, filepath.Join(root, "VirtualCPR"))
	t.Chdir(filepath.Join(root, "app"))
	return root
}

// cprService lays out, at dir, a service's checkout that maps "me" and has
// the test sets sets.
func cprService(t *testing.T, dir, self string, sets ...string) {
	t.Helper()
	schema := filepath.Join(dir, "keploy", "schema")
	mkdir(t, filepath.Join(schema, "mocks", "me"))
	mkdir(t, filepath.Join(schema, "tests"))
	if err := os.WriteFile(filepath.Join(schema, "serviceMappings.yaml"), []byte("self: "+self+"\nservicesMapping:\n  me:\n    - http://"+self+"/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, set := range sets {
		writeSchema(t, filepath.Join(dir, "keploy", set, "tests"), "test-1", set)
	}
}

// keploy contract download reads the other services' checkouts in
// VirtualCPR. A service folder, or a test set in one, that is a symbolic link
// to a folder is read through the link, like the folders beside it; it used
// to be passed over without a word, so validate then checked nothing against
// it. A link not named like a test set, in a service's keploy folder, is
// still no test set.
func TestDownload_ASymlinkedServiceOrTestSetIsRead(t *testing.T) {
	t.Run("mocks", func(t *testing.T) {
		root := virtualCPR(t)
		cpr := filepath.Join(root, "VirtualCPR")
		cprService(t, filepath.Join(cpr, "svc"), "svc", "test-set-0")
		mkdir(t, filepath.Join(root, "shared", "mset"))
		symlink(t, filepath.Join(root, "shared", "mset"), filepath.Join(cpr, "svc", "keploy", "test-set-1"))
		symlink(t, filepath.Join(root, "gone"), filepath.Join(cpr, "svc", "keploy", "notes"))
		cprService(t, filepath.Join(root, "shared", "other"), "other", "test-set-0")
		symlink(t, filepath.Join(root, "shared", "other"), filepath.Join(cpr, "other"))
		db := &recordingMockDB{}
		s := &contract{logger: zap.NewNop(), mockDB: db, config: &config.Config{Contract: config.Contract{Mappings: config.Mappings{Self: "me"}}}}
		if err := s.DownloadMocks(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(db.asked, " "), "other/test-set-0 svc/test-set-0 svc/test-set-1"; got != want {
			t.Fatalf("DownloadMocks read %q; want %q", got, want)
		}
	})
	t.Run("tests", func(t *testing.T) {
		root := virtualCPR(t)
		cpr := filepath.Join(root, "VirtualCPR")
		cprService(t, filepath.Join(cpr, "svc"), "svc", "test-set-0")
		cprService(t, filepath.Join(root, "shared"), "unused", "test-set-1")
		symlink(t, filepath.Join(root, "shared", "keploy", "test-set-1"), filepath.Join(cpr, "svc", "keploy", "test-set-1"))
		symlink(t, filepath.Join(root, "gone"), filepath.Join(cpr, "svc", "keploy", "notes"))
		s := &contract{logger: zap.NewNop(), config: &config.Config{Contract: config.Contract{Mappings: config.Mappings{ServicesMapping: map[string][]string{"svc": {"http://svc/x"}}}}}}
		if err := s.DownloadTests(""); err != nil {
			t.Fatal(err)
		}
		for _, set := range []string{"test-set-0", "test-set-1"} {
			if _, err := os.Stat(filepath.Join(root, "app", "Download", "Tests", "svc", "schema", set, "test-1.yaml")); err != nil {
				t.Errorf("DownloadTests did not copy %s's tests: %v", set, err)
			}
		}
	})
}

// A service folder, or a test set in one, that is a symbolic link keploy
// cannot follow stops the download with that error, instead of being passed
// over.
func TestDownload_ASymlinkThatCannotBeFollowedIsAnError(t *testing.T) {
	t.Run("mocks, service", func(t *testing.T) {
		root := virtualCPR(t)
		cprService(t, filepath.Join(root, "VirtualCPR", "svc"), "svc", "test-set-0")
		symlink(t, filepath.Join(root, "gone"), filepath.Join(root, "VirtualCPR", "broken"))
		s := &contract{logger: zap.NewNop(), mockDB: &recordingMockDB{}, config: &config.Config{Contract: config.Contract{Mappings: config.Mappings{Self: "me"}}}}
		if err := s.DownloadMocks(context.Background(), ""); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "cannot follow a symbolic link") {
			t.Fatalf("DownloadMocks = %v; want the link refused", err)
		}
	})
	t.Run("mocks, test set", func(t *testing.T) {
		root := virtualCPR(t)
		cprService(t, filepath.Join(root, "VirtualCPR", "svc"), "svc", "test-set-0")
		symlink(t, filepath.Join(root, "gone"), filepath.Join(root, "VirtualCPR", "svc", "keploy", "test-set-1"))
		s := &contract{logger: zap.NewNop(), mockDB: &recordingMockDB{}, config: &config.Config{Contract: config.Contract{Mappings: config.Mappings{Self: "me"}}}}
		if err := s.DownloadMocks(context.Background(), ""); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "cannot follow a symbolic link") {
			t.Fatalf("DownloadMocks = %v; want the link refused", err)
		}
	})
	t.Run("tests, test set", func(t *testing.T) {
		root := virtualCPR(t)
		cprService(t, filepath.Join(root, "VirtualCPR", "svc"), "svc", "test-set-0")
		symlink(t, filepath.Join(root, "gone"), filepath.Join(root, "VirtualCPR", "svc", "keploy", "test-set-1"))
		s := &contract{logger: zap.NewNop(), config: &config.Config{Contract: config.Contract{Mappings: config.Mappings{ServicesMapping: map[string][]string{"svc": {"http://svc/x"}}}}}}
		if err := s.DownloadTests(""); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "cannot follow a symbolic link") {
			t.Fatalf("DownloadTests = %v; want the link refused", err)
		}
	})
}
