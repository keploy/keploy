package mock

import (
	"context"
	"os"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestExportRecordStartHandsTheCommandTheStart(t *testing.T) {
	t.Setenv(models.RecordStartEnv, "")
	exportRecordStart(zap.NewNop(), "2026-10-12T12:29:14.123Z")
	if got := os.Getenv(models.RecordStartEnv); got != "2026-10-12T12:29:14.123Z" {
		t.Fatalf("%s = %q, want the recording's start", models.RecordStartEnv, got)
	}
}

func TestExportRecordStartClearsAnInheritedStart(t *testing.T) {
	// A replay of a recording made before the stamp must not leave the test
	// command anchored on a start some parent keploy exported.
	t.Setenv(models.RecordStartEnv, "2020-01-01T00:00:00Z")
	exportRecordStart(zap.NewNop(), "")
	if v, ok := os.LookupEnv(models.RecordStartEnv); ok {
		t.Fatalf("%s still set to %q", models.RecordStartEnv, v)
	}
}

// startsMockDB keeps the recording start each persisted mock carries.
type startsMockDB struct {
	recordingMockDB
	starts []string
}

func (s *startsMockDB) InsertMock(ctx context.Context, m *models.Mock, set string) error {
	s.mu.Lock()
	s.starts = append(s.starts, m.Spec.Metadata[models.RecordStartMetadataKey])
	s.mu.Unlock()
	return s.recordingMockDB.InsertMock(ctx, m, set)
}

func TestRecordStampsEveryMockWithTheStartItHandsTheCommand(t *testing.T) {
	t.Setenv(models.RecordStartEnv, "")
	inst := &drainInstrumentation{out: make(chan *models.Mock, 4), emitAfter: 50 * time.Millisecond}
	db := &startsMockDB{}
	cfg := &config.Config{}
	cfg.Mock.Name = "record-start-test"

	if err := New(zap.NewNop(), inst, db, nil, FileStore{}, nil, cfg).Record(context.Background()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	inst.wg.Wait()

	exported := os.Getenv(models.RecordStartEnv)
	if _, err := time.Parse(time.RFC3339Nano, exported); err != nil {
		t.Fatalf("%s = %q is not an RFC 3339 time: %v", models.RecordStartEnv, exported, err)
	}
	if len(db.starts) != 2 {
		t.Fatalf("persisted %d mocks, want 2", len(db.starts))
	}
	for i, s := range db.starts {
		if s != exported {
			t.Fatalf("mock %d carries start %q, want the exported %q", i, s, exported)
		}
	}
}
