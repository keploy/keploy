package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

func TestDeleteMappingsForSetRemovesYamlAndJSON(t *testing.T) {
	dir := t.TempDir()
	setDir := filepath.Join(dir, "stale-map-demo")
	require.NoError(t, os.MkdirAll(setDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(setDir, "mappings.yaml"), []byte("stale-yaml"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(setDir, "mappings.json"), []byte("stale-json"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(setDir, "mocks.yaml"), []byte("keep mocks"), 0644))

	db := NewWithFormat(zap.NewNop(), dir, "mappings", yaml.FormatYAML)
	require.NoError(t, db.DeleteMappingsForSet(context.Background(), "stale-map-demo"))

	require.NoFileExists(t, filepath.Join(setDir, "mappings.yaml"))
	require.NoFileExists(t, filepath.Join(setDir, "mappings.json"))
	require.FileExists(t, filepath.Join(setDir, "mocks.yaml"), "mapping deletion must not remove mock artifacts")
}

func TestDeleteMappingsForSetMissingFilesIsNoop(t *testing.T) {
	db := New(zap.NewNop(), t.TempDir(), "mappings")
	require.NoError(t, db.DeleteMappingsForSet(context.Background(), "missing-set"))
}

func TestDeleteMappingsForSetRejectsUnsafeTestSetID(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "victim.yaml")
	require.NoError(t, os.WriteFile(outside, []byte("do not delete"), 0644))

	db := New(zap.NewNop(), filepath.Join(dir, "mappings"), "mappings")
	require.Error(t, db.DeleteMappingsForSet(context.Background(), "../victim"))
	require.FileExists(t, outside)
}
