package agent

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func encodeBatch(t *testing.T, seq int, mocks []*models.Mock) (models.MockBatchHeader, *gob.Decoder) {
	t.Helper()
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	hdr := models.MockBatchHeader{Seq: seq, Count: len(mocks)}
	if err := enc.Encode(hdr); err != nil {
		t.Fatalf("encode batch header: %v", err)
	}
	for _, m := range mocks {
		if err := enc.Encode(m); err != nil {
			t.Fatalf("encode batch mock: %v", err)
		}
	}
	dec := gob.NewDecoder(&buf)
	var decodedHdr models.MockBatchHeader
	if err := dec.Decode(&decodedHdr); err != nil {
		t.Fatalf("decode batch header: %v", err)
	}
	return decodedHdr, dec
}

func TestStoreMocksStaged_MatchesStreamAndIdempotent(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)

	filtered := []*models.Mock{
		diskEligibleMock("pt-1", base.Add(2*time.Second)),
		diskEligibleMock("pt-2", base.Add(1*time.Second)),
		mkMock("pt-ineligible", models.HTTP, base.Add(3*time.Second)),
	}
	unfiltered := []*models.Mock{
		mkMock("cfg-1", models.DNS, base.Add(4*time.Second)),
		mkMock("cfg-2", models.HTTP, base.Add(5*time.Second)),
	}

	a := strictAgent(true)
	RegisterHooks(&captureHook{})

	id, err := a.BeginStoreMocks(ctx, models.StoreMocksBeginReq{
		FilteredCount:   len(filtered),
		UnfilteredCount: len(unfiltered),
	})
	if err != nil {
		t.Fatalf("BeginStoreMocks: %v", err)
	}

	// Out-of-order seq=1 before seq=0 must return StoreMocksSeqConflictError(ExpectedSeq=0).
	hdr1, dec1 := encodeBatch(t, 1, filtered[2:])
	_, err = a.StoreMocksBatch(ctx, id, 1, hdr1, dec1)
	var conflict *StoreMocksSeqConflictError
	if !errors.As(err, &conflict) || conflict.ExpectedSeq != 0 || conflict.GotSeq != 1 {
		t.Fatalf("out-of-order seq=1 returned %v, want StoreMocksSeqConflictError(expected=0, got=1)", err)
	}

	// Batch 0: first 2 filtered mocks.
	hdr0, dec0 := encodeBatch(t, 0, filtered[:2])
	resp0, err := a.StoreMocksBatch(ctx, id, 0, hdr0, dec0)
	if err != nil || !resp0.IsSuccess || resp0.ExpectedSeq != 1 || resp0.Ingested != 2 {
		t.Fatalf("batch 0: resp=%+v err=%v", resp0, err)
	}

	// Idempotent replay of batch 0 must succeed without duplicating mocks.
	hdr0Dup, dec0Dup := encodeBatch(t, 0, filtered[:2])
	resp0Dup, err := a.StoreMocksBatch(ctx, id, 0, hdr0Dup, dec0Dup)
	if err != nil || !resp0Dup.IsSuccess || resp0Dup.ExpectedSeq != 1 || resp0Dup.Ingested != 2 {
		t.Fatalf("idempotent batch 0 replay: resp=%+v err=%v", resp0Dup, err)
	}

	// Truncated batch 1 must fail without partially appending mocks.
	var truncBuf bytes.Buffer
	enc := gob.NewEncoder(&truncBuf)
	_ = enc.Encode(models.MockBatchHeader{Seq: 1, Count: 3})
	_ = enc.Encode(filtered[2])
	truncDec := gob.NewDecoder(&truncBuf)
	var truncHdr models.MockBatchHeader
	_ = truncDec.Decode(&truncHdr)
	if _, err := a.StoreMocksBatch(ctx, id, 1, truncHdr, truncDec); err == nil {
		t.Fatal("truncated batch 1 unexpectedly succeeded")
	}

	// Commit while incomplete must fail with ErrStoreMocksIncomplete and not publish.
	if err := a.CommitStoreMocks(ctx, id); !errors.Is(err, ErrStoreMocksIncomplete) {
		t.Fatalf("incomplete commit returned %v, want ErrStoreMocksIncomplete", err)
	}
	if _, loaded := a.clientMocks.Load(uint64(0)); loaded {
		t.Fatal("incomplete commit published partial storage")
	}

	// Valid retry of batch 1 (remaining 1 filtered + 2 unfiltered).
	hdr1OK, dec1OK := encodeBatch(t, 1, append([]*models.Mock{filtered[2]}, unfiltered...))
	resp1, err := a.StoreMocksBatch(ctx, id, 1, hdr1OK, dec1OK)
	if err != nil || !resp1.IsSuccess || resp1.ExpectedSeq != 2 || resp1.Ingested != 5 {
		t.Fatalf("batch 1 retry: resp=%+v err=%v", resp1, err)
	}

	// Commit succeeds and is idempotent on retry.
	if err := a.CommitStoreMocks(ctx, id); err != nil {
		t.Fatalf("CommitStoreMocks: %v", err)
	}
	if err := a.CommitStoreMocks(ctx, id); err != nil {
		t.Fatalf("idempotent CommitStoreMocks retry: %v", err)
	}

	st := loadStorage(t, a)
	if st.diskMocks == nil || st.diskMocks.Len() != 2 {
		t.Fatalf("on-disk mocks = %v, want 2", st.diskMocks)
	}
	if len(st.filtered) != 1 || st.filtered[0].Name != "pt-ineligible" {
		t.Fatalf("resident filtered = %v, want [pt-ineligible]", sigs(st.filtered))
	}
	if len(st.unfiltered) != 2 {
		t.Fatalf("unfiltered = %d, want 2", len(st.unfiltered))
	}
	if a.loadedMocks != 5 {
		t.Fatalf("loadedMocks = %d, want 5 (zero duplicates)", a.loadedMocks)
	}
}

