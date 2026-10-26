package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A read before a write the replay has not made (integrations.ReadBeforeUnmadeWrite)
// on the agent's manager. The recordings say in their metadata whether they
// are writes ("op") and which request they are ("q"), as a parser would
// tell from its protocol.

var unmadeBase = time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)

func unmadeRecording(name string, kind models.Kind, ms int, op, q string) *models.Mock {
	at := unmadeBase.Add(time.Duration(ms) * time.Millisecond)
	return &models.Mock{
		Name: name,
		Kind: kind,
		Spec: models.MockSpec{
			Metadata:         map[string]string{"op": op, "q": q},
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(time.Millisecond),
		},
		TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest},
	}
}

func unmadeIsWrite(mk *models.Mock) bool { return mk.Spec.Metadata["op"] == "write" }

func unmadeSame(q string) func(*models.Mock) bool {
	return func(mk *models.Mock) bool { return mk.Spec.Metadata["q"] == q }
}

// unmadeStage stages staged and opens test with owned on a manager under
// exact serving.
func unmadeStage(t *testing.T, staged []*models.Mock, test string, owned ...*models.Mock) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindowThreeTier(staged, nil, nil, models.BaseTime, time.Now())
	mm.ServeExactPool(models.PoolKeyNone, nil)
	if test != "" {
		mm.ServeExactTest(test, owned)
	}
	return mm
}

// viewCopy is the copy of the recording named name a parser reads from the
// manager now.
func viewCopy(t *testing.T, db integrations.MockMemDb, name string) *models.Mock {
	t.Helper()
	got, err := db.GetFilteredMocks()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, mk := range got {
		if mk.Name == name {
			return mk
		}
	}
	t.Fatalf("the store shows no %s", name)
	return nil
}

func answer(t *testing.T, db integrations.MockMemDb, chosen *models.Mock, q string) integrations.BeforeUnmadeWrite {
	t.Helper()
	got, err := integrations.ReadBeforeUnmadeWrite(db, chosen, unmadeIsWrite, unmadeSame(q))
	if err != nil {
		t.Fatalf("ReadBeforeUnmadeWrite: %v", err)
	}
	return got
}

func answerName(b integrations.BeforeUnmadeWrite) string {
	if b.Answer == nil {
		return ""
	}
	return b.Answer.Name
}

// The block-list shape: test-1 read the list; test-2 recorded the block,
// then the list holding the client. A read of the list before the block is
// answered with test-1's, a copy; the list test-2 recorded after the block
// is still unanswered, and answers once the block is made. With no earlier
// recording of the list the read misses, naming the write.
func TestUnmadeWrite_AReadBeforeTheBlockIsAnsweredFromBeforeIt(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block := unmadeRecording("block-t2", models.Mongo, 1000, "write", "block")
	list2 := unmadeRecording("list-t2", models.Mongo, 1010, "read", "list")

	t.Run("answered from the earlier test", func(t *testing.T) {
		mm := unmadeStage(t, []*models.Mock{list1, block, list2}, "test-2", block, list2)
		got := answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
		if got.Write == nil || got.Write.Name != "block-t2" || answerName(got) != "list-t1" {
			t.Fatalf("got write %v answer %q; want block-t2 unmade and list-t1 to answer", got.Write, answerName(got))
		}
		for _, mk := range mm.StagedMocks() {
			if mk == got.Answer {
				t.Fatal("the answer is the staged recording itself; want a copy")
			}
		}
		// Nothing was consumed: the list recorded after the block is still
		// the test's, and answers once the block is made.
		if !mm.MarkMockAsUsed(*viewCopy(t, mm, "block-t2")) {
			t.Fatal("the block was refused")
		}
		after := viewCopy(t, mm, "list-t2")
		if got := answer(t, mm, after, "list"); got.Applies() {
			t.Fatalf("the read after the block is still held back by %v", got.Write)
		}
		if !mm.MarkMockAsUsed(*after) {
			t.Fatal("the recorded read after the block was refused: serving the earlier answer consumed it")
		}
	})
	t.Run("no earlier recording: a miss naming the write", func(t *testing.T) {
		mm := unmadeStage(t, []*models.Mock{block, list2}, "test-2", block, list2)
		got := answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
		if !got.Applies() || got.Answer != nil || !strings.Contains(got.Note(), "an extra read before block-t2") {
			t.Fatalf("got applies %v answer %q note %q; want a miss naming block-t2", got.Applies(), answerName(got), got.Note())
		}
	})
	t.Run("a recording at the write's instant or after is no answer", func(t *testing.T) {
		same := unmadeRecording("list-at-block", models.Mongo, 1000, "read", "list")
		mm := unmadeStage(t, []*models.Mock{list1, same, block, list2}, "test-2", block, list2)
		if got := answerName(answer(t, mm, viewCopy(t, mm, "list-t2"), "list")); got != "list-t1" {
			t.Fatalf("answered by %q; want list-t1", got)
		}
	})
	t.Run("the newest earlier recording", func(t *testing.T) {
		list0 := unmadeRecording("list-t0", models.Mongo, -500, "read", "list")
		mm := unmadeStage(t, []*models.Mock{list0, list1, block, list2}, "test-2", block, list2)
		if got := answerName(answer(t, mm, viewCopy(t, mm, "list-t2"), "list")); got != "list-t1" {
			t.Fatalf("answered by %q; want list-t1, the newest before the block", got)
		}
	})
	t.Run("another request or another kind is no answer", func(t *testing.T) {
		other := unmadeRecording("users-t1", models.Mongo, 100, "read", "users")
		http := unmadeRecording("list-http", models.HTTP, 200, "read", "list")
		mm := unmadeStage(t, []*models.Mock{list1, other, http, block, list2}, "test-2", block, list2)
		if got := answerName(answer(t, mm, viewCopy(t, mm, "list-t2"), "list")); got != "list-t1" {
			t.Fatalf("answered by %q; want list-t1", got)
		}
	})
}

