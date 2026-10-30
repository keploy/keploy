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

// unmadeGates is what a parser says of chosen: nil for a write (it reads
// nothing a write changes), else whether a recording is a write that can
// change what chosen reads: one of the same namespace ("ns"), or of none
// named (its reach unknown: it holds every read).
func unmadeGates(chosen *models.Mock) func(*models.Mock) bool {
	if chosen == nil || unmadeIsWrite(chosen) {
		return nil
	}
	ns := chosen.Spec.Metadata["ns"]
	return func(w *models.Mock) bool {
		wns := w.Spec.Metadata["ns"]
		return unmadeIsWrite(w) && (wns == "" || ns == "" || wns == ns)
	}
}

// inNs is mk read or written in namespace ns.
func inNs(mk *models.Mock, ns string) *models.Mock {
	mk.Spec.Metadata["ns"] = ns
	return mk
}

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
	got, err := db.GetPerTestMocksInWindow()
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
	got, err := integrations.ReadBeforeUnmadeWrite(db, chosen, unmadeGates(chosen), unmadeSame(q))
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

// Two writes of the open test, neither made by the replay: the read is held
// to the first of them, in recorded order, and answered from before it. A
// read of the same request the background recorded between the two writes
// holds the state of the first write, which the replay has not made: it
// never answers, with an earlier test's read before the first write or
// without one (then the read is a miss that names the first write).
func TestUnmadeWrite_TwoUnmadeWritesHoldTheReadToTheFirst(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block1 := unmadeRecording("block-c1", models.Mongo, 1000, "write", "block")
	polled := unmadeRecording("bg-list", models.Mongo, 1500, "read", "list")
	polled.TestModeInfo.Lifetime = models.LifetimeSession
	block2 := unmadeRecording("block-c2", models.Mongo, 2000, "write", "block")
	list2 := unmadeRecording("list-t2", models.Mongo, 2010, "read", "list")
	for _, tc := range []struct {
		name   string
		staged []*models.Mock
		answer string
	}{
		{"an earlier test's read, and the background's between the writes", []*models.Mock{list1, block1, polled, block2, list2}, "list-t1"},
		{"only the background's read between the writes", []*models.Mock{block1, polled, block2, list2}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mm := unmadeStage(t, tc.staged, "test-2", block1, block2, list2)
			got := answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
			if got.Write == nil || got.Write.Name != "block-c1" {
				t.Fatalf("held by %v; want block-c1, the first write the replay has not made", got.Write)
			}
			if answerName(got) != tc.answer {
				t.Fatalf("answered by %q; want %q: bg-list holds block c1, which the replay has not made", answerName(got), tc.answer)
			}
			if tc.answer == "" && !strings.Contains(got.Note(), "an extra read before block-c1") {
				t.Fatalf("the miss says %q; want it to name block-c1", got.Note())
			}
		})
	}
}

// Another test's recording chosen for a read (by the pairing, or the
// background's exact read) is held to the first of the open test's unmade
// writes recorded before it, and answered from before that write, never by
// the background's read between the two writes.
func TestUnmadeWrite_AnotherTestsRecordingAfterTwoUnmadeWritesIsHeldToTheFirst(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block1 := unmadeRecording("block-c1", models.Mongo, 1000, "write", "block")
	polled := unmadeRecording("bg-list", models.Mongo, 1500, "read", "list")
	polled.TestModeInfo.Lifetime = models.LifetimeSession
	block2 := unmadeRecording("block-c2", models.Mongo, 2000, "write", "block")
	other := unmadeRecording("users-t2", models.Mongo, 2005, "read", "users")
	list3 := unmadeRecording("list-t3", models.Mongo, 3010, "read", "list")
	mm := unmadeStage(t, []*models.Mock{list1, block1, polled, block2, other, list3}, "test-2", block1, block2, other)
	got := answer(t, mm, list3, "list")
	if got.Write == nil || got.Write.Name != "block-c1" || answerName(got) != "list-t1" {
		t.Fatalf("held by %v answered by %q; want block-c1 and list-t1", got.Write, answerName(got))
	}
}

