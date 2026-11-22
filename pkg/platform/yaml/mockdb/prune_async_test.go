package mockdb

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// asyncTestMock is mock i of the fixture: an async epoch of lane at pos
// (models.AsyncMeta.AnchorPos), seq, answering value.
func asyncTestMock(i int, lane string, pos, seq int, value string) *models.Mock {
	m := pruneHTTPMock(i)
	m.Spec.HTTPReq.Method, m.Spec.HTTPReq.URL, m.Spec.HTTPReq.Body = "GET", "http://cfg/config?watch=true", ""
	m.Spec.HTTPResp.Body = value
	m.Spec.Async = &models.AsyncMeta{Lane: lane, Seq: seq, AnchorPos: pos}
	return m
}

// The prune keeps every async epoch a replay serves, consumed or not: the
// replay serves a lane's timeline by the position the test set reached, and
// no epoch is ever consumed (async.Engine.Decide). It used to drop every epoch
// recorded after the first tests: the next replay answered the tests after a
// value changed with the value before it. An epoch a later one at its
// position supersedes is never served, and goes.
func TestUpdateMocks_KeepsTheAsyncEpochsAReplayServes(t *testing.T) {
	fixture := []*models.Mock{
		asyncTestMock(0, "cfg", 0, 1, "v1-boot"), // mock-0: superseded at position 0 by mock-1
		asyncTestMock(1, "cfg", 0, 2, "v1"),
		asyncTestMock(2, "cfg", 3, 3, "v1"), // mock-2: superseded at position 3 by mock-3
		asyncTestMock(3, "cfg", 3, 4, "v1"),
		asyncTestMock(4, "cfg", 8, 5, "v2"),
		asyncTestMock(5, "other", 0, 1, "o1"), // another lane at the same position
		pruneHTTPMock(6),                      // not async, not consumed: goes
		pruneHTTPMock(7),                      // not async, consumed: stays
		asyncTestMock(8, "cfg", 10, 6, "v3"),  // mock-8, 9: a tie, which the replay orders as it loads them
		asyncTestMock(9, "cfg", 10, 6, "v3"),
	}
	keep := depsConsumed("mock-7")
	want := []string{"mock-1", "mock-3", "mock-4", "mock-5", "mock-7", "mock-8", "mock-9"}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, fixture, "")
			before := readDepsSet(t, dir)
			core, logs := observer.New(zapcore.DebugLevel)
			ys := New(zap.New(core), dir, "mocks")
			if err := ys.UpdateMocks(context.Background(), pruneSet, keep, pruneT0.Add(time.Hour), time.Time{}); err != nil {
				t.Fatal(err)
			}
			after := readDepsSet(t, dir)
			if names := depsNamesOf(after); !reflect.DeepEqual(names, want) {
				t.Fatalf("the prune kept %v, want %v: every async epoch the replay serves, and the consumed mock", names, want)
			}
			done := logs.FilterMessageSnippet("pruned mocks successfully").All()
			if len(done) != 1 {
				t.Fatalf("%d prune summaries logged, want 1", len(done))
			}
			if got := done[0].ContextMap()["keptAsAsyncEpochMocks"]; !reflect.DeepEqual(got, []interface{}{"mock-1", "mock-3", "mock-4", "mock-5", "mock-8", "mock-9"}) {
				t.Errorf("the prune log names %v as kept as an async epoch", got)
			}

			// What the replay serves, at every position, is what it served
			// before the prune.
			for _, lane := range []string{"cfg", "other"} {
				for pos := 0; pos <= 12; pos++ {
					if b, a := servedAt(before, lane, pos), servedAt(after, lane, pos); b != a {
						t.Errorf("lane %s, %d tests completed: the replay served %q before the prune and %q after", lane, pos, b, a)
					}
				}
			}
		})
	}
}

// servedAt is what the async engine serves lane, set's mocks loaded, with
// completed tests completed.
func servedAt(set map[string]*models.Mock, lane string, completed int) string {
	l := models.AsyncLane{Name: lane, Type: "http"}
	e := async.NewEngine(zap.NewNop(), []models.AsyncLane{l}, nil)
	var mocks []*models.Mock
	for i := 0; i < 64; i++ {
		if m, ok := set[fmt.Sprintf("mock-%d", i)]; ok {
			mocks = append(mocks, m)
		}
	}
	e.Load(mocks)
	for i := 0; i < completed; i++ {
		e.OnTestComplete()
	}
	rec, _, _ := e.Decide(context.Background(), l, &models.Mock{})
	if rec == nil {
		return "<none>"
	}
	return rec.Spec.HTTPResp.Body
}

// An async epoch kept because a replay serves it keeps what it requires, as
// any kept mock does.
func TestUpdateMocks_AKeptAsyncEpochKeepsWhatItRequires(t *testing.T) {
	prev := models.RegisterMockDeps(models.HTTP, depsByMetadata)
	t.Cleanup(func() { models.RegisterMockDeps(models.HTTP, prev) })
	epoch := asyncTestMock(1, "cfg", 2, 1, "v1")
	epoch.Spec.Metadata = map[string]string{"requires": "k"}
	fixture := []*models.Mock{
		linkedHTTPMock(0, "k", ""), // mock-0: provides k
		epoch,                      // mock-1: served at position 2, requires k
		linkedHTTPMock(2, "j", ""), // mock-2: provides what nothing kept requires
	}
	for _, format := range []string{"yaml", "json", "gob"} {
		t.Run(format, func(t *testing.T) {
			dir := t.TempDir()
			writePruneFixture(t, dir, format, fixture, "")
			ys := New(zap.NewNop(), dir, "mocks")
			if err := ys.UpdateMocks(context.Background(), pruneSet, depsConsumed(), pruneT0.Add(time.Hour), time.Time{}); err != nil {
				t.Fatal(err)
			}
			if names := depsNamesOf(readDepsSet(t, dir)); !reflect.DeepEqual(names, []string{"mock-0", "mock-1"}) {
				t.Fatalf("the prune kept %v, want mock-0 and mock-1", names)
			}
		})
	}
}
