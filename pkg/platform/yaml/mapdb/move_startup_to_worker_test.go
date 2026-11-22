package mapdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMoveToWorkerFromStartup(t *testing.T) {
	dir := t.TempDir()
	set := "test-set-0"
	require.NoError(t, os.MkdirAll(filepath.Join(dir, set), 0o755))
	initial := `version: api.keploy.io/v1beta1
kind: TestMocksMapping
test_set_id: test-set-0
startup:
    - name: mock-hello
      kind: Mongo
    - name: mock-create-idx
      kind: Mongo
tests:
    - id: .::TestAppCRUD
      dir: .
      mocks:
        - name: mock-t1
          kind: Mongo
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, set, "mappings.yaml"), []byte(initial), 0o644))

	db := New(zap.NewNop(), dir, "")
	moved, err := db.MoveToWorker(context.Background(), set, []models.MockEntry{{
		Name:     "mock-create-idx",
		Kind:     "Mongo",
		Place:    "",
		Rule:     "startup-indexes@12345678",
		Series:   "startup-indexes/-/1",
		Once:     true,
		Optional: false,
		Reason:   "startup-capable background write required once per run",
	}})
	require.NoError(t, err)
	require.Equal(t, []string{"mock-create-idx"}, moved)

	mp, err := db.GetMapping(context.Background(), set)
	require.NoError(t, err)
	require.Len(t, mp.Startup, 1, "only the moved startup entry should leave startup:")
	require.Equal(t, "mock-hello", mp.Startup[0].Name)
	require.Len(t, mp.Worker, 1, "moved startup entry should be added to worker:")
	require.Equal(t, "mock-create-idx", mp.Worker[0].Name)
	require.Equal(t, "", mp.Worker[0].Owner, "startup entry keeps empty Owner")
	require.Equal(t, "", mp.Worker[0].Place, "startup entry placed at set start (empty Place)")
	require.Equal(t, "startup-indexes@12345678", mp.Worker[0].Rule)
	require.Equal(t, "startup-indexes/-/1", mp.Worker[0].Series)
	require.False(t, mp.Worker[0].Optional, "fail-closed write guard keeps Optional false")
	require.False(t, mp.Worker[0].Read)
}
