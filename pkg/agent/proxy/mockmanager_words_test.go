package proxy

import (
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Word-level learn's persistence through the mock manager (design r9, plan
// K5): the learn step leaves the words it learned of a consumed mock's
// leaves on the copy it hands a consume door (MockSpec.LearnedWords), with
// record's full round's values there (MockSpec.LearnedF). The door's state
// is all that reaches the mock file and learned.local.yaml, so a door that
// dropped them would leave every word-learned leaf of its mocks compared
// whole at replay: a loud false red.

// wordsEntry is a readable words entry (its guards and slots are never read
// here).
func wordsEntry(digest string, slots ...models.WordSlot) models.WordsEntry {
	return models.WordsEntry{V: models.WordsVersion, Digest: digest, Layout: "4be1c09a5d3e7f21", Slots: slots}
}

// withLearned is a copy of m as the learn step hands it to a consume door:
// words and F's values on its spec, the pool's mock unchanged.
func withLearned(m *models.Mock, words map[string]models.WordsEntry, f map[string]map[string]string) models.Mock {
	c := *m
	c.Spec.LearnedWords = words
	c.Spec.SetLearnedF(f)
	return c
}

// windowManager is a mock manager whose open test's window holds now, with
// perTest and session staged.
func windowManager(t *testing.T, now time.Time, perTest, session []*models.Mock) *MockManager {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(perTest, session, now.Add(-time.Hour), now.Add(time.Hour))
	return mm
}

// only is the one mock of ms named name.
func only(t *testing.T, what string, ms []*models.Mock, name string) *models.Mock {
	t.Helper()
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("precondition: %s holds no %s: %v", what, name, exactNames(ms))
	return nil
}

// Every consume door's state carries the words and F's values the learn
// step left on the copy it consumed, under the proven switch: a test's own
// mock (DeleteFilteredMock, the door consumeWith takes for a test's call),
// a carry-over mock (DeleteFilteredMock's carry-over tier), a session mock
// (DeleteUnFilteredMock), a reusable one (MarkMockAsUsed, UpdateUnFilteredMock)
// and a start-up one (DeleteStartupMock). With the switch off nothing learns
// words, and no door's state carries any.
func TestEachConsumeDoorCarriesTheLearnedWords(t *testing.T) {
	words := map[string]models.WordsEntry{
		"body.note":    wordsEntry("9f2c41d07a3be815", models.WordSlot{Seg: 4, Run: 0}),
		"header.x-ref": wordsEntry("1111111111111111", models.WordSlot{Seg: 0, Kind: models.WordSlotToken}),
	}
	f := map[string]map[string]string{"w": {"body.note": "ORD-1 paid ref=d4e5f6", "header.x-ref": "r-2"}}
	for _, door := range []struct {
		name string
		// consume stages w where the door reads it and consumes the
		// learn step's copy of it there.
		consume   func(t *testing.T) (*MockManager, bool)
		carryOver bool
	}{
		{"DeleteFilteredMock, a test's own mock", func(t *testing.T) (*MockManager, bool) {
			now := time.Now()
			mm := windowManager(t, now, []*models.Mock{newMockForTest("w", now, models.LifetimePerTest)}, nil)
			pt, _ := mm.GetPerTestMocksInWindow()
			return mm, mm.DeleteFilteredMock(withLearned(only(t, "the per-test tier", pt, "w"), words, f))
		}, false},
		{"DeleteFilteredMock, a carry-over mock", func(t *testing.T) (*MockManager, bool) {
			registerSends(t)
			r := newCarryRig(t, brokerMockAt("s1", pulsarKind, "SEND", 2050), brokerMockAt("w", pulsarKind, "SEND", 2500))
			r.window(2)
			co, _ := r.mm.GetCarryOverMocks()
			return r.mm, r.mm.DeleteFilteredMock(withLearned(only(t, "the carry-over tier", co, "w"), words, f))
		}, true},
		{"DeleteUnFilteredMock", func(t *testing.T) (*MockManager, bool) {
			now := time.Now()
			mm := windowManager(t, now, nil, []*models.Mock{newMockForTest("w", now, models.LifetimeSession)})
			un, _ := mm.GetUnFilteredMocks()
			return mm, mm.DeleteUnFilteredMock(withLearned(only(t, "the session tier", un, "w"), words, f))
		}, false},
		{"MarkMockAsUsed", func(t *testing.T) (*MockManager, bool) {
			now := time.Now()
			mm := windowManager(t, now, nil, []*models.Mock{newMockForTest("w", now, models.LifetimeSession)})
			un, _ := mm.GetUnFilteredMocks()
			return mm, mm.MarkMockAsUsed(withLearned(only(t, "the session tier", un, "w"), words, f))
		}, false},
		{"UpdateUnFilteredMock", func(t *testing.T) (*MockManager, bool) {
			now := time.Now()
			mm := windowManager(t, now, nil, []*models.Mock{newMockForTest("w", now, models.LifetimeSession)})
			un, _ := mm.GetUnFilteredMocks()
			old := only(t, "the session tier", un, "w")
			c := withLearned(old, words, f)
			return mm, mm.UpdateUnFilteredMock(old, &c)
		}, false},
		{"DeleteStartupMock", func(t *testing.T) (*MockManager, bool) {
			// A staging at BaseTime files a test's mocks in the start-up
			// tier (TestAPerTestReadFindsTheOpenTestsMocks).
			mm := stagedManager(t, []*models.Mock{exactMock("w", 0, models.LifetimePerTest)}, nil)
			st, _ := mm.GetStartupMocks()
			return mm, mm.DeleteStartupMock(withLearned(only(t, "the start-up tier", st, "w"), words, f))
		}, false},
	} {
		t.Run(door.name, func(t *testing.T) {
			for _, on := range []bool{true, false} {
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
				}
				mm, ok := door.consume(t)
				if !ok {
					t.Fatalf("switch on %v: the door did not consume the mock", on)
				}
				st, found := r3States(mm)["w"]
				if !found {
					t.Fatalf("switch on %v: the door left no state", on)
				}
				if st.CarryOver != door.carryOver {
					t.Fatalf("switch on %v: CarryOver %v, want %v", on, st.CarryOver, door.carryOver)
				}
				if !on {
					if st.LearnedWords != nil || st.LearnedF != nil {
						t.Fatalf("with the switch off the state carries words %v and F %v", st.LearnedWords, st.LearnedF)
					}
					continue
				}
				if !reflect.DeepEqual(st.LearnedWords, words) {
					t.Fatalf("the state's words %+v, want what the learn step learned %+v", st.LearnedWords, words)
				}
				if !reflect.DeepEqual(st.LearnedF, f) {
					t.Fatalf("the state's F %v, want %v", st.LearnedF, f)
				}
			}
		})
	}
}

