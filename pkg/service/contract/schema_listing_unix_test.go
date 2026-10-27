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

// writeSchema writes a schema titled title at dir/name.yaml.
func writeSchema(t *testing.T, dir, name, title string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := strings.Replace(schemaDoc, "title: test-1", "title: "+title, 1)
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// A test set, mock set or service folder that is a symbolic link to a
// folder is read through the link, as the sets beside it are. keploy used to
// pass over every link without a word, and validate checked nothing against
// the set behind it. A regular file beside the sets, or a link to one, is
// still no set.
func TestGetAllSchemas_ASymlinkedSetIsRead(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	tests := filepath.Join(root, "keploy", "schema", "tests")
	writeSchema(t, filepath.Join(tests, "test-set-0"), "test-1", "test-1")
	writeSchema(t, filepath.Join(root, "shared", "tests-set"), "test-2", "test-2")
	symlink(t, filepath.Join(root, "shared", "tests-set"), filepath.Join(tests, "test-set-1"))
	if err := os.WriteFile(filepath.Join(tests, "README.md"), []byte("not a set\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A link to a regular file is no set either.
	symlink(t, filepath.Join(tests, "README.md"), filepath.Join(tests, "notes-link"))
	mocks := filepath.Join(root, "Download", "Mocks")
	writeSchema(t, filepath.Join(mocks, "svc", "test-set-0"), "schema", "svc-0")
	writeSchema(t, filepath.Join(root, "shared", "mock-set"), "schema", "svc-1")
	symlink(t, filepath.Join(root, "shared", "mock-set"), filepath.Join(mocks, "svc", "test-set-1"))
	writeSchema(t, filepath.Join(root, "shared", "service", "test-set-0"), "schema", "linked-0")
	symlink(t, filepath.Join(root, "shared", "service"), filepath.Join(mocks, "linked-svc"))
	if err := os.WriteFile(filepath.Join(mocks, "svc", "notes.txt"), []byte("not a set\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &contract{logger: zap.NewNop(), openAPIDB: openapidb.New(zap.NewNop(), filepath.Join(root, "keploy", "schema"))}
	got, err := s.GetAllTestsSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["test-set-0"]["test-1"] == nil || got["test-set-1"]["test-2"] == nil {
		t.Fatalf("GetAllTestsSchema = %v; want test-set-0 (test-1) and the linked test-set-1 (test-2)", got)
	}
	mapping, err := s.GetAllDownloadedMocksSchemas(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var sets []string
	for _, m := range mapping {
		title := ""
		if len(m.Mocks) == 1 {
			title = m.Mocks[0].Info.Title
		}
		sets = append(sets, m.Service+"/"+m.TestSetID+"="+title)
	}
	want := "linked-svc/test-set-0=linked-0 svc/test-set-0=svc-0 svc/test-set-1=svc-1"
	if strings.Join(sets, " ") != want {
		t.Fatalf("GetAllDownloadedMocksSchemas read %v; want %s", sets, want)
	}
}

// A symbolic link among the sets that cannot be followed (its target is
// missing, it loops, or keploy may not look at it) is an error: it may well
// be a set, and validate would otherwise check nothing against it.
func TestGetAllSchemas_ASymlinkThatCannotBeFollowedIsAnError(t *testing.T) {
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
		link string // under the working directory
		loop bool   // a link to itself, else to a path that does not exist
		read func(*contract) error
		want error
		// noSearch, when set, makes the link's folder one that can be
		// listed but not searched, so the link cannot be followed.
		noSearch bool
	}{
		{"test set, target missing", filepath.Join("keploy", "schema", "tests", "test-set-1"), false, readTests, fs.ErrNotExist, false},
		{"test set, loop", filepath.Join("keploy", "schema", "tests", "test-set-1"), true, readTests, syscall.ELOOP, false},
		{"mock set, target missing", filepath.Join("Download", "Mocks", "svc", "test-set-1"), false, readMocks, fs.ErrNotExist, false},
		{"mock set, loop", filepath.Join("Download", "Mocks", "svc", "test-set-1"), true, readMocks, syscall.ELOOP, false},
		{"service, target missing", filepath.Join("Download", "Mocks", "linked-svc"), false, readMocks, fs.ErrNotExist, false},
		{"service, loop", filepath.Join("Download", "Mocks", "linked-svc"), true, readMocks, syscall.ELOOP, false},
		{"test set, permission denied", filepath.Join("keploy", "schema", "tests", "test-set-1"), false, readTests, fs.ErrPermission, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Chdir(root)
			writeSchema(t, filepath.Join(root, "keploy", "schema", "tests", "test-set-0"), "test-1", "test-1")
			writeSchema(t, filepath.Join(root, "Download", "Mocks", "svc", "test-set-0"), "schema", "svc-0")
			link := filepath.Join(root, tc.link)
			target := filepath.Join(root, "gone")
			if tc.loop {
				target = link
			}
			if tc.noSearch {
				// A link to a set that exists, in a folder it cannot be
				// followed from.
				target = filepath.Join(root, "keploy", "schema", "tests", "test-set-0")
			}
			symlink(t, target, link)
			if tc.noSearch {
				if why := noSearch(t, filepath.Dir(link), link); why != "" {
					t.Skip(why)
				}
				if _, err := os.ReadDir(filepath.Dir(link)); err != nil {
					// A filesystem that reports no entry types makes
					// os.ReadDir look at each entry itself, so the listing
					// fails before the link is followed.
					t.Skip("this filesystem cannot list a mode-0600 folder without looking into it: " + err.Error())
				}
			}
			s := &contract{logger: zap.NewNop(), openAPIDB: openapidb.New(zap.NewNop(), filepath.Join(root, "keploy", "schema"))}
			err := tc.read(s)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "cannot follow a symbolic link") {
				t.Fatalf("read = %v; want the link refused, wrapping %v", err, tc.want)
			}
		})
	}
}
