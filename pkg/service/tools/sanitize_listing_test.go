package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A test set whose tests entry is there but is not a directory cannot be
// sanitized: keploy cannot tell what tests the set holds. keploy sanitize used
// to read it as a set with no tests and succeed, leaving whatever the set
// holds unsanitized; with the proven switch on it already failed (the set's
// recordings could not be named), and it now fails the same way with the
// switch off. Desanitize says what is wrong instead of "tests directory not
// found".
func TestSanitize_ATestsEntryThatIsNotADirectoryIsAnError(t *testing.T) {
	ctx := context.Background()
	for _, on := range []bool{false, true} {
		for _, step := range []string{"sanitize", "desanitize"} {
			name := step + "/switch off"
			if on {
				name = step + "/switch on"
			}
			t.Run(name, func(t *testing.T) {
				if on {
					t.Setenv(provenswitch.Env, "1")
					testdb.SetRekeyer(&fileRekeyer{})
					t.Cleanup(func() { testdb.SetRekeyer(nil) })
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				root := t.TempDir()
				set := filepath.Join(root, "test-set-0")
				if err := os.MkdirAll(set, 0o755); err != nil {
					t.Fatal(err)
				}
				tests := filepath.Join(set, "tests")
				body := []byte("kind: Http\nspec:\n  req:\n    header:\n      Authorization: Bearer ghp_R8xQ2mT5vK9pL3nW7yZ1aB4cD6eF0gH2jK\n")
				if err := os.WriteFile(tests, body, 0o644); err != nil {
					t.Fatal(err)
				}
				if step == "desanitize" {
					// A set that looks sanitized, so desanitize looks at its tests.
					if err := os.WriteFile(filepath.Join(set, "secret.yaml"), []byte("{}\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				tools := &Tools{logger: zap.NewNop()}
				var err error
				if step == "sanitize" {
					err = tools.SanitizeTestSetDir(ctx, set)
				} else {
					_, err = tools.DesanitizeTestSet("test-set-0", root)
				}
				if err == nil {
					t.Fatalf("%s of a set whose tests entry is a regular file = nil; want an error", step)
				}
				if !on && !strings.Contains(err.Error(), "cannot tell what tests the set holds") {
					t.Fatalf("%s = %v; want the error that the tests entry is not a directory", step, err)
				}
				if !on && !errors.Is(err, syscall.ENOTDIR) {
					t.Fatalf("%s = %v; want it to wrap ENOTDIR", step, err)
				}
				if got, rerr := os.ReadFile(tests); rerr != nil || string(got) != string(body) {
					t.Fatalf("the tests entry was changed (%v): %q", rerr, got)
				}
				if step == "sanitize" {
					if _, serr := os.Lstat(filepath.Join(set, "secret.yaml")); !os.IsNotExist(serr) {
						t.Fatalf("sanitize wrote a secret.yaml for a set it could not read (%v)", serr)
					}
				}
			})
		}
	}
}

// A test set with no tests directory still has no tests: sanitize does
// nothing and succeeds.
func TestSanitize_ASetWithoutATestsDirectoryHasNothingToSanitize(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	root := t.TempDir()
	set := filepath.Join(root, "test-set-0")
	if err := os.MkdirAll(set, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&Tools{logger: zap.NewNop()}).SanitizeTestSetDir(context.Background(), set); err != nil {
		t.Fatalf("SanitizeTestSetDir of a set without tests = %v; want nil", err)
	}
	if _, err := os.Stat(filepath.Join(set, "secret.yaml")); !os.IsNotExist(err) {
		t.Fatalf("a set without tests got a secret.yaml (%v)", err)
	}
}
