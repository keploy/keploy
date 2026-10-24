//go:build unix

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
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// keploy sanitize, desanitize and normalize each name a set's recordings
// before they rewrite any (testdb.BeginSetRewrite). A tests folder that is
// there but cannot be listed stops the command with that error, before it
// writes anything, instead of going on with nothing named. (Each of them
// also reads the folder itself later -- sanitize and desanitize list it,
// normalize reads the set's test cases -- and failed there before this, but
// with an error that did not say the recordings could not be named.)
func TestASetRewriteThatCannotListTheTestsStopsTheCommand(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	ctx := context.Background()
	testdb.SetRekeyer(&fileRekeyer{})
	t.Cleanup(func() { testdb.SetRekeyer(nil) })
	for _, how := range []struct {
		name string
		want error
	}{
		{"permission denied", fs.ErrPermission},
		{"symlink loop", syscall.ELOOP},
	} {
		for _, step := range []string{"sanitize", "desanitize", "normalize"} {
			t.Run(how.name+"/"+step, func(t *testing.T) {
				root := t.TempDir()
				db := testdb.New(zap.NewNop(), root)
				set := filepath.Join(root, "test-set-0")
				tests := filepath.Join(set, "tests")
				tools := &Tools{logger: zap.NewNop(), testDB: db, reportDB: rekeyReportDB{answer: `{"v":"new"}`}, config: &config.Config{Path: root}}
				tc := &models.TestCase{Name: "test-1", Kind: models.HTTP, Version: models.GetVersion(),
					HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://api.test/v1/items", Header: map[string]string{"Authorization": "Bearer ghp_R8xQ2mT5vK9pL3nW7yZ1aB4cD6eF0gH2jK"}},
					HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"v":"old"}`}}
				if err := db.InsertTestCase(ctx, tc, "test-set-0", false); err != nil {
					t.Fatal(err)
				}
				if step == "desanitize" {
					// A sanitized set, so desanitize has work to do.
					if err := tools.SanitizeTestSetDir(ctx, set); err != nil {
						t.Fatal(err)
					}
				}
				switch how.name {
				case "permission denied":
					if err := os.Chmod(tests, 0o300); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = os.Chmod(tests, 0o755) })
					if _, err := os.ReadDir(tests); err == nil {
						t.Skipf("this process (euid %d) can still list a mode-0300 folder, so the permission error cannot be made here; the symlink-loop case covers the same path", os.Geteuid())
					}
				case "symlink loop":
					if err := os.RemoveAll(tests); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(tests, tests); err != nil {
						t.Fatal(err)
					}
				}

				var err error
				switch step {
				case "sanitize":
					err = tools.SanitizeTestSetDir(ctx, set)
				case "desanitize":
					_, err = tools.DesanitizeTestSet("test-set-0", root)
				case "normalize":
					err = tools.normalizeTestSet(ctx, "test-run-0", config.SelectedTests{TestSet: "test-set-0"})
				}
				// The command stops at its own naming of the recordings, the
				// first thing it does, not at a later step that happens to
				// hit the same folder (normalize's desanitize, which names
				// them too, or the reading each command does itself).
				const want = "keploy cannot list the tests of test set test-set-0"
				if !errors.Is(err, how.want) || !strings.HasPrefix(err.Error(), want) {
					t.Fatalf("%s = %v; want %q..., wrapping %v", step, err, want, how.want)
				}
			})
		}
	}
}
