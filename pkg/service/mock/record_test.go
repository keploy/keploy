package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestRecordOverwriteDeletesMappingsBeforeRunnerStarts(t *testing.T) {
	var order []string
	mappingDB := &recordMappingDB{order: &order}
	instr := &recordInstrumentation{order: &order}
	mockDB := &recordMockDB{order: &order}
	store := &recordStore{order: &order}
	cfg := config.New()
	cfg.Command = "go test ./..."
	cfg.Mock.Name = "stale-map-demo"

	svc := &mockService{
		logger:          zap.NewNop(),
		instrumentation: instr,
		mockDB:          mockDB,
		mappingDB:       mappingDB,
		store:           store,
		config:          cfg,
	}

	require.NoError(t, svc.Record(context.Background()))
	require.True(t, mockDB.deleted)
	require.True(t, mappingDB.deleted)
	require.Equal(t, []string{"setup", "delete-mocks", "delete-mappings", "reset-counter", "get-outgoing", "run", "push", "notify-shutdown"}, order)
}

type recordInstrumentation struct {
	order *[]string
}

func (i *recordInstrumentation) Setup(context.Context, string, models.SetupOptions) error {
	*i.order = append(*i.order, "setup")
	return nil
}

func (i *recordInstrumentation) GetOutgoing(context.Context, models.OutgoingOptions) (<-chan *models.Mock, error) {
	*i.order = append(*i.order, "get-outgoing")
	ch := make(chan *models.Mock)
	close(ch)
	return ch, nil
}

func (i *recordInstrumentation) MockOutgoing(context.Context, models.OutgoingOptions) error {
	return nil
}
func (i *recordInstrumentation) StoreMocks(context.Context, []*models.Mock, []*models.Mock) error {
	return nil
}
func (i *recordInstrumentation) UpdateMockParams(context.Context, models.MockFilterParams) error {
	return nil
}
func (i *recordInstrumentation) GetConsumedMocks(context.Context) ([]models.MockState, error) {
	return nil, nil
}
func (i *recordInstrumentation) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) {
	return nil, nil
}

func (i *recordInstrumentation) Run(context.Context, models.RunOptions) models.AppError {
	*i.order = append(*i.order, "run")
	return models.AppError{AppErrorType: models.ErrAppStopped}
}

func (i *recordInstrumentation) MakeAgentReadyForDockerCompose(context.Context) error { return nil }
func (i *recordInstrumentation) NotifyGracefulShutdown(context.Context) error {
	*i.order = append(*i.order, "notify-shutdown")
	return nil
}

type recordMockDB struct {
	order   *[]string
	deleted bool
}

func (db *recordMockDB) InsertMock(context.Context, *models.Mock, string) error { return nil }
func (db *recordMockDB) DeleteMocksForSet(context.Context, string) error {
	db.deleted = true
	*db.order = append(*db.order, "delete-mocks")
	return nil
}
func (db *recordMockDB) GetFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (db *recordMockDB) GetUnFilteredMocks(context.Context, string, time.Time, time.Time, map[string]bool, map[string]bool) ([]*models.Mock, error) {
	return nil, nil
}
func (db *recordMockDB) ResetCounterID()    { *db.order = append(*db.order, "reset-counter") }
func (db *recordMockDB) SetCounterID(int64) {}

type recordMappingDB struct {
	order   *[]string
	deleted bool
}

func (db *recordMappingDB) DeleteMappingsForSet(context.Context, string) error {
	db.deleted = true
	*db.order = append(*db.order, "delete-mappings")
	return nil
}
func (db *recordMappingDB) UpsertBatch(context.Context, string, map[string][]models.MockEntry) error {
	return nil
}
func (db *recordMappingDB) Get(context.Context, string) (map[string][]models.MockEntry, bool, error) {
	return nil, false, nil
}

type recordStore struct {
	order *[]string
}

func (recordStore) Pull(context.Context, string) error { return nil }
func (s recordStore) Push(context.Context, string) error {
	*s.order = append(*s.order, "push")
	return nil
}