func TestStoreMocksStaged_CleanupOnTimeoutSupersedeAbort(t *testing.T) {
	ctx := context.Background()

	t.Run("supersede", func(t *testing.T) {
		a := strictAgent(true)
		id1, err := a.BeginStoreMocks(ctx, models.StoreMocksBeginReq{FilteredCount: 2})
		if err != nil {
			t.Fatalf("begin 1: %v", err)
		}
		st1, ok := a.lookupStage(id1)
		if !ok || st1.disk == nil {
			t.Fatal("expected active stage 1 with disk store")
		}
		disk1 := st1.disk

		id2, err := a.BeginStoreMocks(ctx, models.StoreMocksBeginReq{FilteredCount: 1})
		if err != nil {
			t.Fatalf("begin 2: %v", err)
		}
		if id1 == id2 {
			t.Fatal("expected distinct staging IDs")
		}
		// Stage 1 must be gone and its DiskMocks closed.
		if _, err := a.StoreMocksBatch(ctx, id1, 0, models.MockBatchHeader{Seq: 0, Count: 0}, gob.NewDecoder(bytes.NewReader(nil))); !errors.Is(err, ErrStoreMocksStageNotFound) {
			t.Fatalf("superseded stage returned %v, want ErrStoreMocksStageNotFound", err)
		}
		if err := disk1.Flush(); err == nil {
			t.Fatal("superseded stage's DiskMocks was not closed (temp-file/FD leak)")
		}
		_ = a.AbortStoreMocks(ctx, id2)
	})

	t.Run("abort", func(t *testing.T) {
		a := strictAgent(true)
		id, err := a.BeginStoreMocks(ctx, models.StoreMocksBeginReq{FilteredCount: 2})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		st, _ := a.lookupStage(id)
		disk := st.disk
		if err := a.AbortStoreMocks(ctx, id); err != nil {
			t.Fatalf("abort: %v", err)
		}
		// Idempotent second abort.
		if err := a.AbortStoreMocks(ctx, id); err != nil {
			t.Fatalf("second abort: %v", err)
		}
		if err := disk.Flush(); err == nil {
			t.Fatal("aborted stage's DiskMocks was not closed")
		}
		if err := a.CommitStoreMocks(ctx, id); !errors.Is(err, ErrStoreMocksStageNotFound) {
			t.Fatalf("commit after abort returned %v, want ErrStoreMocksStageNotFound", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		a := strictAgent(true)
		a.stagingTTL = 40 * time.Millisecond
		id, err := a.BeginStoreMocks(ctx, models.StoreMocksBeginReq{FilteredCount: 2})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		st, _ := a.lookupStage(id)
		disk := st.disk

		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := a.lookupStage(id); !ok {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, ok := a.lookupStage(id); ok {
			t.Fatal("idle staging session did not expire within TTL")
		}
		if err := disk.Flush(); err == nil {
			t.Fatal("expired stage's DiskMocks was not closed")
		}
		if err := a.CommitStoreMocks(ctx, id); !errors.Is(err, ErrStoreMocksStageNotFound) {
			t.Fatalf("commit after expiry returned %v, want ErrStoreMocksStageNotFound", err)
		}
	})

	t.Run("closeAllStaging", func(t *testing.T) {
		a := strictAgent(true)
		id, err := a.BeginStoreMocks(ctx, models.StoreMocksBeginReq{FilteredCount: 1})
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		st, ok := a.lookupStage(id)
		if !ok || st.disk == nil {
			t.Fatal("expected active stage with disk store")
		}
		disk := st.disk

		a.closeAllStaging()

		if _, ok := a.lookupStage(id); ok {
			t.Fatal("active stage remained after closeAllStaging")
		}
		if err := disk.Flush(); err == nil {
			t.Fatal("staging DiskMocks was not closed by closeAllStaging")
		}
	})
}

type blockingFreezeHook struct {
	AgentHook
	started chan struct{}
	release chan struct{}
}

func (b *blockingFreezeHook) SetFreezeAnchor(_ context.Context, _ time.Time) error {
	close(b.started)
	<-b.release
	return nil
}

// TestStoreMocksStaged_ConcurrentCommitRetryWaitsForInFlightCommit proves that
// a retried CommitStoreMocks arriving while the first commit is still running
// Flush/Finalize/finalizeClientMocks waits on the in-flight commit and observes
// its result instead of returning ErrStoreMocksStageNotFound (404).
func TestStoreMocksStaged_ConcurrentCommitRetryWaitsForInFlightCommit(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)

	prevHooks := ActiveHooks
	bh := &blockingFreezeHook{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	ActiveHooks = bh
	defer func() { ActiveHooks = prevHooks }()

	a := strictAgent(true)
	id, err := a.BeginStoreMocks(ctx, models.StoreMocksBeginReq{FilteredCount: 1})
	if err != nil {
		t.Fatalf("BeginStoreMocks: %v", err)
	}
	hdr, dec := encodeBatch(t, 0, []*models.Mock{diskEligibleMock("pt-1", base)})
	if _, err := a.StoreMocksBatch(ctx, id, 0, hdr, dec); err != nil {
		t.Fatalf("StoreMocksBatch: %v", err)
	}

	err1Ch := make(chan error, 1)
	go func() {
		err1Ch <- a.CommitStoreMocks(ctx, id)
	}()

	// Wait until the first commit is actively inside finalizeClientMocks.
	<-bh.started

	// Launch a concurrent retry while the first commit is in flight.
	err2Ch := make(chan error, 1)
	go func() {
		err2Ch <- a.CommitStoreMocks(ctx, id)
	}()

	// Retry must block waiting for the in-flight commit, NOT immediately return 404.
	select {
	case err := <-err2Ch:
		t.Fatalf("concurrent commit retry returned early while commit was in flight: %v", err)
	case <-time.After(30 * time.Millisecond):
	}

	// Release the first commit; both callers must succeed.
	close(bh.release)

	if err := <-err1Ch; err != nil {
		t.Fatalf("first CommitStoreMocks failed: %v", err)
	}
	if err := <-err2Ch; err != nil {
		t.Fatalf("concurrent retried CommitStoreMocks failed: %v", err)
	}
	// A subsequent post-commit retry must also succeed idempotently.
	if err := a.CommitStoreMocks(ctx, id); err != nil {
		t.Fatalf("post-commit retried CommitStoreMocks failed: %v", err)
	}
}
