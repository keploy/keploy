package record

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// mock_lookahead.go lets the recorder's mock loop keep up with the agent.
//
// The loop takes every mock the agent records, on one goroutine, and writes
// each before it takes the next. Making a mock's document (its YAML) was
// nearly all of that time: about 300 µs for a MySQL answer of 20 rows, so the
// loop took about 3,000 mocks a second on one CPU. An app that called out
// faster filled the agent's queue for the stream (64 MiB), and the agent
// dropped what came past it: the test cases those mocks were for were left
// out of the recording.
//
// Now the loop takes the mocks waiting for it ahead of their writes, up to
// twice as many as it has encoders: it does what comes before a write for
// each in turn (its set, the BeforeMockInsert hooks, its name), hands its
// document to an encoder, and writes the mocks, and does what follows each
// write, in the order they came, as soon as each one's document is made and
// no mock waits to be taken. Only making the documents runs on other
// goroutines; the hooks, the writes and the bookkeeping stay on the loop's.
// What changes is the order of hooks across mocks: a mock's BeforeMockInsert
// may run before the AfterMockInsert of the mocks before it.
//
// A recording that rolls over to a new test set does not look ahead: a mock's
// set is chosen as the one before it is written (setRoll), which taking mocks
// ahead would change. Nor does a MockDB that wraps MockYaml unless it
// explicitly opts in through PreparedMockDB, so a wrapper that overrides
// InsertMock never has its inserts bypassed by method promotion.

// mockPreparer is a MockDB that inserts a mock in steps (mockdb.PreparedMock).
type mockPreparer interface {
	PrepareMock(ctx context.Context, mock *models.Mock, testSetID string) *mockdb.PreparedMock
	WritePrepared(ctx context.Context, p *mockdb.PreparedMock) error
}

// PreparedMockDB is a MockDB (typically a pointer wrapper around
// *mockdb.MockYaml) that opts into the recorder's look-ahead encoding by
// implementing PrepareMock and WritePrepared with the same side-effects as
// InsertMock.
//
// Opt-in is identity-checked: PreparedMockStore must return the receiver
// itself (a non-nil pointer equal to the MockDB handed to the recorder), or
// nil to fall back to InsertMock. Because a wrapper that embeds another
// PreparedMockDB and overrides InsertMock inherits PreparedMockStore returning
// the inner pointer, identity comparison is false at every nesting depth and
// accidental opt-in by method promotion cannot happen.
//
// The recorder passes every prepared mock to WritePrepared once, in arrival
// order, even after cancellation.
//
// Contract for implementers:
//   - PrepareMock must never return nil (Encode is called on it).
//   - WritePrepared must not mutate p.Mock() (text formats are already encoded
//     before WritePrepared runs; gob encodes at write time).
//   - WritePrepared for mock N may run after BeforeMockInsert and PrepareMock
//     for mocks N+1..N+depth; side-effects must not assume strict isolation
//     across adjacent mocks.
//   - PrepareMock and WritePrepared must delegate to the same *mockdb.MockYaml.
type PreparedMockDB interface {
	MockDB
	PrepareMock(ctx context.Context, mock *models.Mock, testSetID string) *mockdb.PreparedMock
	WritePrepared(ctx context.Context, p *mockdb.PreparedMock) error
	PreparedMockStore() MockDB
}

// maxMockEncoders bounds the encoders a recording's mock loop uses.
const maxMockEncoders = 8

// mockLookahead is the mocks the loop has taken ahead of their writes, and the
// encoders making their documents. A nil one takes none: the loop writes each
// mock as it takes it.
type mockLookahead struct {
	logger  *zap.Logger
	db      mockPreparer
	doc     *InsertedMockDoc
	finish  func(mock *models.Mock, tempID string, set *rollSet, encoded []byte, encodedFormat string, err error)
	depth   int
	jobs    chan *aheadMock
	pending []*aheadMock
	wg      sync.WaitGroup
	// mostPending is the most mocks pending at once, said as the loop ends.
	mostPending int
}

// aheadMock is one mock taken ahead of its write.
type aheadMock struct {
	mock   *models.Mock
	tempID string
	set    *rollSet
	prep   *mockdb.PreparedMock
	done   chan struct{} // closed once its document is made
}