// What is no read before one of the open test's own unmade writes keeps its
// choice.
// Only a write that can change what the read reads holds it: the parser
// says which (gates). A write elsewhere the replay has not made, recorded
// before the read, leaves the read its own recording, and the write the
// read is held to is the first of those that can change it, past any
// write elsewhere recorded before that one.
func TestUnmadeWrite_OnlyAWriteThatCanChangeTheReadHoldsIt(t *testing.T) {
	list1 := inNs(unmadeRecording("list-t1", models.Mongo, 0, "read", "list"), "oauth.blocklist")
	audit := inNs(unmadeRecording("audit-t2", models.Mongo, 1000, "write", "audit"), "oauth.audit")
	block := inNs(unmadeRecording("block-t2", models.Mongo, 1005, "write", "block"), "oauth.blocklist")
	list2 := inNs(unmadeRecording("list-t2", models.Mongo, 1010, "read", "list"), "oauth.blocklist")

	mm := unmadeStage(t, []*models.Mock{list1, audit, list2}, "test-2", audit, list2)
	if got := answer(t, mm, viewCopy(t, mm, "list-t2"), "list"); got.Applies() {
		t.Fatalf("a read held to %s, a write to another collection; want its own recording", got.Write.Name)
	}

	mm = unmadeStage(t, []*models.Mock{list1, audit, block, list2}, "test-2", audit, block, list2)
	got := answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
	if !got.Applies() || got.Write.Name != "block-t2" || answerName(got) != "list-t1" {
		t.Fatalf("held to %v answered by %q; want block-t2, answered by list-t1", got.Write, answerName(got))
	}
}

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
		{"a recording of another test made after the write, once the write is made", func(t *testing.T) integrations.BeforeUnmadeWrite {
			list3 := unmadeRecording("list-t3", models.Mongo, 3010, "read", "list")
			mm := unmadeStage(t, []*models.Mock{list1, block, list2, list3}, "test-2", block, list2)
			if !mm.MarkMockAsUsed(*viewCopy(t, mm, "block-t2")) {
				t.Fatal("the block was refused")
			}
			return answer(t, mm, list3, "list")
		}},
		{"a recording of another test at the write's instant", func(t *testing.T) integrations.BeforeUnmadeWrite {
			at := unmadeRecording("list-at-block", models.Mongo, 1000, "read", "list")
			mm := unmadeStage(t, []*models.Mock{list1, at, block, list2}, "test-2", block, list2)
			return answer(t, mm, at, "list")
		}},
		{"after a write the test shares with the tests beside it", func(t *testing.T) integrations.BeforeUnmadeWrite {
			shared := unmadeRecording("block-shared", models.Mongo, 1000, "write", "block")
			users := unmadeRecording("users-t2", models.Mongo, 900, "read", "users")
			list3 := unmadeRecording("list-t3", models.Mongo, 3010, "read", "list")
			mm := unmadeStage(t, []*models.Mock{list1, users, shared, list3}, "")
			mm.ServeExactTestConcurrent("test-2", []*models.Mock{users}, []*models.Mock{shared})
			return answer(t, mm, list3, "list")
		}},
		{"a read the test shares with the tests beside it, recorded before the write", func(t *testing.T) integrations.BeforeUnmadeWrite {
			shared := unmadeRecording("list-shared", models.Mongo, 500, "read", "list")
			mm := unmadeStage(t, []*models.Mock{shared, block, list2}, "")
			mm.ServeExactTestConcurrent("test-2", []*models.Mock{block, list2}, []*models.Mock{shared})
			return answer(t, mm, viewCopy(t, mm, "list-shared"), "list")
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

// Every answer step is held to the open test's unmade writes, whoever
// recorded the answer: a read recorded only after the block, by another
// test, by the background, or shared with the tests beside the open one at
// record, is answered from before the block (list-t1, a copy), consuming
// nothing, or misses naming it.
func TestUnmadeWrite_ARecordingNotTheTestsOwnMadeAfterTheWriteIsNoAnswer(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block := unmadeRecording("block-t2", models.Mongo, 1000, "write", "block")
	other := unmadeRecording("users-t2", models.Mongo, 1005, "read", "users")
	for _, tc := range []struct {
		name  string
		early bool
		stage func(t *testing.T, staged []*models.Mock) (*MockManager, *models.Mock)
	}{
		{"another test's later recording", true, func(t *testing.T, staged []*models.Mock) (*MockManager, *models.Mock) {
			list3 := unmadeRecording("list-t3", models.Mongo, 3010, "read", "list")
			return unmadeStage(t, append(staged, list3), "test-2", block, other), list3
		}},
		{"a background recording", true, func(t *testing.T, staged []*models.Mock) (*MockManager, *models.Mock) {
			polled := unmadeRecording("bg-list", models.Mongo, 1020, "read", "list")
			polled.TestModeInfo.Lifetime = models.LifetimeSession
			return unmadeStage(t, append(staged, polled), "test-2", block, other), polled
		}},
		{"a read the test shares with the tests beside it", true, func(t *testing.T, staged []*models.Mock) (*MockManager, *models.Mock) {
			shared := unmadeRecording("list-shared", models.Mongo, 1010, "read", "list")
			mm := unmadeStage(t, append(staged, shared), "")
			mm.ServeExactTestConcurrent("test-2", []*models.Mock{block, other}, []*models.Mock{shared})
			return mm, viewCopy(t, mm, "list-shared")
		}},
		{"another test's later recording, no earlier one", false, func(t *testing.T, staged []*models.Mock) (*MockManager, *models.Mock) {
			list3 := unmadeRecording("list-t3", models.Mongo, 3010, "read", "list")
			return unmadeStage(t, append(staged, list3), "test-2", block, other), list3
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staged := []*models.Mock{block, other}
			if tc.early {
				staged = append([]*models.Mock{list1}, staged...)
			}
			mm, chosen := tc.stage(t, staged)
			got := answer(t, mm, chosen, "list")
			if got.Write == nil || got.Write.Name != "block-t2" {
				t.Fatalf("got write %v; want block-t2 unmade before %s", got.Write, chosen.Name)
			}
			if !tc.early {
				if got.Answer != nil || !strings.Contains(got.Note(), "an extra read before block-t2") {
					t.Fatalf("got answer %q note %q; want a miss naming block-t2", answerName(got), got.Note())
				}
				return
			}
			if answerName(got) != "list-t1" {
				t.Fatalf("answered by %q; want list-t1, the newest before the block", answerName(got))
			}
			// The test's own recordings are untouched: the block and the
			// other read still answer.
			for _, name := range []string{"block-t2", "users-t2"} {
				if !mm.MarkMockAsUsed(*viewCopy(t, mm, name)) {
					t.Fatalf("%s was refused: serving the earlier answer consumed it", name)
				}
			}
			if got := answer(t, mm, chosen, "list"); got.Applies() {
				t.Fatalf("after the block %s is still held back by %v", chosen.Name, got.Write)
			}
		})
	}
}

// A read before a write the replay has not made is never answered from
// before a write the replay has made (Made): the open test's own, made in
// this attempt, or any other recording a call of the set consumed (an
// earlier test's, one a test shares). test-1 read the list; test-2
// recorded block c1, block c2, then the list. With no read recorded
// between the made write and the unmade one, the read misses, naming both.
func TestUnmadeWrite_NeverFromBeforeAWriteTheReplayHasMade(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block0 := unmadeRecording("block-c0", models.Mongo, 500, "write", "block-c0")
	blockC1 := unmadeRecording("block-c1", models.Mongo, 1000, "write", "block-c1")
	blockC2 := unmadeRecording("block-c2", models.Mongo, 2000, "write", "block-c2")
	listC1C2 := unmadeRecording("list-c1c2", models.Mongo, 2010, "read", "list")
	use := func(t *testing.T, mm *MockManager, name string) {
		t.Helper()
		if !mm.MarkMockAsUsed(*viewCopy(t, mm, name)) {
			t.Fatalf("%s was refused", name)
		}
	}
	miss := func(t *testing.T, db integrations.MockMemDb, made, unmade string) {
		t.Helper()
		got := answer(t, db, viewCopy(t, db, "list-c1c2"), "list")
		if !got.Applies() || got.Write.Name != unmade || got.Answer != nil {
			t.Fatalf("got write %v answer %q; want a miss before %s", got.Write, answerName(got), unmade)
		}
		if got.Made == nil || got.Made.Name != made {
			t.Fatalf("made = %v; want %s", got.Made, made)
		}
		if want := "no read was recorded between " + made + " and " + unmade + "."; !strings.Contains(got.Note(), want) {
			t.Fatalf("Note() = %q; want it to say %q", got.Note(), want)
		}
	}
	t.Run("the test's own write, made", func(t *testing.T) {
		mm := unmadeStage(t, []*models.Mock{list1, blockC1, blockC2, listC1C2}, "test-2", blockC1, blockC2, listC1C2)
		use(t, mm, "block-c1")
		miss(t, mm, "block-c1", "block-c2")
	})
	t.Run("through a connection's scoped view", func(t *testing.T) {
		mm := unmadeStage(t, []*models.Mock{list1, blockC1, blockC2, listC1C2}, "test-2", blockC1, blockC2, listC1C2)
		use(t, mm, "block-c1")
		miss(t, &scopedMockDb{MockMemDb: mm, live: true}, "block-c1", "block-c2")
	})
	t.Run("an earlier test's write, made", func(t *testing.T) {
		mm := unmadeStage(t, []*models.Mock{list1, block0, blockC2, listC1C2}, "test-1", list1, block0)
		use(t, mm, "list-t1")
		use(t, mm, "block-c0")
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactTest("test-2", []*models.Mock{blockC2, listC1C2})
		miss(t, mm, "block-c0", "block-c2")
	})
	t.Run("an earlier test's write the replay never made bounds nothing", func(t *testing.T) {
		mm := unmadeStage(t, []*models.Mock{list1, block0, blockC2, listC1C2}, "test-1", list1, block0)
		use(t, mm, "list-t1")
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactTest("test-2", []*models.Mock{blockC2, listC1C2})
		if got := answer(t, mm, viewCopy(t, mm, "list-c1c2"), "list"); answerName(got) != "list-t1" || got.Made != nil {
			t.Fatalf("answered by %q, made %v; want list-t1 and no made write", answerName(got), got.Made)
		}
	})
	t.Run("a write a test shares, made by the test beside", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		mm.SetMocksWithWindowThreeTier([]*models.Mock{list1, block0, blockC2, listC1C2}, nil, nil, models.BaseTime, time.Now())
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactTestConcurrent("test-1", []*models.Mock{list1}, []*models.Mock{block0})
		use(t, mm, "list-t1")
		use(t, mm, "block-c0")
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactTestConcurrent("test-2", []*models.Mock{blockC2, listC1C2}, []*models.Mock{block0})
		miss(t, mm, "block-c0", "block-c2")
	})
	t.Run("the test's own write made by an earlier attempt only is unmade", func(t *testing.T) {
		list2 := unmadeRecording("list-c1c2", models.Mongo, 1010, "read", "list")
		mm := unmadeStage(t, []*models.Mock{list1, blockC1, list2}, "test-2", blockC1, list2)
		use(t, mm, "block-c1")
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactTest("test-2", []*models.Mock{blockC1, list2})
		got := answer(t, mm, viewCopy(t, mm, "list-c1c2"), "list")
		if got.Write == nil || got.Write.Name != "block-c1" || answerName(got) != "list-t1" || got.Made != nil {
			t.Fatalf("got write %v answer %q made %v; want block-c1 unmade in this attempt and list-t1 to answer", got.Write, answerName(got), got.Made)
		}
	})
}

// The recordings the manager keeps on disk (an earlier test's, once its
// test ended) are read for the answer and the made writes alike: the whole
// staged set, whatever request time the disk keeps in nanoseconds.
func TestUnmadeWrite_TheRecordingsKeptOnDiskAreRead(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block0 := unmadeRecording("block-c0", models.Mongo, 500, "write", "block-c0")
	blockC2 := unmadeRecording("block-c2", models.Mongo, 2000, "write", "block-c2")
	listC2 := unmadeRecording("list-c2", models.Mongo, 2010, "read", "list")
	stage := func(t *testing.T, onDisk ...*models.Mock) *MockManager {
		t.Helper()
		disk, err := NewDiskMocks(zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = disk.Close() })
		for _, mk := range onDisk {
			if err := disk.Add(mk); err != nil {
				t.Fatal(err)
			}
		}
		disk.Finalize()
		mm := unmadeStage(t, []*models.Mock{blockC2, listC2}, "test-2", blockC2, listC2)
		mm.SetRecordedBefore(disk.RangeRecordedBefore)
		return mm
	}
	t.Run("the answer", func(t *testing.T) {
		mm := stage(t, list1)
		if got := answer(t, mm, viewCopy(t, mm, "list-c2"), "list"); answerName(got) != "list-t1" {
			t.Fatalf("answered by %q (note %q); want list-t1, kept on disk", answerName(got), got.Note())
		}
	})
	t.Run("a made write", func(t *testing.T) {
		mm := stage(t, list1, block0)
		if err := mm.flagMockAsUsed(exactUsedState(block0), 0); err != nil {
			t.Fatal(err)
		}
		got := answer(t, mm, viewCopy(t, mm, "list-c2"), "list")
		if got.Answer != nil || got.Made == nil || got.Made.Name != "block-c0" {
			t.Fatalf("answered by %q, made %v; want a miss after block-c0, kept on disk and made", answerName(got), got.Made)
		}
	})
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
// recording the open test does not own, made before its write, stamped as
// this view's, says no write is unmade.
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

// outcome is the test's mock accounting once the replay ended it (the pool
// view installed): which of its own recordings answered and which did not,
// and the names flagged as used (what the replay compares with the test's
// recorded mapping).
func outcome(t *testing.T, mm *MockManager) (consumed, unconsumed, flagged string) {
	t.Helper()
	mm.ServeExactPool(models.PoolKeyNone, nil)
	_, c, u, ok := mm.ExactTestOutcome()
	if !ok {
		t.Fatal("exact serving is off")
	}
	var f []string
	for _, st := range mm.GetConsumedMocks() {
		f = append(f, st.Name)
	}
	return strings.Join(c, ","), strings.Join(u, ","), strings.Join(f, ",")
}

// A read answered from before a write the replay has not made is the call
// of the recording chosen for it, made at another point: one read recorded
// is one read replayed. The recording still answers a call made after the
// write, once; when the test ends with none having taken it, it counts as
// the read's, so a scheduling difference (the read sent ahead of a write of
// the same collection) never fails the test's mock accounting.
func TestUnmadeWrite_AReadAnsweredFromBeforeTheWriteIsItsRecordingsCall(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block := unmadeRecording("block-t2", models.Mongo, 1000, "write", "block")
	list2 := unmadeRecording("list-t2", models.Mongo, 1010, "read", "list")
	staged := []*models.Mock{list1, block, list2}

	t.Run("the read sent ahead of the write: the test's accounting holds", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block, list2)
		if got := answerName(answer(t, mm, viewCopy(t, mm, "list-t2"), "list")); got != "list-t1" {
			t.Fatalf("answered by %q; want list-t1", got)
		}
		if !mm.MarkMockAsUsed(*viewCopy(t, mm, "block-t2")) {
			t.Fatal("the block was refused")
		}
		c, u, f := outcome(t, mm)
		if c != "block-t2,list-t2" || u != "" || f != "block-t2,list-t2" {
			t.Fatalf("consumed [%s] unconsumed [%s] flagged [%s]; want block-t2 and list-t2 consumed and flagged, nothing unconsumed: the read made ahead of the block was list-t2's call", c, u, f)
		}
	})
	t.Run("a read made after the write still takes its recording, once", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block, list2)
		answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
		if !mm.MarkMockAsUsed(*viewCopy(t, mm, "block-t2")) {
			t.Fatal("the block was refused")
		}
		after := viewCopy(t, mm, "list-t2")
		if !mm.MarkMockAsUsed(*after) {
			t.Fatal("the read after the block was refused its own recording: the earlier read consumed it")
		}
		if mm.MarkMockAsUsed(*after) {
			t.Fatal("list-t2 answered twice")
		}
		c, u, f := outcome(t, mm)
		if c != "block-t2,list-t2" || u != "" || f != "block-t2,list-t2" {
			t.Fatalf("consumed [%s] unconsumed [%s] flagged [%s]; want block-t2,list-t2 once each", c, u, f)
		}
	})
	t.Run("a second read before the write is still held", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block, list2)
		answer(t, mm, viewCopy(t, mm, "list-t2"), "list")
		if got := answer(t, mm, viewCopy(t, mm, "list-t2"), "list"); !got.Applies() || answerName(got) != "list-t1" {
			t.Fatalf("the second read before the block: applies %v answered by %q; want list-t1 again, never list-t2", got.Applies(), answerName(got))
		}
	})
	t.Run("a miss stands in for nothing", func(t *testing.T) {
		mm := unmadeStage(t, []*models.Mock{block, list2}, "test-2", block, list2)
		if got := answer(t, mm, viewCopy(t, mm, "list-t2"), "list"); got.Answer != nil {
			t.Fatalf("answered by %q; want a miss", answerName(got))
		}
		if !mm.MarkMockAsUsed(*viewCopy(t, mm, "block-t2")) {
			t.Fatal("the block was refused")
		}
		if _, u, f := outcome(t, mm); u != "list-t2" || f != "block-t2" {
			t.Fatalf("unconsumed [%s] flagged [%s]; want list-t2 unconsumed: the read was answered by nothing", u, f)
		}
	})
	t.Run("another test's recording is no call this test owes", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block)
		if got := answerName(answer(t, mm, list2.DeepCopy(), "list")); got != "list-t1" {
			t.Fatalf("answered by %q; want list-t1", got)
		}
		if c, u, f := outcome(t, mm); c != "" || u != "block-t2" || f != "" {
			t.Fatalf("consumed [%s] unconsumed [%s] flagged [%s]; want nothing consumed or flagged", c, u, f)
		}
	})
	t.Run("a recording the test shares is no call it owes", func(t *testing.T) {
		mm := unmadeStage(t, staged, "")
		mm.ServeExactTestConcurrent("test-2", []*models.Mock{block}, []*models.Mock{list2})
		if got := answerName(answer(t, mm, viewCopy(t, mm, "list-t2"), "list")); got != "list-t1" {
			t.Fatalf("answered by %q; want list-t1", got)
		}
		if c, _, f := outcome(t, mm); c != "" || f != "" {
			t.Fatalf("consumed [%s] flagged [%s]; want nothing: list-t2 is shared with the tests beside test-2, and a read of test-2 stands in for none of them", c, f)
		}
	})
	t.Run("a copy an earlier attempt read stands in for nothing", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block, list2)
		earlier := viewCopy(t, mm, "list-t2")
		mm.ServeExactTest("test-2", []*models.Mock{block, list2})
		mm.StandIn(earlier)
		if _, u, _ := outcome(t, mm); u != "block-t2,list-t2" {
			t.Fatalf("unconsumed [%s]; want block-t2,list-t2: the copy was the previous attempt's", u)
		}
	})
	t.Run("with no test open, nothing", func(t *testing.T) {
		// A test that ended between the rule and the stand-in: the pool
		// view in force holds no test's recordings.
		mm := unmadeStage(t, staged, "")
		mm.StandIn(list2.DeepCopy())
		if f := mm.GetConsumedMocks(); len(f) != 0 {
			t.Fatalf("flagged %v; want nothing", f)
		}
	})
	t.Run("a scoped view forwards it", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block, list2)
		var db integrations.MockMemDb = &scopedMockDb{MockMemDb: mm, live: true}
		if got := answerName(answer(t, db, viewCopy(t, db, "list-t2"), "list")); got != "list-t1" {
			t.Fatalf("answered by %q; want list-t1", got)
		}
		if _, u, _ := outcome(t, mm); u != "block-t2" {
			t.Fatalf("unconsumed [%s]; want block-t2 alone: list-t2 was the read's", u)
		}
	})
}

