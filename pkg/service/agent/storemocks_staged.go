package agent

import (
	"context"
	"crypto/rand"
	"encoding/gob"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	proxyPkg "go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// defaultStoreMocksStageTTL bounds how long an idle staged upload session is
// retained before its temporary DiskMocks file is automatically closed and
// removed.
const defaultStoreMocksStageTTL = 10 * time.Minute

var (
	// ErrStoreMocksStageNotFound reports a batch or commit referencing an
	// unknown, expired, superseded, or aborted staging session.
	ErrStoreMocksStageNotFound = errors.New("storemocks: staging session not found")

	// ErrStoreMocksIncomplete reports a commit before all declared mocks have
	// been ingested.
	ErrStoreMocksIncomplete = errors.New("storemocks: staging upload incomplete")
)

// StoreMocksSeqConflictError reports an out-of-order batch sequence number
// (seq > expectedSeq).
type StoreMocksSeqConflictError struct {
	ExpectedSeq int
	GotSeq      int
}

func (e *StoreMocksSeqConflictError) Error() string {
	return fmt.Sprintf("storemocks: out-of-order batch seq %d (expected %d)", e.GotSeq, e.ExpectedSeq)
}

type storeMocksStage struct {
	id             string
	header         models.MockStreamHeader
	total          int
	storage        *ClientMockStorage
	disk           *proxyPkg.DiskMocks
	unfilteredDisk *proxyPkg.DiskMocks
	asyncMocks     []*models.Mock
	wantAsync      bool
	proven         bool
	tally          containerkey.RecordedTally

	mu         sync.Mutex
	nextSeq    int
	ingested   int
	closed     bool
	committing bool
	commitDone chan struct{}
	commitErr  error
	timer      *time.Timer
}

// closeLocked tears down the stage's on-disk stores and expiry timer. Caller
// holds s.mu. While a commit is in flight, ownership of s.disk and
// s.unfilteredDisk belongs to CommitStoreMocks so closeLocked leaves them to
// finish cleanly.
func (s *storeMocksStage) closeLocked() {
	if s.closed {
		return
	}
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if !s.committing {
		if s.disk != nil {
			_ = s.disk.Close()
			s.disk = nil
		}
		if s.unfilteredDisk != nil {
			_ = s.unfilteredDisk.Close()
			s.unfilteredDisk = nil
		}
	}
}

func (a *Agent) stageTTL() time.Duration {
	if a.stagingTTL > 0 {
		return a.stagingTTL
	}
	return defaultStoreMocksStageTTL
}

// BeginStoreMocks opens a staged batched upload session, superseding and
// cleaning up any prior active staging session so abandoned uploads never leak
// temporary DiskMocks files or file descriptors.
func (a *Agent) BeginStoreMocks(_ context.Context, req models.StoreMocksBeginReq) (string, error) {
	total := req.FilteredCount + req.UnfilteredCount
	if req.FilteredCount < 0 || req.UnfilteredCount < 0 || total < 0 {
		return "", fmt.Errorf("storemocks begin: invalid counts (filtered=%d unfiltered=%d)", req.FilteredCount, req.UnfilteredCount)
	}
	if uint64(total) > math.MaxUint32 {
		return "", fmt.Errorf("storemocks begin: a set of %d mocks: a mock's place in the set counts at most %d", total, uint64(math.MaxUint32))
	}

	var disk *proxyPkg.DiskMocks
	if pkg.IsStrictMockWindow(a.config != nil && a.config.Test.StrictMockWindow) || a.stagesMockReplay() {
		d, diskErr := proxyPkg.NewDiskMocks(a.logger)
		if diskErr != nil {
			a.logger.Warn("on-disk mock store unavailable; keeping per-test mocks resident", zap.Error(diskErr))
		} else {
			disk = d
		}
	}
	var unfilteredDisk *proxyPkg.DiskMocks
	if req.UnfilteredCount > 0 && (disk != nil || req.UnfilteredCount > unfilteredResidentLimit) {
		ud, udErr := proxyPkg.NewDiskMocksWithBudget(a.logger, unfilteredDiskBudgetBytes)
		if udErr != nil {
			a.logger.Warn("on-disk unfiltered mock store unavailable; keeping unfiltered mocks resident", zap.Error(udErr))
		} else {
			unfilteredDisk = ud
		}
	}
	storage := &ClientMockStorage{
		diskMocks:      disk,
		unfilteredDisk: unfilteredDisk,
	}
	if disk == nil {
		storage.filtered = make([]*models.Mock, 0, presizeCap(req.FilteredCount))
	}
	if unfilteredDisk == nil {
		storage.unfiltered = make([]*models.Mock, 0, presizeCap(req.UnfilteredCount))
	}
	proven := provenswitch.On()
	if proven {
		storage.dests = recordedDests{}
	}

	id := rand.Text()
	st := &storeMocksStage{
		id:             id,
		header:         models.MockStreamHeader{FilteredCount: req.FilteredCount, UnfilteredCount: req.UnfilteredCount},
		total:          total,
		storage:        storage,
		disk:           disk,
		unfilteredDisk: unfilteredDisk,
		wantAsync:      a.wantsAsyncMocks(),
		proven:         proven,
	}

	ttl := a.stageTTL()
	var superseded []*storeMocksStage
	a.stagingMu.Lock()
	for oldID, oldSt := range a.staging {
		superseded = append(superseded, oldSt)
		delete(a.staging, oldID)
	}
	if a.staging == nil {
		a.staging = make(map[string]*storeMocksStage)
	}
	st.timer = time.AfterFunc(ttl, func() {
		a.expireStoreMocksStage(id, st)
	})
	a.staging[id] = st
	a.stagingMu.Unlock()

	for _, oldSt := range superseded {
		oldSt.mu.Lock()
		oldSt.closeLocked()
		oldSt.mu.Unlock()
	}

	return id, nil
}

func (a *Agent) expireStoreMocksStage(id string, st *storeMocksStage) {
	a.stagingMu.Lock()
	cur, ok := a.staging[id]
	if ok && cur == st {
		delete(a.staging, id)
	}
	a.stagingMu.Unlock()

	st.mu.Lock()
	st.closeLocked()
	st.mu.Unlock()
}

func (a *Agent) lookupStage(stagingID string) (*storeMocksStage, bool) {
	if stagingID == "" {
		return nil, false
	}
	a.stagingMu.Lock()
	st, ok := a.staging[stagingID]
	a.stagingMu.Unlock()
	return st, ok
}

// ingestStagedMockLocked mirrors StoreMocksStream's per-mock processing so
// batched and legacy streaming produce identical ClientMockStorage state.
// Caller holds st.mu.
func (a *Agent) ingestStagedMockLocked(st *storeMocksStage, mock *models.Mock, idx int) {
	mock.TestModeInfo.LoadSeq = uint32(idx + 1)
	mock.DeriveLifetime()
	mocknoise.MaterializeCorrelations(a.logger, mock)
	if st.proven {
		st.tally.Add(mock)
		st.storage.dests.add(mock)
	}
	if st.proven && !a.followable(mock) {
		st.storage.noteRefused(mock)
		return
	}
	if st.wantAsync && mock.IsAsync() {
		st.asyncMocks = append(st.asyncMocks, mock)
	}
	if idx < st.header.FilteredCount {
		if st.disk != nil && proxyPkg.EligibleForDisk(mock) {
			if err := st.disk.Add(mock); err != nil {
				a.logger.Warn("on-disk mock write failed; keeping mock resident",
					zap.String("mock", mock.Name), zap.Error(err))
				st.storage.filtered = append(st.storage.filtered, mock)
			}
		} else {
			st.storage.filtered = append(st.storage.filtered, mock)
		}
	} else {
		if isUnfilteredWindowSensitive(mock) {
			st.storage.unfilteredWindowSensitive = true
		}
		if st.unfilteredDisk != nil {
			if err := st.unfilteredDisk.Add(mock); err != nil {
				a.logger.Warn("on-disk unfiltered mock write failed; keeping mock resident",
					zap.String("mock", mock.Name), zap.Error(err))
				st.storage.unfiltered = append(st.storage.unfiltered, mock)
			}
		} else {
			st.storage.unfiltered = append(st.storage.unfiltered, mock)
		}
	}
}

// StoreMocksBatch decodes and atomically ingests one bounded batch into an
// active staging session. Batches are idempotent by seq: if seq < nextSeq
// (already committed, e.g. ACK lost to a TCP reset), it returns success without
// duplicating mocks; if seq > nextSeq, it returns *StoreMocksSeqConflictError.
// If decoding fails mid-batch, no mocks from the batch are appended.
func (a *Agent) StoreMocksBatch(ctx context.Context, stagingID string, seq int, header models.MockBatchHeader, dec *gob.Decoder) (models.StoreMocksBatchResp, error) {
	if seq < 0 {
		return models.StoreMocksBatchResp{}, fmt.Errorf("storemocks batch: invalid negative seq %d", seq)
	}
	if header.Seq != seq {
		return models.StoreMocksBatchResp{}, fmt.Errorf("storemocks batch: header seq %d does not match query seq %d", header.Seq, seq)
	}
	if header.Count < 0 {
		return models.StoreMocksBatchResp{}, fmt.Errorf("storemocks batch %d: invalid negative count %d", seq, header.Count)
	}

	st, ok := a.lookupStage(stagingID)
	if !ok {
		return models.StoreMocksBatchResp{}, ErrStoreMocksStageNotFound
	}

	ttl := a.stageTTL()
	st.mu.Lock()
	if st.closed || st.committing {
		st.mu.Unlock()
		return models.StoreMocksBatchResp{}, ErrStoreMocksStageNotFound
	}
	if st.timer != nil {
		st.timer.Reset(ttl)
	}
	if seq < st.nextSeq {
		resp := models.StoreMocksBatchResp{
			IsSuccess:   true,
			StagingID:   stagingID,
			Seq:         seq,
			ExpectedSeq: st.nextSeq,
			Ingested:    st.ingested,
		}
		st.mu.Unlock()
		return resp, nil
	}
	if seq > st.nextSeq {
		expected := st.nextSeq
		st.mu.Unlock()
		return models.StoreMocksBatchResp{
			StagingID:   stagingID,
			Seq:         seq,
			ExpectedSeq: expected,
		}, &StoreMocksSeqConflictError{ExpectedSeq: expected, GotSeq: seq}
	}
	if st.ingested+header.Count > st.total {
		ingested, total := st.ingested, st.total
		st.mu.Unlock()
		return models.StoreMocksBatchResp{}, fmt.Errorf("storemocks batch %d: count %d exceeds remaining mocks (%d/%d ingested)", seq, header.Count, ingested, total)
	}
	st.mu.Unlock()

	// Decode the full bounded batch before mutating stage state so a mid-batch
	// connection reset or decode error never partially appends mocks.
	batch := make([]*models.Mock, 0, presizeCap(header.Count))
	for i := 0; i < header.Count; i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return models.StoreMocksBatchResp{}, err
			}
		}
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			return models.StoreMocksBatchResp{}, fmt.Errorf("storemocks batch %d: decode mock %d/%d: %w", seq, i+1, header.Count, err)
		}
		batch = append(batch, &m)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed || st.committing {
		return models.StoreMocksBatchResp{}, ErrStoreMocksStageNotFound
	}
	if st.timer != nil {
		st.timer.Reset(ttl)
	}
	if seq < st.nextSeq {
		// Another retry already committed this seq while we were decoding.
		return models.StoreMocksBatchResp{
			IsSuccess:   true,
			StagingID:   stagingID,
			Seq:         seq,
			ExpectedSeq: st.nextSeq,
			Ingested:    st.ingested,
		}, nil
	}
	if seq > st.nextSeq {
		return models.StoreMocksBatchResp{
			StagingID:   stagingID,
			Seq:         seq,
			ExpectedSeq: st.nextSeq,
		}, &StoreMocksSeqConflictError{ExpectedSeq: st.nextSeq, GotSeq: seq}
	}
	if st.ingested+len(batch) > st.total {
		return models.StoreMocksBatchResp{}, fmt.Errorf("storemocks batch %d: count %d exceeds remaining mocks (%d/%d ingested)", seq, len(batch), st.ingested, st.total)
	}

	baseIdx := st.ingested
	for j, mock := range batch {
		a.ingestStagedMockLocked(st, mock, baseIdx+j)
	}
	st.ingested += len(batch)
	st.nextSeq++

	return models.StoreMocksBatchResp{
		IsSuccess:   true,
		StagingID:   stagingID,
		Seq:         seq,
		ExpectedSeq: st.nextSeq,
		Ingested:    st.ingested,
	}, nil
}