// lookaheadFor is the loop's look-ahead, nil when the recording rolls over
// (rolls) or db cannot insert in steps. finish does what follows a mock's
// write.
func lookaheadFor(logger *zap.Logger, db MockDB, rolls bool, doc *InsertedMockDoc, finish func(mock *models.Mock, tempID string, set *rollSet, encoded []byte, encodedFormat string, err error)) *mockLookahead {
	// A bare *mockdb.MockYaml or a pointer wrapper that explicitly opts in
	// through PreparedMockDB (identity-checked so embedding never promotes
	// opt-in to an outer wrapper that overrides InsertMock).
	var p mockPreparer
	if !rolls {
		switch v := db.(type) {
		case *mockdb.MockYaml:
			if v != nil {
				p = v
			}
		case PreparedMockDB:
			rv := reflect.ValueOf(v)
			if rv.Kind() == reflect.Pointer && !rv.IsNil() && v.PreparedMockStore() == db {
				p = v
			}
		}
	}
	if p == nil {
		if logger != nil {
			logger.Debug("the mock loop writes each mock before it takes the next: no look-ahead",
				zap.String("mockStore", fmt.Sprintf("%T", db)), zap.Bool("rollsOver", rolls))
		}
		return nil
	}
	encoders := min(max(runtime.GOMAXPROCS(0)-1, 1), maxMockEncoders)
	a := &mockLookahead{logger: logger, db: p, doc: doc, finish: finish, depth: 2 * encoders, jobs: make(chan *aheadMock, 2*encoders)}
	if logger != nil {
		logger.Debug("the mock loop encodes the mocks waiting for it ahead of their writes",
			zap.Int("encoders", encoders), zap.Int("mostAhead", a.depth))
	}
	for range encoders {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			for m := range a.jobs {
				m.prep.Encode()
				close(m.done)
			}
		}()
	}
	return a
}

// next is the next frame from frames, ok false once frames is closed. Meanwhile
// it writes the oldest mock taken ahead whenever no frame is ready, or as many
// are taken as there is room for.
func (a *mockLookahead) next(frames <-chan *models.Mock) (*models.Mock, bool) {
	if a == nil {
		m, ok := <-frames
		return m, ok
	}
	for {
		if len(a.pending) == 0 {
			m, ok := <-frames
			return m, ok
		}
		if len(a.pending) < a.depth {
			select {
			case m, ok := <-frames:
				return m, ok
			default:
			}
		}
		a.finishOldest()
	}
}

// take takes mock ahead of its write, named and its document handed to an
// encoder; false when the look-ahead is off (the caller writes it now).
func (a *mockLookahead) take(mock *models.Mock, tempID string, set *rollSet) bool {
	if a == nil {
		return false
	}
	m := &aheadMock{mock: mock, tempID: tempID, set: set,
		prep: a.db.PrepareMock(a.doc.ctx, mock, set.id), done: make(chan struct{})}
	a.pending = append(a.pending, m)
	a.mostPending = max(a.mostPending, len(a.pending))
	// Never waits: no more than depth mocks are pending, and as many jobs fit.
	a.jobs <- m
	return true
}

// finishOldest writes the oldest mock taken ahead, once its document is made,
// and does what follows its write.
func (a *mockLookahead) finishOldest() {
	m := a.pending[0]
	a.pending[0] = nil
	a.pending = a.pending[1:]
	<-m.done
	encoded, encodedFormat, err := a.doc.Write(a.db, m.prep)
	a.finish(m.mock, m.tempID, m.set, encoded, encodedFormat, err)
}

// flush writes every mock taken ahead, in order.
func (a *mockLookahead) flush() {
	if a == nil {
		return
	}
	for len(a.pending) > 0 {
		a.finishOldest()
	}
}

// stop ends the encoders once nothing is pending.
func (a *mockLookahead) stop() {
	if a == nil {
		return
	}
	a.flush()
	close(a.jobs)
	a.wg.Wait()
	if a.logger != nil {
		a.logger.Debug("the mock loop's look-ahead ended", zap.Int("mostMocksAheadAtOnce", a.mostPending))
	}
}
