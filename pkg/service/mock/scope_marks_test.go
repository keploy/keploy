package mock

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/mapdb"
	"go.uber.org/zap"
)

func marksInstr(t *testing.T, marks ...models.ScopeWindow) *runnerInstr {
	t.Helper()
	r := newRunnerInstr(t, "")
	r.windows = marks
	return r
}

func mark(name string, start, end time.Duration) models.ScopeWindow {
	return models.ScopeWindow{Name: name, Start: runnerT0.Add(start), End: runnerT0.Add(end)}
}

func sequentialMarks() []models.ScopeWindow {
	return []models.ScopeWindow{
		mark("orders/e2e.TestA", 0, 10*time.Millisecond),
		mark("orders/e2e.TestC", 10*time.Millisecond, 20*time.Millisecond),
	}
}

func flowMarks() []models.ScopeWindow {
	return []models.ScopeWindow{
		mark("e2e/orders.TestZ", 0, time.Second),
		mark("e2e/orders.TestFlow/create", 3*time.Second, 4*time.Second),
		mark("e2e/orders.TestFlow/delete", 6*time.Second, 7*time.Second),
		mark("e2e/orders.TestFlow", 2*time.Second, 8*time.Second),
	}
}

func TestRecordMapsEachTestByItsOwnMarks(t *testing.T) {
	instr := marksInstr(t, append(sequentialMarks(), mark("fixture.Setup", 30*time.Millisecond, 40*time.Millisecond))...)
	instr.mocks = []*models.Mock{
		mockAt("mock-0", runnerT0.Add(5*time.Millisecond)),
		mockAt("mock-1", runnerT0.Add(35*time.Millisecond)),
	}
	mapDB := mapdb.New(zap.NewNop(), t.TempDir(), "")
	require.NoError(t, recordSet(t, instr, mapDB, nil))
	got, _, err := mapDB.Get(context.Background(), "set")
	require.NoError(t, err)
	require.Equal(t, []string{"mock-0"}, mappedNames(t, mapDB)["orders/e2e.TestA"])
	require.Equal(t, []string{"mock-1"}, mappedNames(t, mapDB)["fixture.Setup"])
	mocks, listed := got["orders/e2e.TestC"]
	require.True(t, listed, "a test that marked itself is listed even though it made no call")
	require.Empty(t, mocks)
}

func TestRecordWithoutMarksIsOnePool(t *testing.T) {
	instr := newRunnerInstr(t, "")
	instr.mocks = []*models.Mock{mockAt("mock-0", runnerT0.Add(5*time.Millisecond))}
	dir := t.TempDir()
	require.NoError(t, recordSet(t, instr, mapdb.New(zap.NewNop(), dir, ""), nil))
	_, err := os.Stat(filepath.Join(dir, "set", "mappings.yaml"))
	require.True(t, os.IsNotExist(err), "no marks, no per-test mapping: the whole run is one pool")
}

func TestStepWindowsComeFromSubtestMarks(t *testing.T) {
	steps := stepWindows(append(flowMarks(), mark("e2e/orders.TestFlow/create/nested", 3100*time.Millisecond, 3200*time.Millisecond)))
	var names []string
	for _, w := range steps {
		names = append(names, w.Name)
	}
	require.Equal(t, []string{"create", "delete", "create"}, names)
	require.Equal(t, "", containing(steps, runnerT0.Add(flowSetupAt)), "a call in the flow's own body has no step")
	require.Equal(t, "delete", containing(steps, runnerT0.Add(6500*time.Millisecond)))
}

func TestRepeatedScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		windows []models.ScopeWindow
		existed bool
		want    string
	}{
		{"one run each", flowMarks(), true, ""},
		{"a test began twice over a recording that existed", append(sequentialMarks(), mark("orders/e2e.TestA", 30*time.Millisecond, 40*time.Millisecond)), true,
			"Recording stopped: TestA ran 2 times in orders/e2e.\nEach test in a folder needs its own name. Rename one of them (or drop -count=2), then run keploy mock record again.\nNothing was changed: your previous recording is kept."},
		{"a test began three times in a new set", append(sequentialMarks(), mark("orders/e2e.TestA", 30*time.Millisecond, 40*time.Millisecond), mark("orders/e2e.TestA", 50*time.Millisecond, 60*time.Millisecond)), false,
			"Recording stopped: TestA ran 3 times in orders/e2e.\nEach test in a folder needs its own name. Rename one of them (or drop -count=3), then run keploy mock record again.\nNothing was saved."},
		{"the same subtest name under two tests is fine", []models.ScopeWindow{mark("p.TestA", 0, 1), mark("p.TestA/one", 0, 1), mark("p.TestB", 2, 3), mark("p.TestB/one", 2, 3)}, true, ""},
		{"the same name in two packages is fine", []models.ScopeWindow{mark("a.TestX", 0, 1), mark("b.TestX", 2, 3)}, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := repeatedScope(tc.windows, tc.existed)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.want)
			require.ErrorIs(t, err, ErrRecordRefused)
		})
	}
}
