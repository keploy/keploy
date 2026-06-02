package testset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type registryState struct {
	Mock string `yaml:"mock,omitempty"`
	App  string `yaml:"app,omitempty"`
	User string `yaml:"user,omitempty"`
}

type extendedTestSet struct {
	models.TestSet `yaml:",inline"`
	Registry       *registryState `yaml:"mockRegistry,omitempty"`
}

func (ts *extendedTestSet) WithoutSecrets() *extendedTestSet {
	if ts == nil {
		return &extendedTestSet{}
	}

	testSetCopy := *ts
	testSetCopy.Secret = nil
	return &testSetCopy
}

func TestReadInjectsSecretsWithoutConfigForBaseTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"
	testSetDir := filepath.Join(root, testSetID)
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "secret.yaml"), []byte("token: abc123\n"), 0o644))

	db := New[*models.TestSet](zap.NewNop(), root)
	cfg, err := db.Read(context.Background(), testSetID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, map[string]interface{}{"token": "abc123"}, cfg.Secret)
	require.Empty(t, cfg.Template)
	require.Empty(t, cfg.Metadata)
}

func TestReadUsesZeroExtendedTypeOnMalformedConfig(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"
	testSetDir := filepath.Join(root, testSetID)
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "config.yaml"), []byte("template: [\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "secret.yaml"), []byte("token: abc123\n"), 0o644))

	db := New[*extendedTestSet](zap.NewNop(), root)
	cfg, err := db.Read(context.Background(), testSetID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, map[string]interface{}{"token": "abc123"}, cfg.Secret)
	require.Nil(t, cfg.Registry)
	require.Empty(t, cfg.Template)
	require.Empty(t, cfg.Metadata)
}

func TestReadSampleConfigCompatibilityForExtendedTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"
	testSetDir := filepath.Join(root, testSetID)
	require.NoError(t, os.MkdirAll(testSetDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(testSetDir, "config.yaml"), []byte(`
preScript: ''
postScript: ''
appCommand: ''
template: {}
mockRegistry:
  mock: e227caac693a155a767bf231f1814b237de77c2b39a5ee3c2c1eb65daefbd89b
  app: user-management-service
metadata:
  name: student_login
  scenario-description: 'VST-1: New student registration'
  secret_versions:
    student_login: c08bfa5a-31df-4479-9ad7-4a25e07ee117
`), 0o644))

	db := New[*extendedTestSet](zap.NewNop(), root)
	cfg, err := db.Read(context.Background(), testSetID)
	require.NoError(t, err)
	require.NotNil(t, cfg)
	require.Equal(t, "", cfg.PreScript)
	require.Equal(t, "", cfg.PostScript)
	require.Equal(t, "", cfg.AppCommand)
	require.Empty(t, cfg.Template)
	require.NotNil(t, cfg.Registry)
	require.Equal(t, "e227caac693a155a767bf231f1814b237de77c2b39a5ee3c2c1eb65daefbd89b", cfg.Registry.Mock)
	require.Equal(t, "user-management-service", cfg.Registry.App)
	require.Equal(t, "", cfg.Registry.User)
	require.Equal(t, "student_login", cfg.Metadata["name"])
	require.Equal(t, "VST-1: New student registration", cfg.Metadata["scenario-description"])
}

func TestWriteStripsSecretsForBaseTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"

	db := New[*models.TestSet](zap.NewNop(), root)
	cfg := &models.TestSet{
		PreScript: "echo hi",
		Secret: map[string]interface{}{
			"token": "abc123",
		},
		Metadata: map[string]interface{}{
			"name": "student_login",
		},
	}

	require.NoError(t, db.Write(context.Background(), testSetID, cfg))

	raw, err := os.ReadFile(filepath.Join(root, testSetID, "config.yaml"))
	require.NoError(t, err)
	content := string(raw)
	require.NotContains(t, content, "secret:")
	require.Contains(t, content, "preScript: echo hi")
	require.Contains(t, content, "metadata:")
}

func TestWriteStripsSecretsForExtendedTestSet(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	testSetID := "test-set-1"

	db := New[*extendedTestSet](zap.NewNop(), root)
	cfg := &extendedTestSet{
		TestSet: models.TestSet{
			Template: map[string]interface{}{},
			Secret: map[string]interface{}{
				"token": "abc123",
			},
			Metadata: map[string]interface{}{
				"name": "student_login",
			},
		},
		Registry: &registryState{
			Mock: "hash-value",
			App:  "user-management-service",
			User: "demo-user",
		},
	}

	require.NoError(t, db.Write(context.Background(), testSetID, cfg))

	raw, err := os.ReadFile(filepath.Join(root, testSetID, "config.yaml"))
	require.NoError(t, err)
	content := string(raw)
	require.NotContains(t, content, "secret:")
	require.True(t, strings.Contains(content, "template: {}") || strings.Contains(content, "template:\n    {}"))
	require.Contains(t, content, "mockRegistry:")
	require.Contains(t, content, "mock: hash-value")
	require.Contains(t, content, "app: user-management-service")
	require.Contains(t, content, "user: demo-user")
}
