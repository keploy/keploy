//go:build !windows

package tools

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const projectSecretTest = "version: api.keploy.io/v1beta1\nkind: Http\nname: test-1\nspec:\n  req:\n    method: GET\n    url: http://localhost:8080/a\n    header:\n      Authorization: Bearer ghp_0123456789abcdefghijABCDEFGHIJ012345\n  resp:\n    status_code: 200\n"

// linkedKeployFolder makes a project whose keploy folder is a link to at,
// and a recorded test set, test-set-0, where the link leads: its one test is
// body and, if secrets is not nil, it has a secret.yaml. The run's directory
// is elsewhere. It returns the keploy folder and the test set where it lies.
func linkedKeployFolder(t *testing.T, at func(project string) string, body string, secrets map[string]string) (keployDir, set string) {
	t.Helper()
	project := t.TempDir()
	target := at(project)
	resolved := target
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(project, resolved)
	}
	set = filepath.Join(resolved, "test-set-0")
	if err := os.MkdirAll(filepath.Join(set, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(set, "tests", "test-1.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if secrets != nil {
		b, _ := yaml.Marshal(secrets)
		if err := os.WriteFile(filepath.Join(set, "secret.yaml"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keployDir = filepath.Join(project, "keploy")
	if err := os.Symlink(target, keployDir); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	return keployDir, set
}

// keploy sanitize and keploy normalize rewrite a test set's tests, and write,
// read and remove its secret.yaml, only inside the project: a keploy folder
// the repository carries as a link out of the project (or one -p names), by
// an absolute target or by "..", is refused with why, and nothing where it
// leads is read, rewritten, written or removed. They followed it: sanitize
// rewrote the tests of the folder it led to and wrote a secret.yaml there,
// and normalize put the secrets back into them and removed it.
func TestSanitizeAndNormalizeStayInsideTheProject(t *testing.T) {
	outside := func(t *testing.T, relative bool) func(string) string {
		dir := t.TempDir()
		return func(project string) string {
			if !relative {
				return dir
			}
			rel, err := filepath.Rel(project, dir)
			if err != nil {
				t.Fatal(err)
			}
			return rel
		}
	}
	for _, relative := range []bool{false, true} {
		name := "absolute link"
		if relative {
			name = "relative link"
		}
		t.Run("sanitize/"+name, func(t *testing.T) {
			keployDir, set := linkedKeployFolder(t, outside(t, relative), projectSecretTest, nil)
			tools := &Tools{logger: zap.NewNop(), testDB: testdb.New(zap.NewNop(), keployDir), config: &config.Config{Path: keployDir}}
			if err := tools.Sanitize(context.Background()); err == nil {
				t.Error("keploy sanitize rewrote a test set through a keploy folder that links out of the project")
			}
			err := tools.SanitizeTestSetDir(context.Background(), filepath.Join(keployDir, "test-set-0"))
			if err == nil || !strings.Contains(err.Error(), "keploy") {
				t.Errorf("SanitizeTestSetDir through a keploy folder that links out = %v; want it refused, naming the folder", err)
			}
			if b, _ := os.ReadFile(filepath.Join(set, "tests", "test-1.yaml")); string(b) != projectSecretTest {
				t.Errorf("the test where the link leads was rewritten: %s", b)
			}
			if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a secret.yaml was written where the link leads: %v", err)
			}
			// The files one by one, as the exported calls take them.
			test := filepath.Join(keployDir, "test-set-0", "tests", "test-1.yaml")
			if err := SanitizeFileInPlace(test, "test-1", map[string]string{}); err == nil {
				t.Error("SanitizeFileInPlace rewrote a test through a keploy folder that links out")
			}
			if err := DesanitizeFileInPlace(test, map[string]string{"TOKEN": "x"}); err == nil {
				t.Error("DesanitizeFileInPlace rewrote a test through a keploy folder that links out")
			}
			if err := WriteSecretsYAML(filepath.Join(keployDir, "test-set-0", "secret.yaml"), map[string]string{"TOKEN": "x"}); err == nil {
				t.Error("WriteSecretsYAML wrote through a keploy folder that links out")
			}
			if b, _ := os.ReadFile(filepath.Join(set, "tests", "test-1.yaml")); string(b) != projectSecretTest {
				t.Errorf("the test where the link leads was rewritten: %s", b)
			}
			if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a secret.yaml was written where the link leads: %v", err)
			}
		})
		// A set where the link leads that is sanitized already is refused
		// too, not passed over as sanitized: the run fails.
		t.Run("sanitize, a sanitized set where the link leads/"+name, func(t *testing.T) {
			keployDir, set := linkedKeployFolder(t, outside(t, relative), projectSecretTest, map[string]string{"TOKEN": "Bearer s3cret"})
			tools := &Tools{logger: zap.NewNop(), testDB: testdb.New(zap.NewNop(), keployDir), config: &config.Config{Path: keployDir}}
			if err := tools.Sanitize(context.Background()); err == nil {
				t.Error("keploy sanitize passed over a test set through a keploy folder that links out, as sanitized")
			}
			if b, _ := os.ReadFile(filepath.Join(set, "tests", "test-1.yaml")); string(b) != projectSecretTest {
				t.Errorf("the test where the link leads was rewritten: %s", b)
			}
		})
		t.Run("normalize/"+name, func(t *testing.T) {
			keployDir, set := linkedKeployFolder(t, outside(t, relative), placeholderTest, map[string]string{"TOKEN": "Bearer s3cret"})
			tools := &Tools{logger: zap.NewNop()}
			done, err := tools.DesanitizeTestSet("test-set-0", keployDir)
			if err == nil || done {
				t.Errorf("DesanitizeTestSet through a keploy folder that links out = %v, %v; want it refused", done, err)
			} else if !strings.Contains(err.Error(), "only inside the project") {
				t.Errorf("DesanitizeTestSet through a keploy folder that links out = %v; want it to say why", err)
			}
			if b, _ := os.ReadFile(filepath.Join(set, "tests", "test-1.yaml")); string(b) != placeholderTest {
				t.Errorf("the test where the link leads was rewritten: %s", b)
			}
			if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); err != nil {
				t.Errorf("the secret.yaml where the link leads was removed: %v", err)
			}
		})
	}

	// A keploy folder that links to a folder inside the project is the
	// project's: it is sanitized and restored as before.
	inside := func(project string) string {
		if err := os.Mkdir(filepath.Join(project, "recordings"), 0o755); err != nil {
			t.Fatal(err)
		}
		return "recordings"
	}
	t.Run("a keploy folder that links inside the project", func(t *testing.T) {
		keployDir, set := linkedKeployFolder(t, inside, projectSecretTest, nil)
		tools := &Tools{logger: zap.NewNop(), testDB: testdb.New(zap.NewNop(), keployDir), config: &config.Config{Path: keployDir}}
		if err := tools.Sanitize(context.Background()); err != nil {
			t.Fatalf("Sanitize: %v", err)
		}
		test := filepath.Join(set, "tests", "test-1.yaml")
		if b, _ := os.ReadFile(test); strings.Contains(string(b), "ghp_0123456789") {
			t.Fatalf("the test was not sanitized: %s", b)
		}
		requireMode(t, filepath.Join(set, "secret.yaml"), 0o600)
		// Sanitized, it is passed over by the next run: its secret.yaml is
		// not written again.
		secrets, _ := os.ReadFile(filepath.Join(set, "secret.yaml"))
		if err := tools.Sanitize(context.Background()); err != nil {
			t.Fatalf("Sanitize of a sanitized set: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(set, "secret.yaml")); string(b) != string(secrets) {
			t.Errorf("the secret.yaml of a sanitized set was written again: %q, was %q", b, secrets)
		}
		if done, err := tools.DesanitizeTestSet("test-set-0", keployDir); err != nil || !done {
			t.Fatalf("DesanitizeTestSet = %v, %v", done, err)
		}
		if b, _ := os.ReadFile(test); !strings.Contains(string(b), "ghp_0123456789") {
			t.Errorf("the test was not restored: %s", b)
		}
		if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("secret.yaml is still there: %v", err)
		}
	})
}

// Inside the keploy folder, a test set, its tests folder and a test file are
// still never followed when they are links, even to a folder or file in the
// keploy folder: each is refused, and nothing is rewritten.
func TestSanitizeAndNormalizeRefuseALinkInsideTheKeployFolder(t *testing.T) {
	tools := &Tools{logger: zap.NewNop()}
	t.Run("a test set that links to another", func(t *testing.T) {
		root, set := testSet(t, map[string]string{"test-1.yaml": projectSecretTest}, nil)
		if err := os.Symlink("test-set-0", filepath.Join(root, "test-set-1")); err != nil {
			t.Fatal(err)
		}
		if err := tools.SanitizeTestSetDir(context.Background(), filepath.Join(root, "test-set-1")); err == nil {
			t.Error("SanitizeTestSetDir followed a test set that links to another")
		}
		if b, _ := os.ReadFile(filepath.Join(set, "tests", "test-1.yaml")); string(b) != projectSecretTest {
			t.Errorf("the test of the set the link leads to was rewritten: %s", b)
		}
		if err := os.WriteFile(filepath.Join(set, "secret.yaml"), []byte("TOKEN: x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := tools.DesanitizeTestSet("test-set-1", root); err == nil {
			t.Error("DesanitizeTestSet followed a test set that links to another")
		}
		if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); err != nil {
			t.Errorf("the secret.yaml of the set the link leads to was removed: %v", err)
		}
	})
	t.Run("a tests folder that links to a folder in the set", func(t *testing.T) {
		_, set := testSet(t, map[string]string{"test-1.yaml": projectSecretTest}, nil)
		if err := os.Rename(filepath.Join(set, "tests"), filepath.Join(set, "real")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("real", filepath.Join(set, "tests")); err != nil {
			t.Fatal(err)
		}
		if err := tools.SanitizeTestSetDir(context.Background(), set); err == nil || !strings.Contains(err.Error(), "is a symbolic link") {
			t.Errorf("SanitizeTestSetDir through a tests folder that links to a folder in the set = %v; want it refused as a link", err)
		}
		if b, _ := os.ReadFile(filepath.Join(set, "real", "test-1.yaml")); string(b) != projectSecretTest {
			t.Errorf("the test the link leads to was rewritten: %s", b)
		}
		if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a secret.yaml was written: %v", err)
		}
	})
	t.Run("a test file that links to a file beside it", func(t *testing.T) {
		_, set := testSet(t, map[string]string{"test-1.yaml": projectSecretTest}, nil)
		link := filepath.Join(set, "tests", "test-2.yaml")
		if err := os.Symlink("test-1.yaml", link); err != nil {
			t.Fatal(err)
		}
		if err := SanitizeFileInPlace(link, "test-2", map[string]string{}); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("SanitizeFileInPlace through a link beside it = %v; want it refused", err)
		}
		if err := DesanitizeFileInPlace(link, map[string]string{"TOKEN": "x"}); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("DesanitizeFileInPlace through a link beside it = %v; want it refused", err)
		}
		if err := WriteSecretsYAML(link, map[string]string{"TOKEN": "x"}); !errors.Is(err, syscall.ELOOP) {
			t.Errorf("WriteSecretsYAML through a link beside it = %v; want it refused", err)
		}
		if b, _ := os.ReadFile(filepath.Join(set, "tests", "test-1.yaml")); string(b) != projectSecretTest {
			t.Errorf("the test the link leads to was rewritten: %s", b)
		}
	})
}

