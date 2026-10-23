package mockdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// readOnDisk reads set-0 of dir with its payloads on disk and returns its
// mocks that left one there.
func readOnDisk(t *testing.T, ys *MockYaml) (models.TestSetMocks, []*models.Mock) {
	t.Helper()
	set, err := ys.GetTestSetMocksPayloadOnDisk(context.Background(), "set-0", models.BaseTime, time.Now(), nil, nil)
	require.NoError(t, err)
	var left []*models.Mock
	for _, m := range append(append([]*models.Mock(nil), set.Filtered...), set.Unfiltered...) {
		if m.PayloadOnDisk() {
			left = append(left, m)
		}
	}
	require.NotEmpty(t, left, "no mock left its payload on disk")
	return set, left
}

// The replay's mutation of a mock before sending it (a decryption) is done to
// each payload as it is read back: the mutator gets the mock with its payload,
// and what it changes is what is sent.
func TestPayloadMutatorChangesEachPayloadReadBack(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	set, left := readOnDisk(t, writeTestSetMocks(t, t.TempDir(), "yaml"))
	var calls atomic.Int32
	set.Payloads.SetPayloadMutator(func(_ context.Context, m *models.Mock) error {
		calls.Add(1)
		require.NotEmpty(t, m.Spec.MongoResponses)
		msg := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
		msg.Sections = []string{"mutated"}
		return nil
	})
	for _, m := range left {
		got, err := m.WithPayload(context.Background())
		require.NoError(t, err)
		assert.Equal(t, []string{"mutated"}, got.Spec.MongoResponses[0].Message.(*models.MongoOpMessage).Sections)
	}
	assert.Equal(t, int32(len(left)), calls.Load())

	failing := errors.New("decrypt failed")
	set.Payloads.SetPayloadMutator(func(context.Context, *models.Mock) error { return failing })
	_, err := left[0].WithPayload(context.Background())
	assert.ErrorIs(t, err, failing)
}

// The replay rewrites a set's mock file once the set has run (the prune, the
// noise write-back): a payload left in the file it replaced is not read from
// the new one, and the error says the set is over (models.ErrPayloadFileReplaced).
func TestPayloadOfAMockFileRewrittenSinceIsNotReadBack(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	for _, change := range []struct {
		name string
		do   func(t *testing.T, path string)
	}{
		{"replaced", func(t *testing.T, path string) {
			tmp := path + ".tmp"
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(tmp, data, 0o600))
			require.NoError(t, os.Rename(tmp, path))
		}},
		{"removed", func(t *testing.T, path string) {
			require.NoError(t, os.Remove(path))
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			dir := t.TempDir()
			_, left := readOnDisk(t, writeTestSetMocks(t, dir, "yaml"))
			change.do(t, filepath.Join(dir, "set-0", "mocks.yaml"))
			_, err := left[0].WithPayload(context.Background())
			assert.ErrorIs(t, err, models.ErrPayloadFileReplaced)
		})
	}
}

// A file changed in place since the read (the same file, other bytes where
// the mock was) is refused rather than read as another mock's payload.
func TestPayloadOfAMockFileChangedInPlaceIsNotReadBack(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	dir := t.TempDir()
	_, left := readOnDisk(t, writeTestSetMocks(t, dir, "yaml"))
	path := filepath.Join(dir, "set-0", "mocks.yaml")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte(strings.ReplaceAll(string(data), "name: mock-", "name: gone-")), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = left[0].WithPayload(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, models.ErrPayloadFileReplaced)
	assert.Contains(t, err.Error(), "changed after its mocks were read")
}

// A file that stops inside the separator after its last document: that
// document is read from its whole lines, and read back from the same ones.
func TestPayloadIsReadBackFromADocumentBeforeAnUnfinishedSeparator(t *testing.T) {
	t.Setenv("KEPLOY_MOCK_FORMAT", "")
	dir := t.TempDir()
	ys := writeTestSetMocks(t, dir, "yaml")
	path := filepath.Join(dir, "set-0", "mocks.yaml")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("--")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	whole, err := ys.GetTestSetMocks(context.Background(), "set-0", models.BaseTime, time.Now(), nil, nil)
	require.NoError(t, err)
	_, left := readOnDisk(t, ys)
	byName := map[string]*models.Mock{}
	for _, m := range append(append([]*models.Mock(nil), whole.Filtered...), whole.Unfiltered...) {
		byName[m.Name] = m
	}
	for _, m := range left {
		got := onTheWire(t, m)
		want := onTheWire(t, byName[m.Name])
		assert.Equal(t, want.Spec.MongoResponses, got.Spec.MongoResponses, m.Name)
	}
}