// Two writes: a read between them is answered with the test's own read from
// between them, though a recording another test made later (beside it)
// precedes the second write too.
func TestUnmadeWrite_AReadBetweenTwoWritesIsAnsweredFromBetweenThem(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	w1 := unmadeRecording("w1", models.Mongo, 100, "write", "block-a")
	ra := unmadeRecording("ra", models.Mongo, 110, "read", "list")
	beside := unmadeRecording("list-beside", models.Mongo, 150, "read", "list")
	w2 := unmadeRecording("w2", models.Mongo, 200, "write", "block-b")
	rb := unmadeRecording("rb", models.Mongo, 210, "read", "list")
	mm := unmadeStage(t, []*models.Mock{list1, w1, ra, beside, w2, rb}, "test-2", w1, ra, w2, rb)
	for _, name := range []string{"w1", "ra"} {
		if !mm.MarkMockAsUsed(*viewCopy(t, mm, name)) {
			t.Fatalf("%s was refused", name)
		}
	}
	got := answer(t, mm, viewCopy(t, mm, "rb"), "list")
	if got.Write == nil || got.Write.Name != "w2" || answerName(got) != "ra" {
		t.Fatalf("got write %v answer %q; want w2 unmade and ra to answer", got.Write, answerName(got))
	}
}