// CommitStoreMocks finalizes and atomically publishes a completed staging
// session. Repeating CommitStoreMocks with the same stagingID while commit is
// in flight waits for the in-flight commit result, and repeating it after commit
// is idempotent so a lost commit ACK can be retried cleanly without a 404.
func (a *Agent) CommitStoreMocks(ctx context.Context, stagingID string) error {
	if stagingID == "" {
		return ErrStoreMocksStageNotFound
	}
	a.stagingMu.Lock()
	if stagingID == a.lastCommittedStagingID {
		a.stagingMu.Unlock()
		return nil
	}
	st, ok := a.staging[stagingID]
	a.stagingMu.Unlock()
	if !ok {
		return ErrStoreMocksStageNotFound
	}

	st.mu.Lock()
	if st.committing {
		done := st.commitDone
		st.mu.Unlock()
		select {
		case <-done:
			return st.commitErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if st.closed {
		st.mu.Unlock()
		return ErrStoreMocksStageNotFound
	}
	if st.ingested != st.total {
		ingested, total := st.ingested, st.total
		st.mu.Unlock()
		return fmt.Errorf("%w: received %d/%d mocks", ErrStoreMocksIncomplete, ingested, total)
	}
	st.committing = true
	commitDone := make(chan struct{})
	st.commitDone = commitDone
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	disk := st.disk
	unfilteredDisk := st.unfilteredDisk
	storage := st.storage
	asyncMocks := st.asyncMocks
	proven := st.proven
	tally := st.tally
	st.mu.Unlock()

	var commitErr error
	defer func() {
		if commitErr != nil {
			if disk != nil {
				_ = disk.Close()
			}
			if unfilteredDisk != nil {
				_ = unfilteredDisk.Close()
			}
		}
		st.commitErr = commitErr

		a.stagingMu.Lock()
		if commitErr == nil {
			a.lastCommittedStagingID = stagingID
		}
		if cur, ok := a.staging[stagingID]; ok && cur == st {
			delete(a.staging, stagingID)
		}
		a.stagingMu.Unlock()

		st.mu.Lock()
		st.closed = true
		st.disk = nil
		st.unfilteredDisk = nil
		st.mu.Unlock()

		close(commitDone)
	}()

	if proven {
		containerkey.Default.SetRecorded(&tally)
	}
	if unfilteredDisk != nil {
		if err := unfilteredDisk.Flush(); err != nil {
			commitErr = fmt.Errorf("storemocks commit: flush the unfiltered on-disk store: %w", err)
			return commitErr
		}
		unfilteredDisk.Finalize()
	}
	if disk != nil {
		if err := disk.Flush(); err != nil {
			commitErr = fmt.Errorf("storemocks commit: flush the on-disk store: %w", err)
			return commitErr
		}
		disk.Finalize()
		statements, err := disk.LoadStatements()
		if err != nil {
			commitErr = fmt.Errorf("storemocks commit: read the statement catalog from the on-disk store: %w", err)
			return commitErr
		}
		storage.statements = statements
	}
	if unfilteredDisk != nil {
		unfStatements, err := unfilteredDisk.LoadStatements()
		if err != nil {
			commitErr = fmt.Errorf("storemocks commit: read the statement catalog from the unfiltered on-disk store: %w", err)
			return commitErr
		}
		if len(unfStatements) > 0 {
			storage.statements = dedupByName(storage.statements, unfStatements)
		}
	}
	if disk != nil || unfilteredDisk != nil {
		ptOnDisk, ptSpilled, ptDiskBytes, ptResBytes := 0, 0, int64(0), int64(0)
		if disk != nil {
			ptOnDisk = disk.Len()
			ptSpilled = disk.SpilledResponses()
			ptDiskBytes = disk.DiskBytes()
			ptResBytes = disk.ResidentBytes()
		}
		unfOnDisk, unfSpilled, unfDiskBytes, unfResBytes := 0, 0, int64(0), int64(0)
		if unfilteredDisk != nil {
			unfOnDisk = unfilteredDisk.Len()
			unfSpilled = unfilteredDisk.SpilledResponses()
			unfDiskBytes = unfilteredDisk.DiskBytes()
			unfResBytes = unfilteredDisk.ResidentBytes()
		}
		a.logger.Info("agent mock residency: per-test mocks parked on disk (windowed)",
			zap.Int("onDisk", ptOnDisk),
			zap.Int("spilledResponses", ptSpilled),
			zap.Int64("diskBytes", ptDiskBytes),
			zap.Int64("residentBytes", ptResBytes),
			zap.Int("residentPerTest", len(storage.filtered)),
			zap.Int("residentConfig", len(storage.unfiltered)),
			zap.Int("unfilteredOnDisk", unfOnDisk),
			zap.Int("unfilteredSpilledResponses", unfSpilled),
			zap.Int64("unfilteredDiskBytes", unfDiskBytes),
			zap.Int64("unfilteredResidentBytes", unfResBytes))
	}

	a.finalizeClientMocks(ctx, storage)
	a.loadAsyncIntoProxy(asyncMocks)

	a.logger.Debug("Successfully stored batched mocks for client",
		zap.String("stagingId", stagingID),
		zap.Int("filtered", len(storage.filtered)),
		zap.Int("unfiltered", len(storage.unfiltered)))
	return nil
}

// AbortStoreMocks closes and deletes an active staging session (best-effort,
// idempotent).
func (a *Agent) AbortStoreMocks(_ context.Context, stagingID string) error {
	if stagingID == "" {
		return nil
	}
	a.stagingMu.Lock()
	st, ok := a.staging[stagingID]
	if ok {
		delete(a.staging, stagingID)
	}
	a.stagingMu.Unlock()

	if ok && st != nil {
		st.mu.Lock()
		st.closeLocked()
		st.mu.Unlock()
	}
	return nil
}

// closeAllStaging closes every active staging session's DiskMocks store and
// stops its expiry timer during agent shutdown.
func (a *Agent) closeAllStaging() {
	a.stagingMu.Lock()
	active := make([]*storeMocksStage, 0, len(a.staging))
	for id, st := range a.staging {
		active = append(active, st)
		delete(a.staging, id)
	}
	a.stagingMu.Unlock()

	for _, st := range active {
		st.mu.Lock()
		st.closeLocked()
		st.mu.Unlock()
	}
}
