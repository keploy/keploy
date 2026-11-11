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
// parser's miss of a call whose view stayed carries it: its code, for
// judging, and what differed, for the person who reads it. Neither holds a
// value.
func (m *LearnMiss) Explain(r *models.MockMismatchReport) {
	if m == nil || m.m == nil {
		return
	}
	m.m.explain(r, false)
}

// LearnHold is how the learn step may learn a leaf of another parser's call
// (LearnMatch's hold).
type LearnHold uint8

const (
	// LearnAny: as it learns a leaf of an HTTP call.
	LearnAny LearnHold = iota
	// LearnNever: never. The leaf is the call's identity (which queue, which
	// operation, which message group): a change there is the request's.
	LearnNever
	// LearnEchoOnly: only as a value the answer carries back (a batch
	// entry's Id), never as noise, a read of the clock or an earlier call's
	// value.
	LearnEchoOnly
)

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
// serves (nil: served itself). The learn step asks it before it takes the
// recording, of the answer it would serve: where it gives nil (the caller
// cannot answer the call from that recording: SQS cannot set a send's
// digests over the message sent), the learn step takes nothing, learns
// nothing and tells nothing, and the call misses (request_changed), naming
// the recording. Of a serve, it tells the tracker (learn.Tracker.Served)
// with the answer written, at the exchange's leaves and in the view of the
// exchange the learn step keeps, which a later call of the set reads
// (learn.Order.ServedView): the caller may write another answer than served
// (SQS sets a send's digests over the message sent), and a view of the
// answer served would read a later call against words the application
// never read and miss the ones it did. The caller states no view of the
// serve again. written must give the same answer each time it is given the
// same one.
//
// hold names how the learn step may learn each leaf of the call, by the
// leaf's name in keploy.yml's noise dialect (learn.Leaf.Noise:
// "body.QueueUrl", "body.Entries[].Id", "header.x-amz-target",
// "url.path.1"); nil learns every leaf as an HTTP call's (LearnAny). A
// leaf held LearnNever is the call's identity: a recording that holds
// another value there is no candidate for the call, so nothing is learned
// there and the call is never answered from such a recording; a call that
// differs from every recording there misses (request_changed), naming the
// leaf. keploy.yml's noise still excuses it, as the user declared. A leaf
// held LearnEchoOnly is learned only as a value the answer carries back,
// which the answer then carries for the call: as noise, a read of the
// clock or an earlier call's value it is refused (request_changed).
//
// Outside record's full round nothing here runs, and served and miss are
// both nil: the caller matches the call as it would have.
func (h *HTTP) LearnMatch(ctx context.Context, call VariableCall, noise RequestNoise, mockDb integrations.MockMemDb, opts models.OutgoingOptions, round integrations.CallRound, written func(served *models.Mock) *models.Mock, hold func(leaf string) LearnHold) (served *models.Mock, miss *LearnMiss, err error) {
	if !provenswitch.On() || !opts.Learn || integrations.LearnEnded(mockDb) {
		return nil, nil, nil
	}
	in := call.req()
	in.written = written
	n := h.newLearnNoise(opts, noise.Body, noise.URL).withSources(mockDb)
	n.hold = hold
	served, lmiss, err := h.learnMatchIn(ctx, in, mockDb, n, opts, round, nil)
	if err != nil {
		return nil, nil, err
	}
	if served != nil {
		return served, nil, nil
	}
	return nil, &LearnMiss{m: lmiss}, nil
}
