package mock

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.keploy.io/server/v3/pkg/models"
)

// The table's series are the worker: entries a declared rule placed, by
// series, in the order worker: lists them (the order they were placed in):
// an entry with no series, or one the agent's worker scope does not hold, is
// in none; no such entry, or no mapping, is no series.
func TestWorkerSeriesInPlacementOrder(t *testing.T) {
	mp := &models.Mapping{Worker: []models.MockEntry{
		{Name: "mock-9", Rule: "r@1", Series: "r/o::TestA/1"},
		{Name: "mock-2"},
		{Name: "mock-4", Rule: "r@1", Series: "r/o::TestA/2"},
		{Name: "mock-3", Rule: "r@1", Series: "r/o::TestA/1"},
		{Name: "mock-7", Rule: "r@1", Series: "r/o::TestA/1"},
	}}
	worker := map[string][]string{"o": {"mock-2", "mock-3", "mock-4"}, "p": {"mock-9"}}
	require.Equal(t, map[string][]string{"r/o::TestA/1": {"mock-9", "mock-3"}, "r/o::TestA/2": {"mock-4"}}, workerSeries(mp, worker))
	require.Nil(t, workerSeries(mp, map[string][]string{"o": {"mock-2"}}))
	require.Nil(t, workerSeries(nil, worker))
}
