//go:build unix

package contract

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/yaml/openapidb"
	"go.uber.org/zap"
)

const schemaDoc = "openapi: 3.0.0\ninfo:\n  title: test-1\n  version: \"1\"\npaths: {}\n"

// keploy contract validate (consumer mode) reads every test set's schemas
// (GetAllTestsSchema) and every downloaded mock set's
// (GetAllDownloadedMocksSchemas). A set it lists but cannot look into is an
// error, and validate stops on it; such a set used to be read as one with no
// schemas, and validated against nothing.
func TestGetAllSchemas_ASetThatCannotBeLookedIntoIsAnError(t *testing.T) {
	readTests := func(s *contract) error {
		_, err := s.GetAllTestsSchema(context.Background())
		return err
	}
	readMocks := func(s *contract) error {
		_, err := s.GetAllDownloadedMocksSchemas(context.Background())
		return err
	}
	for _, tc := range []struct {
		name string
		set  string // the set folder, under the working directory
		// how makes the set unreadable. It returns why the case is
		// skipped when the process can still read it (root), else "".
		how  func(t *testing.T, set string) string
		read func(*contract) error
		want error
		// says is how the reader names the set it failed on: the error
		// comes from that set, not from listing the folder above it.
		says string
	}{
		{"tests, permission denied", filepath.Join("keploy", "schema", "tests", "test-set-0"), unsearchableParent, readTests, fs.ErrPermission, "failed to get test cases for testSetID test-set-0"},
		{"mocks, permission denied", filepath.Join("Download", "Mocks", "svc", "test-set-0"), unsearchable, readMocks, fs.ErrPermission, "failed to get HTTP mocks for mockSetID test-set-0"},
		{"mocks, symlink loop", filepath.Join("Download", "Mocks", "svc", "test-set-0"), loopedSchema, readMocks, syscall.ELOOP, "failed to get HTTP mocks for mockSetID test-set-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			set := filepath.Join(root, tc.set)
			if err := os.MkdirAll(set, 0o755); err != nil {
				t.Fatal(err)
			}
			if why := tc.how(t, set); why != "" {
				t.Skip(why)
			}
			s := &contract{logger: zap.NewNop(), openAPIDB: openapidb.New(zap.NewNop(), filepath.Join(root, "keploy", "schema"))}
			err := tc.read(s)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("read = %v; want %q, wrapping %v", err, tc.says, tc.want)
			}
		})
	}
}

// unsearchableParent writes a schema into set and makes set's parent folder
// one that can be listed but not searched (mode 0600).
func unsearchableParent(t *testing.T, set string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(set, "test-1.yaml"), []byte(schemaDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	if why := noSearch(t, filepath.Dir(set), set); why != "" {
		return why
	}
	if _, err := os.ReadDir(filepath.Dir(set)); err != nil {
		// A filesystem that reports no entry types makes os.ReadDir look
		// at each entry itself, so the listing above the set fails first.
		return "this filesystem cannot list a mode-0600 folder without looking into it, so the set itself is never read: " + err.Error()
	}
	return ""
}

// unsearchable writes set's schema.yaml and makes set one that can be listed
// but not searched (mode 0600).
func unsearchable(t *testing.T, set string) string {
	t.Helper()
	file := filepath.Join(set, "schema.yaml")
	if err := os.WriteFile(file, []byte(schemaDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	return noSearch(t, set, file)
}

// loopedSchema makes set's schema.yaml a symlink to itself; this one fails
// for every user, root included.
func loopedSchema(t *testing.T, set string) string {
	t.Helper()
	file := filepath.Join(set, "schema.yaml")
	if err := os.Symlink(file, file); err != nil {
		t.Fatal(err)
	}
	return ""
}

// noSearch makes dir a folder that can be listed but not searched (mode
// 0600). It returns why the case is skipped when this process can still look
// at inside (a path in dir), as root can; else "".
func noSearch(t *testing.T, dir, inside string) string {
	t.Helper()
	if err := os.Chmod(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.Stat(inside); err == nil {
		return fmt.Sprintf("this process (euid %d) can still search a mode-0600 folder, so the permission error cannot be made here, and this case is not tested as root", os.Geteuid())
	}
	return ""
}