// What a parser keeps for a view alone (AtViewEnd) is forgotten once, when
// the view ends: the next view's install, or the manager's Close; never
// before, never twice, and never kept on a view that ended.
func TestUnmadeWrite_WhatAParserKeepsForAViewIsForgottenWhenItEnds(t *testing.T) {
	list1 := unmadeRecording("list-t1", models.Mongo, 0, "read", "list")
	block := unmadeRecording("block-t2", models.Mongo, 1000, "write", "block")
	staged := []*models.Mock{list1, block}
	counter := func(n *int) func() { return func() { *n++ } }
	t.Run("the next view's install ends the test's view", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block)
		var ran int
		if !mm.AtViewEnd(counter(&ran)) {
			t.Fatal("refused in an open test")
		}
		if ran != 0 {
			t.Fatalf("ran %d times while the test is open", ran)
		}
		mm.ServeExactPool(models.PoolKeyNone, nil)
		mm.ServeExactTest("test-3", nil)
		if ran != 1 {
			t.Fatalf("ran %d times; want once, as test-2's view ended", ran)
		}
	})
	t.Run("the manager's close ends the view in force", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block)
		var ran, late int
		mm.AtViewEnd(counter(&ran))
		mm.Close()
		if ran != 1 {
			t.Fatalf("ran %d times; want once, at Close", ran)
		}
		if mm.AtViewEnd(counter(&late)) {
			t.Fatal("kept on a view that ended; want refused, so the caller keeps nothing")
		}
		mm.Close()
		mm.ServeExactPool(models.PoolKeyNone, nil)
		if ran != 1 || late != 0 {
			t.Fatalf("ran %d and %d times after a second Close and an install; want 1 and 0", ran, late)
		}
	})
	t.Run("with exact serving off, nothing", func(t *testing.T) {
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		if mm.AtViewEnd(func() {}) {
			t.Fatal("kept with no view in force")
		}
	})
	t.Run("a scoped view forwards it", func(t *testing.T) {
		mm := unmadeStage(t, staged, "test-2", block)
		var ran int
		if !(&scopedMockDb{MockMemDb: mm, live: true}).AtViewEnd(counter(&ran)) {
			t.Fatal("the scoped view refused")
		}
		mm.ServeExactPool(models.PoolKeyNone, nil)
		if ran != 1 {
			t.Fatalf("ran %d times; want once", ran)
		}
		if (&scopedMockDb{MockMemDb: noViewEndDb{}}).AtViewEnd(func() {}) {
			t.Fatal("a scoped view over a store with no view end kept it")
		}
	})
}

// noViewEndDb is a store with no ViewEnd.
type noViewEndDb struct{ integrations.MockMemDb }
