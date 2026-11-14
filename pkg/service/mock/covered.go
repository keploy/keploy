package mock

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// coveredRecordings is the names of mp's worker: entries keploy's judging
// marked covered (models.MockEntry.CoveredBy): a later round's recording of
// a series that answers again after an interval, whose calls the recording
// of the entry it names answers in each round. It refuses an entry whose
// cover is not another worker: entry of its series, under its key, that
// answers once in a round and again after a positive interval (Once and
// Every), and is not covered itself: such a cover could not answer the
// entry's calls, so leaving the entry out would lose them. nil when none.
func coveredRecordings(mp *models.Mapping) (map[string]bool, error) {
	if mp == nil {
		return nil, nil
	}
	byName := make(map[string]models.MockEntry, len(mp.Worker))
	for _, e := range mp.Worker {
		if _, seen := byName[e.Name]; !seen {
			byName[e.Name] = e
		}
	}
	var out map[string]bool
	for _, e := range mp.Worker {
		if e.CoveredBy == "" {
			continue
		}
		c, ok := byName[e.CoveredBy]
		why := ""
		switch {
		case !ok || c.Name == e.Name:
			why = "which is no other entry of worker:"
		case c.CoveredBy != "":
			why = "which is covered itself, by " + c.CoveredBy
		case e.Series == "" || c.Series != e.Series:
			why = fmt.Sprintf("which is of series %q, not of the entry's series %q", c.Series, e.Series)
		case c.Key != e.Key:
			why = fmt.Sprintf("which is recorded under key %q, not under the entry's key %q", c.Key, e.Key)
		case !c.Once || !positiveEvery(c.Every):
			why = "which does not answer once in a round and again after a positive interval (once and every)"
		}
		if why != "" {
			return nil, fmt.Errorf("worker: entry %s is covered by %s, %s", e.Name, e.CoveredBy, why)
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[e.Name] = true
	}
	return out, nil
}

// positiveEvery is whether every parses as a positive Go duration, an
// interval a series answers again after (models.MockEntry.Every).
func positiveEvery(every string) bool {
	d, err := time.ParseDuration(every)
	return err == nil && d > 0
}

// leaveCoveredOut is, under the proven switch, filtered and unfiltered less
// the recordings the set's mappings mark covered (coveredRecordings): the
// agent is never handed one, so no path of it serves one, and the
// recording each names answers its calls in each round. A set whose
// mappings this keploy cannot read here leaves both as they are: the table
// push reads them again and refuses them. A cover that cannot answer the
// entry's calls refuses the set, as a mapping the table push cannot read
// does.
func (m *mockService) leaveCoveredOut(ctx context.Context, name string, filtered, unfiltered []*models.Mock) ([]*models.Mock, []*models.Mock, error) {
	reader, ok := m.mappingDB.(MappingReader)
	if !ok {
		return filtered, unfiltered, nil
	}
	mp, err := reader.GetMapping(ctx, name)
	if err != nil {
		return filtered, unfiltered, nil
	}
	covered, err := coveredRecordings(mp)
	if err != nil {
		return nil, nil, fmt.Errorf("keploy cannot judge each test of mock set %q: its mappings cannot be read: %w; if you edited the set's mappings file, correct that edit (a record would replace it), otherwise record the set again with keploy mock record", name, err)
	}
	if len(covered) == 0 {
		return filtered, unfiltered, nil
	}
	m.logger.Info("each covered recording's calls are answered by the recording of its series that covers it, in each round of the series; the agent is not handed the covered ones",
		zap.String("mock-set", name), zap.Strings("covered", slices.Sorted(maps.Keys(covered))))
	drop := func(mocks []*models.Mock) []*models.Mock {
		out := make([]*models.Mock, 0, len(mocks))
		for _, mk := range mocks {
			if mk == nil || !covered[mk.Name] {
				out = append(out, mk)
			}
		}
		return out
	}
	return drop(filtered), drop(unfiltered), nil
}
