package models

import (
	"context"
	"errors"
)

// A replay loads every mock of a test set before its first test: it sorts them
// into the agent's pools, decides which ones each test owns, and sends them to
// the agent, which parks the per-test ones on its own disk (the agent's disk
// store) and reads back only the ones a test's window needs. Holding the whole
// set in memory on the way there is what ran a replay out of memory on a large
// recording: the decoded set is about the size of its mock file, and the parse
// that builds it about doubles that at its peak.
//
// So a replay can leave a mock's payload in the mock file it read it from, the
// way the agent's disk store leaves an elided response on disk: the mock keeps
// everything the replay reads before sending it, and its payload is read back
// from the file, one mock at a time, only to be sent (WithPayload).
//
// The payload a replay leaves on disk is a Mongo mock's request and response
// messages. They are almost all of a recording's bytes (a Mongo response
// carries whole documents), and nothing a replay does before sending a mock
// reads them: the reports' summary of a Mongo mock asks only whether it has
// requests, which a mock whose messages are on disk still has, each with its
// header. A kind whose payload a replay reads before sending it (a MySQL
// command's packet decides its lifetime, an SQS poll's body whether it is an
// idle answer) keeps its payload in memory.

// PayloadLoader reads a mock's payload back into m, a copy of the mock whose
// payload was left on disk: the copy is what is sent, so its payload is the one
// the mock had when it was read, after whatever the replay does to a mock's
// payload before sending it (a decryption).
type PayloadLoader func(ctx context.Context, m *Mock) error

// LeavePayloadOnDisk drops m's payload from memory and keeps load to read it
// back. It reports whether m had a payload to leave; one that did not keeps
// what it has and no loader. Only a Mongo mock's messages are left on disk:
// each request and response keeps its header and read delay, so the mock still
// says how many of each it has.
func (m *Mock) LeavePayloadOnDisk(load PayloadLoader) bool {
	if m == nil || load == nil || m.Kind != Mongo {
		return false
	}
	left := false
	for i := range m.Spec.MongoRequests {
		if m.Spec.MongoRequests[i].Message != nil {
			m.Spec.MongoRequests[i].Message = nil
			left = true
		}
	}
	for i := range m.Spec.MongoResponses {
		if m.Spec.MongoResponses[i].Message != nil {
			m.Spec.MongoResponses[i].Message = nil
			left = true
		}
	}
	if left {
		m.payloadLoader = load
	}
	return left
}

// PayloadOnDisk reports whether m's payload was left on disk (see
// LeavePayloadOnDisk), so m is not the mock as recorded until WithPayload
// reads it back.
func (m *Mock) PayloadOnDisk() bool { return m != nil && m.payloadLoader != nil }

// errPayloadNotRead is the error of a loader that returned without reading the
// payload back.
var errPayloadNotRead = errors.New("the mock's payload was not read back from its mock file")

// ErrPayloadFileReplaced is the error of reading back a payload whose mock file
// has been replaced since the mock was read: the replay rewrites a test set's
// mock file once the set has run (it prunes the mocks no test consumed, folds
// learned noise in), so the mock is of a set that is over.
var ErrPayloadFileReplaced = errors.New("the mock file was rewritten after the mock was read from it")

// WithPayload returns the mock as recorded: m itself when its payload is in
// memory, else a copy of m with its payload read back from disk. The copy is
// meant to be sent and dropped: m keeps its payload on disk, so holding m holds
// no payload.
func (m *Mock) WithPayload(ctx context.Context) (*Mock, error) {
	if !m.PayloadOnDisk() {
		return m, nil
	}
	c := m.ShallowCopy()
	c.payloadLoader = nil
	// The copy gets requests and responses of its own: the loader fills
	// them, and m's stay as they are.
	c.Spec.MongoRequests = nil
	c.Spec.MongoResponses = nil
	if err := m.payloadLoader(ctx, c); err != nil {
		return nil, err
	}
	if len(c.Spec.MongoRequests) != len(m.Spec.MongoRequests) || len(c.Spec.MongoResponses) != len(m.Spec.MongoResponses) {
		return nil, errPayloadNotRead
	}
	return c, nil
}
