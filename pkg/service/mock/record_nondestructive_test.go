package mock

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// statefulMockDB models a real on-disk mock set: DeleteMocksForSet actually
// drops the set and InsertMock actually adds to it, so a test can assert what
// a record run leaves behind. (recordingMockDB, by contrast, only records what
// was inserted and no-ops deletes — it cannot observe data loss.)
type statefulMockDB struct {
	MockDB // nil embed: any unexpected call panics loudly

	mu   sync.Mutex
	sets map[string][]string // testSetID -> mock names currently persisted

	discardErr error // if non-nil, DiscardStagedSet returns it (models a failed cleanup)
}

func newStatefulMockDB() *statefulMockDB {
	return &statefulMockDB{sets: map[string][]string{}}
}

func (s *statefulMockDB) InsertMock(_ context.Context, m *models.Mock, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets[id] = append(s.sets[id], m.Name)
	return nil
}

func (s *statefulMockDB) DeleteMocksForSet(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sets, id)
	return nil
}

func (s *statefulMockDB) ResetCounterID()    {}
func (s *statefulMockDB) SetCounterID(int64) {}

// PromoteStagedSet mirrors the yaml store: a non-empty staging set replaces the
// target, then staging is dropped. An empty staging set is refused so a bug
// upstream can never blank the target.
func (s *statefulMockDB) PromoteStagedSet(_ context.Context, stagingID, targetID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	staged := s.sets[stagingID]
	if len(staged) == 0 {
		return fmt.Errorf("nothing staged to promote for %q", stagingID)
	}
	s.sets[targetID] = staged
	delete(s.sets, stagingID)
	return nil
}

func (s *statefulMockDB) DiscardStagedSet(_ context.Context, stagingID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.discardErr != nil {
		return s.discardErr // the leftover staging dir could not be removed
	}
	delete(s.sets, stagingID)
	return nil
}

func (s *statefulMockDB) get(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sets[id]...)
}

// emptyInstrumentation runs the wrapped app to a clean exit but captures NO
// mocks. This is the common "the runner made no interceptable dependency calls"
// case, and it is also how every run that fails or is interrupted before the
// first mock streams looks from the mock DB's side: nothing new arrives.
type emptyInstrumentation struct {
	Instrumentation // nil embed: any unexpected call panics loudly

	out chan *models.Mock
}

func (e *emptyInstrumentation) Setup(context.Context, string, models.SetupOptions) error { return nil }

func (e *emptyInstrumentation) GetOutgoing(_ context.Context, _ models.OutgoingOptions) (<-chan *models.Mock, error) {
	return e.out, nil
}

func (e *emptyInstrumentation) Run(_ context.Context, _ models.RunOptions) models.AppError {
	close(e.out) // no mocks captured, then the runner exits cleanly
	return models.AppError{}
}

func (e *emptyInstrumentation) NotifyGracefulShutdown(context.Context) error { return nil }

// TestRecord_ZeroCaptureDoesNotDestroyExistingSet reproduces the destructive-
// record data loss (design §P0b, gaps W1/W14 "destructive half"): today
// `keploy mock record --name X` drops X's existing mocks BEFORE capture is even
// armed (record.go DeleteMocksForSet, step 3). So a run that captures nothing —
// the app made no interceptable calls, or it failed or was Ctrl+C'd before any
// mock streamed — leaves the user with NO recording where a good one existed.
//
// The root-cause fix records into a staging set and only swaps it over X once
// the run has succeeded with something captured, leaving X untouched otherwise.
//
// This test FAILS on the delete-first code and passes once record is staged.
func TestRecord_ZeroCaptureDoesNotDestroyExistingSet(t *testing.T) {
	const set = "orders"

	db := newStatefulMockDB()
	db.sets[set] = []string{"mock-0", "mock-1"} // a good prior recording

	inst := &emptyInstrumentation{out: make(chan *models.Mock)}

	cfg := &config.Config{}
	cfg.Mock.Name = set

	svc := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg)

	if err := svc.Record(context.Background()); err != nil {
		t.Fatalf("Record returned an error: %v", err)
	}

	if got := db.get(set); len(got) == 0 {
		t.Fatalf("destructive record: a zero-capture run wiped the existing set %q; "+
			"want the prior recording preserved, got %v", set, got)
	}
}

// TestRecord_SuccessfulCaptureReplacesExistingSet is the complementary guard:
// a complete run that DID capture mocks must still fully replace the old set
// (record is a clean rewrite, not an append), proving the staging promote does
// not accidentally preserve stale recordings.
func TestRecord_SuccessfulCaptureReplacesExistingSet(t *testing.T) {
	const set = "orders"

	db := newStatefulMockDB()
	db.sets[set] = []string{"old-0"} // a stale prior recording

	// drainInstrumentation (record_drain_test.go) emits mock-0 and mock-1.
	inst := &drainInstrumentation{
		out:       make(chan *models.Mock, 4),
		emitAfter: 20 * time.Millisecond,
	}

	cfg := &config.Config{}
	cfg.Mock.Name = set

	svc := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg)

	if err := svc.Record(context.Background()); err != nil {
		t.Fatalf("Record returned an error: %v", err)
	}

	got := db.get(set)
	if len(got) == 0 {
		t.Fatalf("a successful record left set %q empty; want the freshly-captured mocks", set)
	}
	for _, n := range got {
		if n == "old-0" {
			t.Fatalf("a successful record did not replace the old set %q (clean rewrite expected); got %v", set, got)
		}
	}
}