// A mock the round answers twice through a door that keeps it (a reusable
// recording, MarkMockAsUsed) keeps what both pairings learned: both
// pairings' words, and at a place both saw, the lesser of F's values, so
// learned.local.yaml is the same whichever call the app made first.
func TestAMockAnsweredTwiceKeepsWhatBothPairingsLearned(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	note := wordsEntry("9f2c41d07a3be815", models.WordSlot{Seg: 4, Run: 0})
	ref := wordsEntry("1111111111111111", models.WordSlot{Seg: 0, Kind: models.WordSlotToken})
	type pairing struct {
		words map[string]models.WordsEntry
		f     map[string]map[string]string
	}
	a := pairing{map[string]models.WordsEntry{"body.note": note}, map[string]map[string]string{"w": {"body.note": "ORD-1 ref=e5"}}}
	b := pairing{map[string]models.WordsEntry{"header.x-ref": ref}, map[string]map[string]string{"w": {"body.note": "ORD-1 ref=d4", "header.x-ref": "r-2"}}}
	wantWords := map[string]models.WordsEntry{"body.note": note, "header.x-ref": ref}
	wantF := map[string]map[string]string{"w": {"body.note": "ORD-1 ref=d4", "header.x-ref": "r-2"}}
	for _, order := range [][2]pairing{{a, b}, {b, a}} {
		now := time.Now()
		mm := windowManager(t, now, nil, []*models.Mock{newMockForTest("w", now, models.LifetimeSession)})
		un, _ := mm.GetUnFilteredMocks()
		w := only(t, "the session tier", un, "w")
		for i, p := range order {
			if !mm.MarkMockAsUsed(withLearned(w, p.words, p.f)) {
				t.Fatalf("pairing %d was not consumed", i+1)
			}
		}
		st := r3States(mm)["w"]
		if !reflect.DeepEqual(st.LearnedWords, wantWords) {
			t.Errorf("words %+v, want both pairings' %+v", st.LearnedWords, wantWords)
		}
		if !reflect.DeepEqual(st.LearnedF, wantF) {
			t.Errorf("F %v, want %v whichever pairing came first", st.LearnedF, wantF)
		}
	}
}