// What is no read before one of the open test's own unmade writes keeps its
// choice.
func TestUnmadeWrite_WhatIsNoReadBeforeAnUnmadeWriteKeepsItsChoice(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block := unmadeRecording("block-t2", models.Mongo, 1000, "write", "block")
	list2 := unmadeRecording("list-t2", models.Mongo, 1010, "read", "list")
	all := []*models.Mock{list1, block, list2}
	for _, tc := range []struct {
		name string
		run  func(t *testing.T) integrations.BeforeUnmadeWrite
	}{
		{"a read after the write", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "test-2", block, list2)
			if !mm.MarkMockAsUsed(*viewCopy(t, mm, "block-t2")) {
				t.Fatal("the block was refused")
			}
			return answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
		}},
		{"the write itself", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "test-2", block, list2)
			return answer(t, mm, viewCopy(t, mm, "block-t2"), "block")
		}},
		{"exact serving off", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SetMocksWithWindowThreeTier(all, nil, nil, models.BaseTime, time.Now())
			return answer(t, mm, list2, "list")
		}},
		{"no test open", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "test-2", block, list2)
			chosen := viewCopy(t, mm, "list-t2")
			mm.ServeExactPool(models.PoolKeyNone, nil)
			return answer(t, mm, chosen, "list")
		}},
		{"no test open, the next test's own in flight", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "")
			mm.ServeExactPool(models.PoolKeyNone, nil, block, list2)
			chosen := viewCopy(t, mm, "list-t2")
			if w, ok := mm.UnmadeWriteBefore(chosen, unmadeIsWrite); ok {
				t.Fatalf("between tests the manager says %v is unmade", w)
			}
			return answer(t, mm, chosen, "list")
		}},
		{"a recording the test does not own", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "test-2", block, list2)
			return answer(t, mm, list1, "list")
		}},
		{"a copy an earlier view of the test served", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "test-2", block, list2)
			chosen := viewCopy(t, mm, "list-t2")
			mm.ServeExactTest("test-2", []*models.Mock{block, list2})
			return answer(t, mm, chosen, "list")
		}},
		{"a recording that answered already", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "test-2", block, list2)
			chosen := viewCopy(t, mm, "list-t2")
			if !mm.MarkMockAsUsed(*chosen) {
				t.Fatal("list-t2 was refused")
			}
			return answer(t, mm, chosen, "list")
		}},
		{"a write of another kind", func(t *testing.T) integrations.BeforeUnmadeWrite {
			post := unmadeRecording("post", models.HTTP, 1000, "write", "post")
			mm := unmadeStage(t, []*models.Mock{list1, post, list2}, "test-2", post, list2)
			return answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
		}},
		{"a read the test shares with the tests beside it", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "")
			mm.ServeExactTestConcurrent("test-2", []*models.Mock{block}, []*models.Mock{list2})
			return answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
		}},
		{"held to a staging that moved", func(t *testing.T) integrations.BeforeUnmadeWrite {
			mm := unmadeStage(t, all, "test-2", block, list2)
			held, _ := mm.HoldToRound(mm.MarkCallRound())
			chosen := viewCopy(t, mm, "list-t2")
			if got := answer(t, held, chosen, "list"); !got.Applies() {
				t.Fatal("the held store did not apply the rule before the staging moved: the row tests nothing")
			}
			mm.bumpStagingEpoch()
			return answer(t, held, chosen, "list")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.run(t); got.Applies() || got.Answer != nil {
				t.Fatalf("got write %v answer %q; want the choice kept", got.Write, answerName(got))
			}
		})
	}
}

// A connection's scoped view forwards the rule to the manager.
func TestUnmadeWrite_AScopedViewForwardsTheRule(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block := unmadeRecording("block-t2", models.Mongo, 1000, "write", "block")
	list2 := unmadeRecording("list-t2", models.Mongo, 1010, "read", "list")
	mm := unmadeStage(t, []*models.Mock{list1, block, list2}, "test-2", block, list2)
	scoped := &scopedMockDb{MockMemDb: mm, live: true}
	if got := answerName(answer(t, scoped, viewCopy(t, scoped, "list-t2"), "list")); got != "list-t1" {
		t.Fatalf("answered by %q through the scoped view; want list-t1", got)
	}
}

// The manager asked of no recording, with no write classifier, or of a
// recording the open test does not own stamped as this view's, says no write
// is unmade.
func TestUnmadeWrite_TheManagerAskedOfNothingItCanTellSaysNone(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block := unmadeRecording("block-t2", models.Mongo, 1000, "write", "block")
	list2 := unmadeRecording("list-t2", models.Mongo, 1010, "read", "list")
	mm := unmadeStage(t, []*models.Mock{list1, block, list2}, "test-2", block, list2)
	chosen := viewCopy(t, mm, "list-t2")
	stranger := list1.ShallowCopy()
	stranger.TestModeInfo.ServedIn = chosen.TestModeInfo.ServedIn
	for name, ask := range map[string]func() (*models.Mock, bool){
		"no recording":        func() (*models.Mock, bool) { return mm.UnmadeWriteBefore(nil, unmadeIsWrite) },
		"no write classifier": func() (*models.Mock, bool) { return mm.UnmadeWriteBefore(chosen, nil) },
		"a stranger stamped as the view's": func() (*models.Mock, bool) {
			return mm.UnmadeWriteBefore(stranger, unmadeIsWrite)
		},
	} {
		if w, ok := ask(); ok || w != nil {
			t.Errorf("%s: the manager says %v is unmade", name, w)
		}
	}
	if w, ok := mm.UnmadeWriteBefore(chosen, unmadeIsWrite); !ok || w.Name != "block-t2" {
		t.Fatalf("the manager says %v (%v) of the test's own read; want block-t2: the rows test nothing", w, ok)
	}
}
