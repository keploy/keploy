package replay

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// Under the proven switch, a set whose mappings.yaml names a mock under worker:
// that the recording does not have (for example, a typo from a hand edit) is
// refused before the run starts, reporting the mock and its place, rather than
// blaming the application at the end of the set with "the application never made...".
func TestRunTestSet_RefusesSetWhenWorkerMappingNamesAbsentMock(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010)}
	// mock-6 is absent from the recorded mocks
	s12MappingFile(t, h, nil, models.MockEntry{Name: "mock-6", Kind: string(models.HTTP)})

	status, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", false)
	require.Error(t, err)
	require.Equal(t, models.TestSetStatusFailed, status)
	require.Contains(t, err.Error(), "names mocks")
	require.Contains(t, err.Error(), "does not have: mock-6 (under worker:)")
	require.Contains(t, err.Error(), "If you edited mappings.yaml, correct those names; otherwise record the set again")
	require.NotContains(t, err.Error(), "the application never made")
	require.Zero(t, h.instr.runCalls(), "a refused set runs no test")
}

// A connection failure mock under worker: that exists in the recording is not refused.
func TestRunTestSet_AllowsConnectionFailureUnderWorker(t *testing.T) {
	setSwitch(t, true)
	fastWorkerWait(t)
	h := newPartialRunHarness(t, 2, 0)
	pinWindows(h)
	cf := &models.Mock{Name: "mock-cf", Kind: models.ConnectionFailure}
	h.mocks.filtered = []*models.Mock{httpAt("mock-a", 1010), cf}
	s12MappingFile(t, h, nil, models.MockEntry{Name: "mock-cf", Kind: string(models.ConnectionFailure)})

	_, err := h.replayer.RunTestSet(context.Background(), "test-set-0", "test-run-0", false)
	if err != nil {
		require.NotContains(t, err.Error(), "does not have: mock-cf")
	}
}

// refuseUnknownMocks names the mappings file present: mappings.json for a set stored as JSON,
// not mappings.yaml.
func TestRefuseUnknownMocks_NamesJSONMappingFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "set", "mappings.json"), []byte("{}"), 0o644))
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.config.Path = dir

	mp := &models.Mapping{Worker: []models.MockEntry{{Name: "mock-6"}}}
	err := h.replayer.refuseUnknownMocks(context.Background(), "set", mp, []string{"mock-6"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "/set/mappings.json names mocks")
	require.Contains(t, err.Error(), "If you edited mappings.json, correct those names")
	require.NotContains(t, err.Error(), "mappings.yaml")
	require.Contains(t, err.Error(), "does not have: mock-6 (under worker:)")
}

// refuseUnknownMocks lists multiple absent mocks with their respective places.
func TestRefuseUnknownMocks_MultipleAbsentMocksWithPlaces(t *testing.T) {
	h := newPartialRunHarness(t, 1, 0)
	mp := &models.Mapping{
		Worker: []models.MockEntry{{Name: "mock-6"}, {Name: "mock-8"}},
		Shared: []models.MockEntry{{Name: "mock-9"}},
	}
	err := h.replayer.refuseUnknownMocks(context.Background(), "set", mp, []string{"mock-6", "mock-8", "mock-9"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "names mocks")
	require.Contains(t, err.Error(), "mock-6 (under worker:)")
	require.Contains(t, err.Error(), "mock-8 (under worker:)")
	require.Contains(t, err.Error(), "mock-9 (under shared:)")
	require.Contains(t, err.Error(), "correct those names; otherwise record the set again")
}

type unreadReplayStore struct {
	MockDB
}

func (unreadReplayStore) SkippedMocks(context.Context, string) (string, map[string]models.SkippedMock) {
	return "", nil
}

// When the mocks file cannot be read again, the refusal does not call a listed name absent,
// but says the replay did not read them.
func TestRefuseUnknownMocks_UncheckedWhenMocksFileCannotBeRead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keploy")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "set"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "set", "mocks.yaml"), []byte("x"), 0o644))
	h := newPartialRunHarness(t, 1, 0)
	h.replayer.config.Path = dir
	h.replayer.mockDB = unreadReplayStore{MockDB: h.replayer.mockDB}

	mp := &models.Mapping{Worker: []models.MockEntry{{Name: "mock-6"}}}
	err := h.replayer.refuseUnknownMocks(context.Background(), "set", mp, []string{"mock-6"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "names mocks the replay did not read from")
	require.Contains(t, err.Error(), "Check that mocks.yaml has each of them, and that keploy can read it; otherwise")
	require.NotContains(t, err.Error(), "does not have")
	require.NotContains(t, err.Error(), "correct those names")
}
