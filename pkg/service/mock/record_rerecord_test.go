package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.uber.org/zap"
)

func mockAt(name string, at time.Time) *models.Mock {
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{ReqTimestampMock: at}}
}

// A re-record starts from an empty mapping: the old tests would otherwise
// keep pointing at mock names that now belong to different calls.
func TestReRecordDropsTheOldMappings(t *testing.T) {
	mapDB := mapdb.New(zap.NewNop(), t.TempDir(), "")

	first := newRunnerInstr(t)
	first.windows = []models.ScopeWindow{{Name: "TestOld", Start: ts(10), End: ts(20)}}
	first.mocks = []*models.Mock{mockAt("mock-0", ts(15))}
	require.NoError(t, recordSet(t, first, mapDB, nil))

	second := newRunnerInstr(t)
	second.windows = []models.ScopeWindow{{Name: "TestNew", Start: ts(10), End: ts(20)}}
	second.mocks = []*models.Mock{mockAt("mock-0", ts(15))}
	require.NoError(t, recordSet(t, second, mapDB, nil))

	got, meaningful, err := mapDB.Get(context.Background(), "set")
	require.NoError(t, err)
	require.True(t, meaningful)
	require.Len(t, got, 1)
	require.Equal(t, "mock-0", got["TestNew"][0].Name)
}
