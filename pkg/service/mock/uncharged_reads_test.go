package mock

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
)

// seqUnchargedRunner is lossRunner whose reads of the losses no test was
// charged with answer, in turn, errAt[n] for the nth read (from 1), and
// uncharged otherwise.
type seqUnchargedRunner struct {
	*lossRunner
	reads int
	errAt map[int]error
}

func (s *seqUnchargedRunner) GetUnchargedCaptureLosses(context.Context) (models.CaptureLosses, error) {
	s.reads++
	if err := s.errAt[s.reads]; err != nil {
		return nil, err
	}
	return s.uncharged, nil
}

// A judged record reads the losses no test was charged with twice: after the
// test command (captureLossesDuring, which has the agent drain first) and as
// it judges the run (runLosses). When the first read answers and the second
// fails, what was lost after the last test is unknown, never "none": every
// loss of the run is the run's, with the second read's error, so the record
// is never judged clean on what was counted. An agent older than the read
// (the second answers that it cannot say) leaves every counted loss the
// run's, with no error.
func TestARecordsSecondUnchargedReadThatFailsIsNeverClean(t *testing.T) {
	for _, row := range []struct {
		name    string
		second  error
		wantErr bool
	}{
		{"the second read failed", errors.New("the keploy agent made no answer"), true},
		{"an agent older than the read", fmt.Errorf("%w: agent returned 404", models.ErrCaptureLossesUnsupported), false},
	} {
		t.Run(row.name, func(t *testing.T) {
			in := &seqUnchargedRunner{lossRunner: &lossRunner{lossCounter: &lossCounter{after: stuckClose}, uncharged: stuckClose},
				errAt: map[int]error{2: row.second}}
			in.reads, in.lossCounter.reads = 0, 1 // the base read was made
			m := &mockService{logger: zap.NewNop(), instrumentation: in}
			base := lossBase{checked: true}
			lost, err := m.captureLossesDuring(context.Background(), base)
			require.NoError(t, err, "the first read answered")
			require.Equal(t, stuckClose, lost)
			got, err := m.runLosses(context.Background(), base, lost, nil, models.PIDMapRefusals{})
			require.Equal(t, 2, in.reads, "premise: the record read the losses no test was charged with twice")
			require.Equal(t, stuckClose, got, "every loss of the run is the run's")
			if !row.wantErr {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "could not read which of the run's capture losses no test was charged with")
			require.ErrorContains(t, err, "made no answer")
		})
	}
}
