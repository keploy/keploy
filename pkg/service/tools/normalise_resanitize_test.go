package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// reportDBNoReport has a test run but no report of any set in it, as a
// keploy folder that was never tested, or a mistyped --test-run, has.
type reportDBNoReport struct{}

func (reportDBNoReport) GetAllTestRunIDs(context.Context) ([]string, error) {
	return []string{"test-run-0"}, nil
}

func (reportDBNoReport) GetReport(context.Context, string, string) (*models.TestReport, error) {
	return nil, errors.New("no report for this test set")
}

// keploy normalize desanitizes a sanitized test set, normalizes it, and
// sanitizes it again. When the normalize step fails, the set must not be left
// desanitized: its secrets are back in the test files and secret.yaml is
// gone, so the next `git add` publishes them.
func TestNormalizeTestSetSanitizesAgainWhenNormalizingFails(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"report missing", context.Background()},
		// A command stopped part way (Ctrl-C) must not leave the secrets out.
		{"command cancelled", cancelled},
	} {
		t.Run(tc.name, func(t *testing.T) { normalizeFailsAndSanitizesAgain(t, tc.ctx) })
	}
}

func normalizeFailsAndSanitizesAgain(t *testing.T, ctx context.Context) {
	t.Helper()
	// Split so the literal is not itself flagged by a secret scanner.
	secret := "ghp_" + "R7kP2mXq9LdVw4TzYb8NcHf3JsAe6UgQ1oIx"

	keployDir := t.TempDir()
	setDir := filepath.Join(keployDir, "test-set-0")
	testsDir := filepath.Join(setDir, "tests")
	require.NoError(t, os.MkdirAll(testsDir, 0o755))
	testFile := filepath.Join(testsDir, "test-1.yaml")
	plain := "version: api.keploy.io/v1beta1\nkind: Http\nname: test-1\nspec:\n  req:\n    method: GET\n    url: http://example.com/user\n    header:\n      Authorization: " + secret + "\n  resp:\n    status_code: 200\n"
	require.NoError(t, os.WriteFile(testFile, []byte(plain), 0o644))

	tools := &Tools{
		logger:   zap.NewNop(),
		testDB:   &normaliseTestDB{placementTestDB: placementTestDB{written: map[string]models.TestCase{}}},
		reportDB: reportDBNoReport{},
		config:   &config.Config{Path: keployDir},
	}

	// The set as `keploy sanitize` leaves it.
	require.NoError(t, tools.SanitizeTestSetDir(context.Background(), setDir))
	sanitized, err := os.ReadFile(testFile)
	require.NoError(t, err)
	require.NotContains(t, string(sanitized), secret, "sanitize left the secret in the test file")
	require.FileExists(t, filepath.Join(setDir, "secret.yaml"))

	err = tools.normalizeTestSet(ctx, "test-run-0", config.SelectedTests{TestSet: "test-set-0"})
	require.Error(t, err, "a missing report is an error")

	after, readErr := os.ReadFile(testFile)
	require.NoError(t, readErr)
	if strings.Contains(string(after), secret) {
		t.Errorf("the failed normalize left the secret in the test file:\n%s", after)
	}
	if _, statErr := os.Stat(filepath.Join(setDir, "secret.yaml")); statErr != nil {
		t.Errorf("the failed normalize left the set with no secret.yaml: %v", statErr)
	}
}
