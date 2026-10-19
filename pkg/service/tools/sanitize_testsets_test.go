package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// sanitizeSet is a recorded test set, test-set-0, with one test that carries
// a secret, in a keploy folder at root/keploy; the run's directory is
// elsewhere.
func sanitizeSet(t *testing.T) (keployDir, test string) {
	t.Helper()
	root := t.TempDir()
	keployDir = filepath.Join(root, "keploy")
	tests := filepath.Join(keployDir, "test-set-0", "tests")
	if err := os.MkdirAll(tests, 0o755); err != nil {
		t.Fatal(err)
	}
	test = filepath.Join(tests, "test-1.yaml")
	body := "version: api.keploy.io/v1beta1\nkind: Http\nname: test-1\nspec:\n  req:\n    method: GET\n    url: http://localhost:8080/a\n    header:\n      Authorization: Bearer ghp_0123456789abcdefghijABCDEFGHIJ012345\n  resp:\n    status_code: 200\n"
	if err := os.WriteFile(test, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	return keployDir, test
}

// keploy sanitize -p <dir> sanitizes the test sets of the keploy folder -p
// names: it looked for them in the run's own directory, skipped each it did
// not find there, and exited 0 with every secret left in place.
func TestSanitizeReadsTheKeployFolderItWasGiven(t *testing.T) {
	keployDir, test := sanitizeSet(t)
	tools := &Tools{logger: zap.NewNop(), testDB: testdb.New(zap.NewNop(), keployDir), config: &config.Config{Path: keployDir, Test: config.Test{SelectedTests: map[string][]string{"test-set-0": nil}}}}
	if err := tools.Sanitize(context.Background()); err != nil {
		t.Fatalf("Sanitize: %v", err)
	}
	if b, _ := os.ReadFile(test); strings.Contains(string(b), "ghp_0123456789") {
		t.Fatal("the test set of the keploy folder -p names was not sanitized")
	}
}

// A test set -t names that is not recorded is refused, naming it, before
// anything is sanitized (P189), with the proven switch on or off: it was
// skipped with an error line, and the run exited 0, leaving the set the user
// meant unsanitized.
func TestSanitizeRefusesATestSetThatIsNotRecorded(t *testing.T) {
	for _, switchOn := range []string{"0", "1"} {
		t.Run("KEPLOY_PROVEN="+switchOn, func(t *testing.T) {
			t.Setenv(provenswitch.Env, switchOn)
			keployDir, test := sanitizeSet(t)
			tools := &Tools{logger: zap.NewNop(), testDB: testdb.New(zap.NewNop(), keployDir), config: &config.Config{Path: keployDir, Test: config.Test{
				SelectedTests: map[string][]string{"test-set-0": nil, "test-set-9": nil},
			}}}
			err := tools.Sanitize(context.Background())
			if err == nil || !strings.Contains(err.Error(), "test-set-9") {
				t.Fatalf("Sanitize = %v; want test-set-9 refused by name", err)
			}
			if b, _ := os.ReadFile(test); !strings.Contains(string(b), "ghp_0123456789") {
				t.Fatal("a refused run sanitized a test set")
			}
		})
	}
}

// A folder of the keploy folder that is no test set (reports, schema), the
// keploy folder itself, and a path out of it are not recorded test sets:
// each passed for one, was "sanitized" with nothing in it, or another
// folder's tests were, and the run exited 0.
func TestSanitizeRefusesAFolderThatIsNoTestSet(t *testing.T) {
	for _, name := range []string{"reports", "schema", ".", "../outside"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "0")
			keployDir, _ := sanitizeSet(t)
			for _, dir := range []string{"reports", "schema", "../outside/tests"} {
				if err := os.MkdirAll(filepath.Join(keployDir, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			tools := &Tools{logger: zap.NewNop(), testDB: testdb.New(zap.NewNop(), keployDir), config: &config.Config{Path: keployDir, Test: config.Test{
				SelectedTests: map[string][]string{name: nil}}}}
			if err := tools.Sanitize(context.Background()); err == nil || !strings.Contains(err.Error(), "not recorded") {
				t.Fatalf("Sanitize -t %q = %v; want it refused as not recorded", name, err)
			}
		})
	}
}