// TestRecord_DirtyStagingBaseAbortsAndPreservesSet guards the pre-capture clean:
// if a leftover staging set from a crashed run cannot be cleared, record must
// abort (rather than capture into a dirty base and promote a polluted set) and
// leave the existing recording untouched.
func TestRecord_DirtyStagingBaseAbortsAndPreservesSet(t *testing.T) {
	const set = "orders"

	db := newStatefulMockDB()
	db.sets[set] = []string{"mock-0", "mock-1"} // a good prior recording
	db.discardErr = fmt.Errorf("permission denied removing the leftover staging dir")

	inst := &emptyInstrumentation{out: make(chan *models.Mock)}

	cfg := &config.Config{}
	cfg.Mock.Name = set

	svc := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg)

	if err := svc.Record(context.Background()); err == nil {
		t.Fatalf("Record should abort when the staging base cannot be cleared, but returned nil")
	}
	if got := db.get(set); len(got) != 2 {
		t.Fatalf("a failed pre-capture clean must leave the existing set %q intact; got %v", set, got)
	}
}

// captureThenInterruptInstrumentation captures two mocks and then simulates the
// user stopping the recording (Ctrl+C, or a session stop) by cancelling the
// parent context — the normal way `mock record` ends, and the path a
// from-container recording stops through.
type captureThenInterruptInstrumentation struct {
	Instrumentation // nil embed: any unexpected call panics loudly

	out    chan *models.Mock
	cancel context.CancelFunc
	mocks  int // how many mocks to capture before the interrupt
}

func (c *captureThenInterruptInstrumentation) Setup(context.Context, string, models.SetupOptions) error {
	return nil
}

func (c *captureThenInterruptInstrumentation) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	return c.out, nil
}

func (c *captureThenInterruptInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	for i := 0; i < c.mocks; i++ {
		c.out <- &models.Mock{Name: fmt.Sprintf("mock-%d", i), Kind: models.HTTP}
	}
	close(c.out)
	c.cancel() // the user stops the recording
	return models.AppError{}
}

func (c *captureThenInterruptInstrumentation) NotifyGracefulShutdown(context.Context) error {
	return nil
}

// TestRecord_InterruptAfterCaptureSavesMocks guards the from-container regression
// (keploy#4684 CI): stopping a recording after mocks were captured must SAVE them
// (promote the staged set), not discard them. An interrupt is the normal way
// `mock record` ends — the pre-staging code streamed straight to the set and kept
// what was captured on Ctrl+C; discarding here lost the recording.
func TestRecord_InterruptAfterCaptureSavesMocks(t *testing.T) {
	const set = "orders"

	db := newStatefulMockDB() // no prior recording
	ctx, cancel := context.WithCancel(context.Background())
	inst := &captureThenInterruptInstrumentation{out: make(chan *models.Mock, 4), cancel: cancel, mocks: 2}

	cfg := &config.Config{}
	cfg.Mock.Name = set

	svc := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg)

	if err := svc.Record(ctx); err != nil {
		t.Fatalf("Record returned an error: %v", err)
	}

	if got := db.get(set); len(got) != 2 {
		t.Fatalf("an interrupt after capturing 2 mocks must save them (promote); got %v (want 2). "+
			"Discarding here loses a recording stopped the normal way, e.g. from-container.", got)
	}
}

// TestRecord_InterruptWithoutCaptureDoesNotDestroyExistingSet guards the
// data-safety sub-path of the interrupt handler: a recording stopped before it
// captured anything must NOT destroy the existing set — staging is discarded and
// the prior recording stays intact.
func TestRecord_InterruptWithoutCaptureDoesNotDestroyExistingSet(t *testing.T) {
	const set = "orders"

	db := newStatefulMockDB()
	db.sets[set] = []string{"keep-0", "keep-1"} // a good prior recording

	ctx, cancel := context.WithCancel(context.Background())
	inst := &captureThenInterruptInstrumentation{out: make(chan *models.Mock, 4), cancel: cancel, mocks: 0}

	cfg := &config.Config{}
	cfg.Mock.Name = set

	svc := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg)

	if err := svc.Record(ctx); err != nil {
		t.Fatalf("Record returned an error: %v", err)
	}

	if got := db.get(set); len(got) != 2 {
		t.Fatalf("an interrupt with no capture must leave the existing set %q intact; got %v (want the 2 prior mocks)", set, got)
	}
}
