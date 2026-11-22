package testset

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

/*
These drive WriteTemplatedConfig — the code replay.go actually calls.

The previous test for this re-typed replay.go's struct literal by hand in
the test file and asserted on THAT, so it checked the test against
itself: deleting `Metadata: conf.Metadata` from replay.go left the whole
66-package suite green. That is the same shape as the bug it was written
to prevent — two places that must agree, with nothing checking that they
do.
*/

type recordingStore struct {
	read      *models.TestSet
	readErr   error
	written   *models.TestSet
	writeErr  error
	writeCall int
}

func (s *recordingStore) ReadForUpdate(_ context.Context, _ string) (*models.TestSet, error) {
	return s.read, s.readErr
}
func (s *recordingStore) Write(_ context.Context, _ string, ts *models.TestSet) error {
	s.writeCall++
	s.written = ts
	return s.writeErr
}
func (s *recordingStore) ReadSecret(_ context.Context, _ string) (map[string]interface{}, error) {
	return nil, nil
}

func fullTestSet() *models.TestSet {
	return &models.TestSet{
		PreScript:  "echo pre",
		PostScript: "echo post",
		AppCommand: "npm start",
		Template:   map[string]interface{}{"old": "value"},
		Secret:     map[string]interface{}{"token": "abc"},
		Metadata:   map[string]interface{}{"team": "payments"},
	}
}

func TestWriteTemplatedConfigKeepsEveryOtherField(t *testing.T) {
	// Write marshals the WHOLE struct over config.yaml, so anything the
	// written value omits is deleted from disk. Asserting field by field
	// would only cover the fields someone remembered; this asserts the
	// written value equals the read one except for Template, so a field
	// added to models.TestSet later is covered without touching this.
	store := &recordingStore{read: fullTestSet()}
	values := map[string]interface{}{"token": "{{string .token}}"}

	if err := WriteTemplatedConfig(context.Background(), store, "ts", values); err != nil {
		t.Fatalf("write: %v", err)
	}
	if store.writeCall != 1 {
		t.Fatalf("expected one write, got %d", store.writeCall)
	}

	want := fullTestSet()
	want.Template = values
	got := store.written
	if got == nil {
		t.Fatal("nothing was written")
	}
	if !reflect.DeepEqual(got, want) {
		// Compared WHOLE, so a new field on models.TestSet is covered
		// without touching this test — and a field dropped by a future
		// caller fails here rather than on a user's disk.
		t.Errorf("the write changed more than Template:\n got %+v\nwant %+v", *got, *want)
	}
}

func TestWriteTemplatedConfigDoesNotMutateTheCallersConfig(t *testing.T) {
	// replay.go keeps using `conf` after the write.
	store := &recordingStore{read: fullTestSet()}
	original := store.read
	if err := WriteTemplatedConfig(context.Background(), store, "ts", map[string]interface{}{"new": "v"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if original.Template["old"] != "value" {
		t.Fatalf("the caller's config was modified in place: %+v", original.Template)
	}
}

func TestWriteTemplatedConfigWithNothingOnDisk(t *testing.T) {
	// A test-set recorded before config.yaml existed reads as nil, and a
	// fresh config is then correct rather than an error.
	store := &recordingStore{}
	if err := WriteTemplatedConfig(context.Background(), store, "ts", map[string]interface{}{"a": "b"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if store.written == nil || store.written.Template["a"] != "b" {
		t.Fatalf("template not written: %+v", store.written)
	}
}

func TestWriteTemplatedConfigReportsAMissingStore(t *testing.T) {
	// replay.go nil-checks testSetConf at the read and did not at the
	// write, so a nil store panicked there instead of being reported.
	err := WriteTemplatedConfig(context.Background(), nil, "ts", nil)
	if err == nil {
		t.Fatal("a nil store must be reported, not panic")
	}
}

func TestWriteTemplatedConfigPropagatesAWriteFailure(t *testing.T) {
	store := &recordingStore{read: fullTestSet(), writeErr: errors.New("disk full")}
	err := WriteTemplatedConfig(context.Background(), store, "ts", nil)
	if err == nil {
		t.Fatal("a failed write must be reported")
	}
}

func TestWriteTemplatedConfigRefusesAnUnparseableConfig(t *testing.T) {
	// Read hands back a ZERO value for a config.yaml that did not decode,
	// so "read, mutate, write back" wrote that zero over the file: one
	// stray indent in a hand-edited config and preScript, postScript,
	// appCommand and metadata were all erased. ReadForUpdate reports it
	// instead, and the write must not happen at all.
	store := &recordingStore{readErr: errors.New("yaml: line 3: did not find expected key")}
	err := WriteTemplatedConfig(context.Background(), store, "ts", map[string]interface{}{"a": "b"})
	if err == nil {
		t.Fatal("a config that could not be parsed must not be overwritten")
	}
	if store.writeCall != 0 {
		t.Fatalf("wrote anyway (%d times)", store.writeCall)
	}
}

type valueStore struct{ written *models.TestSet }

func (s valueStore) ReadForUpdate(_ context.Context, _ string) (*models.TestSet, error) {
	return &models.TestSet{AppCommand: "npm start"}, nil
}
func (s valueStore) Write(_ context.Context, _ string, ts *models.TestSet) error {
	s.written = ts
	return nil
}

func TestWriteTemplatedConfigAcceptsAValueReceiverStore(t *testing.T) {
	// The nil guard called reflect.Value.IsNil unconditionally, which
	// PANICS on a struct — so a ConfigStore implemented on a value
	// receiver, which is ordinary Go, crashed the guard that exists to
	// stop a crash.
	if err := WriteTemplatedConfig(
		context.Background(), valueStore{}, "ts", map[string]interface{}{"a": "b"},
	); err != nil {
		t.Fatalf("a value-receiver store must be usable: %v", err)
	}
}

func TestWriteTemplatedConfigReportsATypedNilStore(t *testing.T) {
	var typed *recordingStore
	if err := WriteTemplatedConfig(context.Background(), typed, "ts", nil); err == nil {
		t.Fatal("a nil pointer inside a non-nil interface must be reported, not panic")
	}
}

// The full read-modify-write, end to end, over a config nobody can read.
func TestWriteTemplatedConfigDoesNotEraseAnUnreadableConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "ts")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "config.yaml")
	body := "preScript: echo pre\npostScript: echo post\nappCommand: npm start\n" +
		"metadata:\n    io.keploy.ui-join/v1:\n        captureId: cap_01HZY\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	db := New[*models.TestSet](zap.NewNop(), root)
	err := WriteTemplatedConfig(context.Background(), db, "ts",
		map[string]interface{}{"token": "{{string .token}}"})
	require.Error(t, err, "writing over an unreadable config must fail, not erase it")

	require.NoError(t, os.Chmod(path, 0o644))
	after, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, body, string(after), "the config on disk must be byte-identical")
}
