package http

import (
	"context"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The learn step (learn.go) for another parser of HTTP mocks: the SQS parser,
// whose recordings are HTTP mocks. In record's full round the HTTP parser
// decides every request with the learn step (learnMatchIn): a value the
// application made again (a fresh id, a clock read) is learned onto the
// recording it drifted from, or the call misses with the learn step's reason.
// A parser that held its recordings to the exact pass alone missed every such
// call with no reason, and learned nothing.

// LearnMiss is why the learn step answered a call with no recording.
type LearnMiss struct{ m *learnMiss }

// Closest is the recording the learn step refused the call against; nil
// where it named none.
func (m *LearnMiss) Closest() *models.Mock {
	if m == nil || m.m == nil {
		return nil
	}
	return m.m.closest
}

// Explain writes the learn step's reason onto the miss's report, as the HTTP
// parser's miss carries it: its code, for judging, and what differed, for
// the person who reads it. Neither holds a value.
func (m *LearnMiss) Explain(r *models.MockMismatchReport) {
	if m == nil || m.m == nil {
		return
	}
	m.m.explain(r)
}

// LearnMatch decides a call of another parser in record's full round with
// the learn step, as the HTTP parser decides its own: under the proven
// switch, with opts.Learn, until the set's learn step ended
// (integrations.LearnEnded). served is the answer: the recording the call
// matches exactly modulo what is known, or the one it drifted from, taken
// from mockDb with what drifted learned onto it. miss is why it answered
// none. ctx has the call's destination pinned (PinLiveDestination), and
// noise is the run's request noise (NewRequestNoise).
//
// round is the caller's mark of the call (integrations.MarkCallRound of
// mockDb), made before the caller's own reads of the store held to it
// (integrations.HoldToRound): the learn step's first read must be of that
// round. Where the replay moved it since (a staging, a test's end or begin:
// integrations.CallRound.MovedSince), the call is the learn step's moved
// miss, which learns nothing and names no recording: decided in the round
// the learn step reads, a late call of one test would be answered with the
// next test's recording. The zero CallRound, of a store that marks no
// round, holds the learn step to nothing.
//
// written is the answer the caller writes for a recording the learn step
// served (nil: served itself). The learn step tells the tracker of the serve
// (learn.Tracker.Served) with that answer, at its leaves and in F's view of
// the exchange, which a later call of the set reads (learn.Order.ServedView):
// the caller may write another answer than served (SQS sets a send's digests
// over the message sent), and a view of the answer served would read a later
// call against words the application never read and miss the ones it did.
// The caller states no view of the serve again.
//
// Outside record's full round nothing here runs, and served and miss are
// both nil: the caller matches the call as it would have.
func (h *HTTP) LearnMatch(ctx context.Context, call VariableCall, noise RequestNoise, mockDb integrations.MockMemDb, opts models.OutgoingOptions, round integrations.CallRound, written func(served *models.Mock) *models.Mock) (served *models.Mock, miss *LearnMiss, err error) {
	if !provenswitch.On() || !opts.Learn || integrations.LearnEnded(mockDb) {
		return nil, nil, nil
	}
	in := call.req()
	in.written = written
	served, lmiss, err := h.learnMatchIn(ctx, in, mockDb, h.newLearnNoise(opts, noise.Body, noise.URL), opts, round, nil)
	if err != nil {
		return nil, nil, err
	}
	if served != nil {
		return served, nil, nil
	}
	return nil, &LearnMiss{m: lmiss}, nil
}
