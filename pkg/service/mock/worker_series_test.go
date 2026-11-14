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

// The table's once series are the series of workerSeries one entry of
// which says once (models.MockEntry.Once), sorted: an entry the agent does
// not hold, or one of no series, makes none once, and no once entry, or no
// mapping, is none.
func TestOnceSeriesAreTheSeriesAnEntrySaysOnce(t *testing.T) {
	mp := &models.Mapping{Worker: []models.MockEntry{
		{Name: "mock-9", Rule: "r@1", Series: "r/-/2"},
		{Name: "mock-3", Rule: "r@1", Series: "r/-/2", Once: true},
		{Name: "mock-4", Rule: "p@1", Series: "p/-/1", Once: true},
		{Name: "mock-5", Rule: "p@1", Series: "p/-/1", Once: true},
		{Name: "mock-6", Rule: "q@1", Series: "q/-/1", Once: true},
		{Name: "mock-2", Once: true},
		{Name: "mock-7", Rule: "s@1", Series: "s/-/1"},
	}}
	worker := map[string][]string{"o": {"mock-2", "mock-3", "mock-4", "mock-5", "mock-7"}, "p": {"mock-9"}}
	require.Equal(t, []string{"p/-/1", "r/-/2"}, onceSeries(mp, worker))
	require.Nil(t, onceSeries(mp, map[string][]string{"o": {"mock-9", "mock-7"}}))
	require.Nil(t, onceSeries(nil, worker))
}

// The table's every is, by once series, the longest interval a held entry
// of it says (models.MockEntry.Every): an entry whose every does not parse
// as a positive duration, one the agent does not hold, and a series not
// once say none; no once series, or no mapping, is none.
func TestEverySeriesIsTheLongestIntervalOfAOnceSeries(t *testing.T) {
	mp := &models.Mapping{Worker: []models.MockEntry{
		{Name: "mock-3", Rule: "p@1", Series: "p/-/1", Once: true, Every: "5m"},
		{Name: "mock-4", Rule: "p@1", Series: "p/-/1", Once: true, Every: "10m"},
		{Name: "mock-9", Rule: "p@1", Series: "p/-/1", Once: true, Every: "1h"},
		{Name: "mock-5", Rule: "q@1", Series: "q/-/1", Once: true, Every: "soon"},
		{Name: "mock-6", Rule: "q@1", Series: "q/-/1", Once: true, Every: "-5m"},
		{Name: "mock-7", Rule: "s@1", Series: "s/-/1", Every: "5m"},
	}}
	worker := map[string][]string{"o": {"mock-3", "mock-4", "mock-5", "mock-6", "mock-7"}}
	once := onceSeries(mp, worker)
	require.Equal(t, []string{"p/-/1", "q/-/1"}, once)
	require.Equal(t, map[string]string{"p/-/1": "10m0s"}, everySeries(mp, worker, once))
	require.Nil(t, everySeries(mp, worker, nil))
	require.Nil(t, everySeries(nil, worker, once))
}
