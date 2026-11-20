package mock

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// A read series' last answer caused w1, a state-changing write the replay
// serves: w1 ends the series' repeats. A write caused by an earlier
// recording of the series (w2), a read (w3), a write that changed nothing
// (w4), a write the replay does not serve (w5) and a write a write series
// caused (w6) end nothing. The last is the agent's (ordered), not
// mappings.yaml's order, and a series' part gate is kept beside it. A
// second write the last answer caused (w7) is named beside w1.
func TestAgainClosersNameTheWritesAReadSeriesLastAnswerCaused(t *testing.T) {
	mp := &models.Mapping{Worker: []models.MockEntry{
		{Name: "r1", Series: "marks/-/1", Read: true},
		{Name: "r2", Series: "marks/-/1", Read: true},
		{Name: "w1", Series: "unmark/T/1", CausedBy: "r2"},
		{Name: "w2", Series: "unmark/T/2", CausedBy: "r1"},
		{Name: "w3", Series: "unmark/T/3", CausedBy: "r2", Read: true},
		{Name: "w4", Series: "unmark/T/4", CausedBy: "r2", Unchanged: true},
		{Name: "w5", Series: "unmark/T/5", CausedBy: "r2"},
		{Name: "p1", Series: "put/-/1"},
		{Name: "w6", Series: "unmark/T/6", CausedBy: "p1"},
	}}
	worker := map[string][]string{"k": {"r1", "r2", "w1", "w2", "w3", "w4", "p1", "w6"}}
	var gates map[string]models.SeriesGate
	require.NotPanics(t, func() { gates = seriesGates(mp, worker, workerSeries(mp, worker)) })
	require.Equal(t, map[string]models.SeriesGate{"marks/-/1": {AgainClosedBy: []string{"w1"}}}, gates)

	reversed := workerSeries(mp, worker)
	reversed["marks/-/1"] = []string{"r2", "r1"}
	require.Equal(t, map[string]models.SeriesGate{"marks/-/1": {AgainClosedBy: []string{"w2"}}}, seriesGates(mp, worker, reversed),
		"the agent answers the repeats with the last in record order: the write that answer caused ends them")

	mp.Worker[0].ClosedBy = "del"
	require.Equal(t, map[string]models.SeriesGate{"marks/-/1": {ClosedBy: "del", AgainClosedBy: []string{"w1"}}}, seriesGates(mp, worker, workerSeries(mp, worker)),
		"a part gate and the writes that end the repeats are one gate")

	mp.Worker[0].ClosedBy = ""
	mp.Worker = append(mp.Worker, models.MockEntry{Name: "w7", Series: "unmark/T/7", CausedBy: "r2"})
	worker["k"] = append(worker["k"], "w7")
	require.Equal(t, map[string]models.SeriesGate{"marks/-/1": {AgainClosedBy: []string{"w1", "w7"}}}, seriesGates(mp, worker, workerSeries(mp, worker)),
		"every served write the last answer caused ends the repeats, in mappings order")
}
