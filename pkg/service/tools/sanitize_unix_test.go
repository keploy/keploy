//go:build !windows

package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gopkg.in/yaml.v3"
)

// withUmask runs the test under the usual 022 umask, so modes are what a
// user's run would get. The umask is the process's: no test in this package
// may run in parallel with these.
func withUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

// requireMode fails unless path has mode want.
func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != want {
		t.Errorf("%s has mode %v, want %v", filepath.Base(path), fi.Mode().Perm(), want)
	}
}

// secret.yaml holds the secrets sanitize took out of the tests: only its owner
// may read it, whether it is new or left more open by an earlier run.
func TestWriteSecretsYAML_OnlyItsOwnerCanReadIt(t *testing.T) {
	withUmask(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.yaml")
	if err := WriteSecretsYAML(path, map[string]string{"TOKEN": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	requireMode(t, path, 0o600)

	if err := os.Chmod(path, 0o755); err != nil { // as an older keploy left it
		t.Fatal(err)
	}
	if err := WriteSecretsYAML(path, map[string]string{"TOKEN": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	requireMode(t, path, 0o600)
}

// A test set is a directory a cloned repository can carry, and sanitize often
// runs under sudo, as recording does: a symlink at secret.yaml or at a test
// file is never followed, for writing or for reading.
func TestSanitizeFiles_DoNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("TOKEN: not-a-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := WriteSecretsYAML(link, map[string]string{"TOKEN": "s3cret"}); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("WriteSecretsYAML through a symlink = %v; want it refused", err)
	}
	if err := SanitizeFileInPlace(link, "test-1", map[string]string{}); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("SanitizeFileInPlace through a symlink = %v; want it refused", err)
	}
	if err := DesanitizeFileInPlace(link, map[string]string{"TOKEN": "s3cret"}); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("DesanitizeFileInPlace through a symlink = %v; want it refused", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "TOKEN: not-a-test\n" {
		t.Errorf("the symlink's target was changed to %q", b)
	}
	requireMode(t, target, 0o644)
}

// A test file sanitize and normalize rewrite keeps its own mode -- a team's
// group-writable 0664 included -- and is not refused for it.
func TestSanitizeAndDesanitize_KeepATestFilesMode(t *testing.T) {
	withUmask(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "test-1.yaml")
	body := "kind: Http\nspec:\n  req:\n    header:\n      Authorization: Bearer abc.def.ghi\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o664); err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{}
	if err := SanitizeFileInPlace(path, "test-1", secrets); err != nil {
		t.Fatal(err)
	}
	requireMode(t, path, 0o664)
	if err := DesanitizeFileInPlace(path, secrets); err != nil {
		t.Fatal(err)
	}
	requireMode(t, path, 0o664)
}

// testSet makes keploy/test-set-0 under a fresh directory, with tests (name to
// contents) and, if secrets is not nil, a secret.yaml; it returns the keploy
// directory and the test set's.
func testSet(t *testing.T, tests map[string]string, secrets map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	set := filepath.Join(root, "test-set-0")
	if err := os.MkdirAll(filepath.Join(set, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range tests {
		if err := os.WriteFile(filepath.Join(set, "tests", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if secrets != nil {
		b, _ := yaml.Marshal(secrets)
		if err := os.WriteFile(filepath.Join(set, "secret.yaml"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, set
}

const placeholderTest = "kind: Http\nspec:\n  req:\n    header:\n      Authorization: '{{string .secret.TOKEN }}'\n"

// normalize puts the secrets back into the tests and removes secret.yaml.
func TestDesanitizeTestSet_RestoresTheSecrets(t *testing.T) {
	root, set := testSet(t, map[string]string{"test-1.yaml": placeholderTest}, map[string]string{"TOKEN": "Bearer s3cret"})
	done, err := (&Tools{logger: zap.NewNop()}).DesanitizeTestSet("test-set-0", root)
	if err != nil || !done {
		t.Fatalf("DesanitizeTestSet = %v, %v", done, err)
	}
	b, _ := os.ReadFile(filepath.Join(set, "tests", "test-1.yaml"))
	if !strings.Contains(string(b), "Bearer s3cret") || strings.Contains(string(b), ".secret.TOKEN") {
		t.Errorf("the test was not restored: %s", b)
	}
	if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secret.yaml is still there: %v", err)
	}
}

// A test file that is a symlink stops normalize before it rewrites anything:
// a test set is restored whole or not at all.
func TestDesanitizeTestSet_RefusesASymlinkedTestBeforeChangingAnything(t *testing.T) {
	root, set := testSet(t, map[string]string{"test-1.yaml": placeholderTest}, map[string]string{"TOKEN": "Bearer s3cret"})
	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(target, []byte(placeholderTest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(set, "tests", "test-2.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Tools{logger: zap.NewNop()}).DesanitizeTestSet("test-set-0", root); err == nil {
		t.Fatal("DesanitizeTestSet rewrote a test set holding a symlinked test")
	}
	for _, f := range []string{filepath.Join(set, "tests", "test-1.yaml"), target} {
		if b, _ := os.ReadFile(f); string(b) != placeholderTest {
			t.Errorf("%s was rewritten: %s", filepath.Base(f), b)
		}
	}
	if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); err != nil {
		t.Errorf("secret.yaml was removed: %v", err)
	}
}

// A test file that is a symlink stops sanitize before it rewrites anything or
// writes secret.yaml, rather than leaving that test's secrets in it while the
// set reads as sanitized.
func TestSanitizeTestSetDir_RefusesASymlinkedTestBeforeChangingAnything(t *testing.T) {
	const raw = "kind: Http\nspec:\n  req:\n    header:\n      Authorization: Bearer abc.def.ghi\n"
	_, set := testSet(t, map[string]string{"test-1.yaml": raw}, nil)
	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(target, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(set, "tests", "test-2.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := (&Tools{logger: zap.NewNop()}).SanitizeTestSetDir(context.Background(), set); err == nil {
		t.Fatal("SanitizeTestSetDir sanitized a test set holding a symlinked test")
	}
	for _, f := range []string{filepath.Join(set, "tests", "test-1.yaml"), target} {
		if b, _ := os.ReadFile(f); string(b) != raw {
			t.Errorf("%s was rewritten: %s", filepath.Base(f), b)
		}
	}
	if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secret.yaml was written: %v", err)
	}
}

// A tests directory, or a test set, that is a symlink is not followed: tests
// -> /etc/netplan would have had root rewrite every *.yaml there.
func TestSanitizeAndNormalize_RefuseASymlinkedTestsDirOrTestSet(t *testing.T) {
	elsewhere := t.TempDir()
	victim := filepath.Join(elsewhere, "config.yaml")
	if err := os.WriteFile(victim, []byte(placeholderTest), 0o644); err != nil {
		t.Fatal(err)
	}
	tools := &Tools{logger: zap.NewNop()}

	root, set := testSet(t, nil, map[string]string{"TOKEN": "Bearer s3cret"})
	if err := os.Remove(filepath.Join(set, "tests")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(set, "tests")); err != nil {
		t.Fatal(err)
	}
	if err := tools.SanitizeTestSetDir(context.Background(), set); err == nil {
		t.Error("SanitizeTestSetDir followed a symlinked tests directory")
	}
	if _, err := tools.DesanitizeTestSet("test-set-0", root); err == nil {
		t.Error("DesanitizeTestSet followed a symlinked tests directory")
	}

	if b, _ := os.ReadFile(victim); string(b) != placeholderTest {
		t.Errorf("a file behind the symlinked tests directory was rewritten: %s", b)
	}

	// A test set that is a symlink to a real-looking test set elsewhere.
	const raw = "kind: Http\nspec:\n  req:\n    header:\n      Authorization: Bearer abc.def.ghi\n"
	_, other := testSet(t, map[string]string{"test-1.yaml": raw}, map[string]string{"TOKEN": "Bearer s3cret"})
	linkRoot := t.TempDir()
	if err := os.Symlink(other, filepath.Join(linkRoot, "test-set-0")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(other, "secret.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := tools.SanitizeTestSetDir(context.Background(), filepath.Join(linkRoot, "test-set-0")); err == nil {
		t.Error("SanitizeTestSetDir followed a symlinked test set")
	}
	if b, _ := os.ReadFile(filepath.Join(other, "tests", "test-1.yaml")); string(b) != raw {
		t.Errorf("a test behind the symlinked test set was rewritten: %s", b)
	}
	if _, err := os.Lstat(filepath.Join(other, "secret.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secret.yaml was written behind the symlinked test set: %v", err)
	}
}

// The secrets being restored are not written to the log.
func TestDesanitizeTestSet_DoesNotLogTheSecrets(t *testing.T) {
	root := t.TempDir()
	set := filepath.Join(root, "test-set-0")
	if err := os.MkdirAll(filepath.Join(set, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := yaml.Marshal(map[string]string{"TOKEN": "s3cret-value"})
	if err := os.WriteFile(filepath.Join(set, "secret.yaml"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(set, "tests", "test-1.yaml"), []byte("kind: Http\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	if _, err := (&Tools{logger: zap.New(core)}).DesanitizeTestSet("test-set-0", root); err != nil {
		t.Fatal(err)
	}
	for _, e := range logs.All() {
		if line := fmt.Sprint(e.Message, e.ContextMap()); strings.Contains(line, "s3cret-value") {
			t.Errorf("a secret value was logged: %s", line)
		}
	}
}

// Desanitizing does not read a secret.yaml that is a symlink: under sudo it
// would read whatever the link points at (/etc/shadow), and a YAML parse error
// quotes what it could not parse.
func TestDesanitizeTestSet_DoesNotReadASymlinkedSecretsFile(t *testing.T) {
	root := t.TempDir()
	set := filepath.Join(root, "test-set-0")
	if err := os.MkdirAll(filepath.Join(set, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "shadow")
	if err := os.WriteFile(target, []byte("root:PRIVATE-HASH:19000:0:99999:7:::\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(set, "secret.yaml")); err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.DebugLevel)
	_, err := (&Tools{logger: zap.New(core)}).DesanitizeTestSet("test-set-0", root)
	if err == nil {
		t.Fatal("DesanitizeTestSet read a symlinked secret.yaml")
	}
	if strings.Contains(err.Error(), "PRIVATE-HASH") {
		t.Errorf("the symlink's target leaked into the error: %v", err)
	}
	for _, e := range logs.All() {
		if line := fmt.Sprint(e.Message, e.ContextMap()); strings.Contains(line, "PRIVATE-HASH") {
			t.Errorf("the symlink's target leaked into the log: %s", line)
		}
	}
}

// Under sudo, the secret.yaml root creates belongs to the user who ran sudo,
// as keploy's other outputs do, while a test file root rewrites keeps its
// owner. Needs root.
func TestSanitizeUnderSudo_OnlyNewFilesGoToTheInvokingUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, as sanitize under sudo runs")
	}
	t.Setenv("SUDO_USER", "nobody")
	t.Setenv("SUDO_UID", "65534")
	t.Setenv("SUDO_GID", "65534")
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secret.yaml")
	if err := WriteSecretsYAML(secrets, map[string]string{"TOKEN": "s3cret"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(secrets); fi.Sys().(*syscall.Stat_t).Uid != 65534 {
		t.Errorf("root's new secret.yaml belongs to uid %d, want the sudo user 65534", fi.Sys().(*syscall.Stat_t).Uid)
	}
	test := filepath.Join(dir, "test-1.yaml")
	if err := os.WriteFile(test, []byte("kind: Http\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SanitizeFileInPlace(test, "test-1", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(test); fi.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Errorf("rewriting a root-owned test file gave it to uid %d", fi.Sys().(*syscall.Stat_t).Uid)
	}
}

// A test file sanitize cannot rewrite is named in an error, while secret.yaml
// is still written: the files already sanitized need it to be restored.
func TestSanitizeTestSetDir_NamesATestItCouldNotSanitize(t *testing.T) {
	_, set := testSet(t, map[string]string{
		"test-1.yaml": "kind: Http\nspec:\n  req:\n    header:\n      Authorization: Bearer abc.def.ghi\n",
		"test-2.yaml": "kind: [unclosed\n",
	}, nil)
	err := (&Tools{logger: zap.NewNop()}).SanitizeTestSetDir(context.Background(), set)
	if err == nil || !strings.Contains(err.Error(), "test-2.yaml") {
		t.Fatalf("SanitizeTestSetDir = %v; want an error naming test-2.yaml", err)
	}
	if _, err := os.Lstat(filepath.Join(set, "secret.yaml")); err != nil {
		t.Errorf("secret.yaml was not written for the files that were sanitized: %v", err)
	}
}

// A run that leaves a test set unsanitized fails, so a pipeline can tell.
func TestSanitize_FailsWhenATestSetIsLeftUnsanitized(t *testing.T) {
	root := t.TempDir()
	keploy := filepath.Join(root, "keploy")
	set := filepath.Join(keploy, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(set, "tests")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	tools := &Tools{logger: zap.NewNop(), config: &config.Config{Test: config.Test{SelectedTests: map[string][]string{"test-set-0": nil}}}}
	if err := tools.Sanitize(context.Background()); err == nil || !strings.Contains(err.Error(), "test-set-0") {
		t.Errorf("Sanitize = %v; want an error naming test-set-0", err)
	}
}