// A test set normalize is asked for that is not there is not found.
func TestDesanitizeTestSet_AMissingSetIsNotFound(t *testing.T) {
	root, _ := testSet(t, nil, nil)
	_, err := (&Tools{logger: zap.NewNop()}).DesanitizeTestSet("test-set-9", root)
	if err == nil || !strings.Contains(err.Error(), "test set directory not found") {
		t.Errorf("DesanitizeTestSet of a missing set = %v; want it not found", err)
	}
}

// A test set's file is opened by its name in its folder: a name that is a
// path, or "..", is refused, and nothing outside the folder is read or
// written.
func TestATestSetFileIsOnlyANameInItsFolder(t *testing.T) {
	top := t.TempDir()
	folder := filepath.Join(top, "tests")
	if err := os.MkdirAll(filepath.Join(folder, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(top, "outside.yaml"), filepath.Join(folder, "sub", "x.yaml")} {
		if err := os.WriteFile(f, []byte("kept\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.OpenRoot(folder)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	for _, name := range []string{"../outside.yaml", "sub/x.yaml", "..", "../new.yaml"} {
		if b, err := readTestSetFile(dir, name); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("readTestSetFile(%q) = %q, %v; want it refused", name, b, err)
		}
		if err := writeTestSetFile(dir, name, []byte("changed\n"), testFileMode, false); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("writeTestSetFile(%q) = %v; want it refused", name, err)
		}
	}
	for _, f := range []string{filepath.Join(top, "outside.yaml"), filepath.Join(folder, "sub", "x.yaml")} {
		if b, _ := os.ReadFile(f); string(b) != "kept\n" {
			t.Errorf("%s was changed: %q", f, b)
		}
	}
	if _, err := os.Lstat(filepath.Join(top, "new.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file was made outside the folder: %v", err)
	}
	// A name in the folder is read and written.
	if err := writeTestSetFile(dir, "test-1.yaml", []byte("a\n"), testFileMode, false); err != nil {
		t.Fatal(err)
	}
	if b, err := readTestSetFile(dir, "test-1.yaml"); err != nil || string(b) != "a\n" {
		t.Errorf("readTestSetFile(test-1.yaml) = %q, %v", b, err)
	}
}
