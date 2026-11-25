package mock

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

type scopedDrainInstrumentation struct {
	*drainInstrumentation
	windows []models.ScopeWindow
}

func (s *scopedDrainInstrumentation) GetScopeWindows(context.Context) ([]models.ScopeWindow, error) {
	return s.windows, nil
}

type orderingMappingDB struct {
	mu      sync.Mutex
	events  []string
	deleted []string
}

func (o *orderingMappingDB) UpsertBatch(_ context.Context, testSetID string, _ map[string][]models.MockEntry) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "upsert:"+testSetID)
	return nil
}

func (o *orderingMappingDB) Get(_ context.Context, _ string) (map[string][]models.MockEntry, bool, error) {
	return nil, false, nil
}

func (o *orderingMappingDB) Delete(_ context.Context, testSetID string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, "delete:"+testSetID)
	o.deleted = append(o.deleted, testSetID)
	return nil
}

type nonDeletingMappingDB struct{}

func (nonDeletingMappingDB) UpsertBatch(context.Context, string, map[string][]models.MockEntry) error {
	return nil
}

func (nonDeletingMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return nil, false, nil
}

// TestRecord_ClearsStaleMappingsOnRerecord verifies that re-recording a mock set
// removes pre-existing per-test mappings from disk (and from subsequent Get calls)
// even when the new recording produces no scope windows.
func TestRecord_ClearsStaleMappingsOnRerecord(t *testing.T) {
	dir := t.TempDir()
	mdb := mapdb.New(zap.NewNop(), dir, "")
	ctx := context.Background()

	const setName = "rerecord-set"
	if err := mdb.UpsertBatch(ctx, setName, map[string][]models.MockEntry{
		"old-test": {{Name: "mock-0"}},
	}); err != nil {
		t.Fatalf("seed UpsertBatch: %v", err)
	}
	mappingFile := filepath.Join(dir, setName, "mappings.yaml")
	if _, err := os.Stat(mappingFile); err != nil {
		t.Fatalf("seeded mapping file missing: %v", err)
	}

	inst := &drainInstrumentation{
		out:       make(chan *models.Mock, 4),
		emitAfter: 10 * time.Millisecond,
	}
	db := &recordingMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = setName

	svc := New(zap.NewNop(), inst, db, mdb, FileStore{}, nil, cfg)
	if err := svc.Record(ctx); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()

	if _, err := os.Stat(mappingFile); !os.IsNotExist(err) {
		t.Fatalf("stale mapping file still exists after Record: %v", err)
	}
	got, ok, err := mdb.Get(ctx, setName)
	if err != nil {
		t.Fatalf("Get after Record: %v", err)
	}
	if ok || len(got) != 0 {
		t.Fatalf("expected no stale mappings after Record, got ok=%v mappings=%v", ok, got)
	}
}

// TestRecord_DeletesStaleMappingsBeforeUpsert verifies Record invokes
// MappingDeleter.Delete for the target mock set before writing new mappings
// via UpsertBatch.
func TestRecord_DeletesStaleMappingsBeforeUpsert(t *testing.T) {
	now := time.Now()
	inst := &scopedDrainInstrumentation{
		drainInstrumentation: &drainInstrumentation{
			out:       make(chan *models.Mock, 4),
			emitAfter: 10 * time.Millisecond,
		},
		windows: []models.ScopeWindow{
			{
				Name:  "TestFresh",
				Start: now.Add(-time.Second),
				End:   now.Add(time.Second),
			},
		},
	}
	db := &recordingMockDB{}
	mdb := &orderingMappingDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "order-set"

	svc := New(zap.NewNop(), inst, db, mdb, FileStore{}, nil, cfg)
	if err := svc.Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()

	mdb.mu.Lock()
	deleted := append([]string(nil), mdb.deleted...)
	events := append([]string(nil), mdb.events...)
	mdb.mu.Unlock()

	if !slices.Equal(deleted, []string{"order-set"}) {
		t.Fatalf("Delete calls = %v, want [order-set]", deleted)
	}
	wantEvents := []string{"delete:order-set", "upsert:order-set"}
	if !slices.Equal(events, wantEvents) {
		t.Fatalf("mapping events = %v, want %v", events, wantEvents)
	}
}

// TestDeleteMappings_HandlesNilAndNonDeleter verifies deleteMappings is safe
// when mappingDB is nil (logs nothing) or does not implement MappingDeleter
// (logs a warning without panicking).
func TestDeleteMappings_HandlesNilAndNonDeleter(t *testing.T) {
	ctx := context.Background()

	nilCore, nilLogs := observer.New(zapcore.WarnLevel)
	nilSvc := &mockService{logger: zap.New(nilCore)}
	nilSvc.deleteMappings(ctx, "set-a")
	if nilLogs.Len() != 0 {
		t.Fatalf("expected no warnings for nil mappingDB, got %d", nilLogs.Len())
	}

	nonDelCore, nonDelLogs := observer.New(zapcore.WarnLevel)
	nonDelSvc := &mockService{logger: zap.New(nonDelCore), mappingDB: nonDeletingMappingDB{}}
	nonDelSvc.deleteMappings(ctx, "set-b")
	entries := nonDelLogs.All()
	if len(entries) != 1 || !strings.Contains(entries[0].Message, "cannot delete") {
		t.Fatalf("expected one warning about non-deleting mapping store, got %v", entries)
	}
}
