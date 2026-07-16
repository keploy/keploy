package record

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.keploy.io/server/v3/pkg/platform/yaml/mockdb"
	"go.uber.org/zap"
)

// These tests pin the contract behind the go-memory-load-mongo "no_mocks" flake:
// a record session that is shutting down must still persist everything the agent
// has already streamed to it.
//
// Why the tail is special: recording stops on SIGINT, which cancels the root
// context immediately, but the agent's streams run on reqCtx — deliberately
// WithoutCancel'd — and keep delivering right through teardown (a graceful-
// shutdown notify of up to 10s, then an app drain of up to 30s). Every store
// call in that window used to run on the cancelled context and refuse to write.
// Mappings are emitted last (the agent resolves a test's mock range only once
// that test is done), so the tail of every endpoint landed exactly there: in CI,
// 21 of 327 tests were absent from mappings.yaml, and replay reported
// no_mocks/candidates:0 for precisely those tests.

// TestConsumeMappings_PersistsTailAfterShutdown asserts on mappings.yaml itself
// — the artifact replay actually reads. Accepting a mapping off the channel is
// worthless if the write then discards it.
func TestConsumeMappings_PersistsTailAfterShutdown(t *testing.T) {
	const testSetID = "test-set-0"
	dir := t.TempDir()

	r := &Recorder{
		logger:    zap.NewNop(),
		config:    &config.Config{},
		mappingDb: mapdb.New(zap.NewNop(), dir, "mappings"),
	}

	// The mock loop has already correlated these tempIDs.
	var correlationMap, asyncMockIDs sync.Map
	tail := []string{"post-orders-58", "post-orders-59", "post-orders-60"}
	for _, tn := range tail {
		correlationMap.Store("temp-"+tn, models.MockEntry{
			Name: "mock-" + tn,
			Kind: string(models.Mongo),
		})
	}

	mappings := make(chan models.TestMockMapping, len(tail))
	for _, tn := range tail {
		mappings <- models.TestMockMapping{TestName: tn, MockIDs: []string{"temp-" + tn}}
	}
	close(mappings)

	// Recording has been cancelled: the state every tail mapping is written in.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, r.consumeMappings(ctx, testSetID, mappings, &correlationMap, &asyncMockIDs))

	require.FileExists(t, filepath.Join(dir, testSetID, "mappings.yaml"),
		"no mappings.yaml on disk: every mapping written during shutdown was discarded "+
			"by the cancelled context, and replay reports no_mocks for the tail")

	// Read back through the same store replay uses, on a live context.
	saved, meaningful, err := mapdb.New(zap.NewNop(), dir, "mappings").Get(context.Background(), testSetID)
	require.NoError(t, err)
	require.True(t, meaningful, "mappings.yaml exists but holds no mock entries")

	for _, tn := range tail {
		assert.Len(t, saved[tn], 1,
			"test %q was streamed by the agent during shutdown and must be mapped in "+
				"mappings.yaml; dropping it is the go-memory-load-mongo no_mocks flake", tn)
	}
}

// TestConsumeMappings_UpsertsIntoExistingFile covers the path production actually
// takes. The sibling test starts from an empty dir, so it only exercises the
// create-file gate; by the time the tail arrives in a real run, mappings.yaml
// already holds hundreds of tests and the write goes through the read-modify-write
// path instead. Both must survive cancellation.
func TestConsumeMappings_UpsertsIntoExistingFile(t *testing.T) {
	const testSetID = "test-set-0"
	dir := t.TempDir()
	db := mapdb.New(zap.NewNop(), dir, "mappings")

	// An earlier, healthy part of the session — written while ctx was live.
	require.NoError(t, db.Upsert(context.Background(), testSetID, "post-orders-1",
		[]models.MockEntry{{Name: "mock-1", Kind: string(models.Mongo)}}))

	r := &Recorder{logger: zap.NewNop(), config: &config.Config{}, mappingDb: db}

	var correlationMap, asyncMockIDs sync.Map
	correlationMap.Store("temp-tail", models.MockEntry{Name: "mock-tail", Kind: string(models.Mongo)})

	mappings := make(chan models.TestMockMapping, 1)
	mappings <- models.TestMockMapping{TestName: "post-orders-60", MockIDs: []string{"temp-tail"}}
	close(mappings)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, r.consumeMappings(ctx, testSetID, mappings, &correlationMap, &asyncMockIDs))

	saved, _, err := mapdb.New(zap.NewNop(), dir, "mappings").Get(context.Background(), testSetID)
	require.NoError(t, err)
	assert.Len(t, saved["post-orders-60"], 1,
		"the tail mapping must be merged into the existing mappings.yaml during shutdown")
	assert.Len(t, saved["post-orders-1"], 1,
		"upserting the tail must not lose mappings written earlier in the session")
}

// TestMockStore_RefusesCancelledContext is the reason persistCtx exists, pinned
// at the store boundary.
//
// It documents why threading the recording context into a record-time write is a
// data-loss bug rather than a style preference — and it guards the second half of
// the failure: the mock consumer skips correlationMap.Store when its insert
// fails, so a dropped tail mock also strands the mapping that references it. The
// mapping is then uncorrelatable and no row is written for that test at all, which
// is the same no_mocks symptom by a different route.
func TestMockStore_RefusesCancelledContext(t *testing.T) {
	dir := t.TempDir()
	db := mockdb.New(zap.NewNop(), dir, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	mock := &models.Mock{
		Version: models.V1Beta1,
		Kind:    models.Mongo,
		Name:    "mock-tail",
		Spec:    models.MockSpec{Metadata: map[string]string{}},
	}

	err := db.InsertMock(ctx, mock, "test-set-0")
	require.Error(t, err,
		"if the mock store ever starts accepting a cancelled context this test is "+
			"obsolete; until then, record-time writes must not run on the recording context")
	require.ErrorIs(t, err, context.Canceled)

	// The same insert on a detached context — what Start now passes — persists.
	require.NoError(t, db.InsertMock(context.WithoutCancel(ctx), mock, "test-set-0"),
		"detaching the write from cancellation is what keeps the tail of a recording")
}
