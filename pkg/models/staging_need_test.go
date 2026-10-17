package models

import "testing"

// A StagingWithStartup need marks the per-test mocks of its kind that its
// Match accepts, and no other: not another kind's, not a mock that is not
// per-test, not a carry-over mock, and nothing once it is removed.
func TestAStagingWithStartupNeedMarksOnlyThePerTestMocksItDeclares(t *testing.T) {
	const kind Kind = "StagingNeedTestKind"
	mock := func(kind Kind, cmd string, life Lifetime) *Mock {
		return &Mock{Kind: kind, Spec: MockSpec{Metadata: map[string]string{"commandType": cmd}}, TestModeInfo: TestModeInfo{Lifetime: life}}
	}
	push := mock(kind, "MESSAGE", LifetimePerTest)
	if IsStagedAtStaging(push) || StagedAtStagingRegistered() {
		t.Fatal("a mock no need declares is staged")
	}
	remove := RegisterStagingNeed(kind, StagingNeed{Use: StagingWithStartup, Match: func(m *Mock) bool {
		return m.Spec.Metadata["commandType"] == "MESSAGE" || m.Spec.Metadata["commandType"] == "SEND"
	}})
	// Removed however the test ends, so a failure here leaves no need
	// registered for the next test; removing it twice is harmless.
	t.Cleanup(remove)
	if !IsStagedAtStaging(push) || !StagedAtStagingRegistered() {
		t.Fatal("the declared per-test mock is not staged")
	}
	defer RegisterCarryOver(kind, func(m *Mock) bool { return m.Spec.Metadata["commandType"] == "SEND" })()
	for name, m := range map[string]*Mock{
		"a carry-over mock": mock(kind, "SEND", LifetimePerTest),
		"another command":   mock(kind, "ACK", LifetimePerTest),
		"another kind":      mock("OtherKind", "MESSAGE", LifetimePerTest),
		"a session mock":    mock(kind, "MESSAGE", LifetimeSession),
		"no mock at all":    nil,
	} {
		if IsStagedAtStaging(m) {
			t.Fatalf("%s is staged", name)
		}
	}
	remove()
	if IsStagedAtStaging(push) || StagedAtStagingRegistered() {
		t.Fatal("a removed need still declares its mocks")
	}
}

// A need its use cannot take is a programming error: it panics at
// registration rather than declaring nothing.
func TestAStagingNeedItsUseCannotTakePanics(t *testing.T) {
	for name, need := range map[string]StagingNeed{
		"no use":                       {Match: func(*Mock) bool { return true }},
		"an unknown use":               {Use: 99, Match: func(*Mock) bool { return true }},
		"StagingWithStartup, no Match": {Use: StagingWithStartup},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: registered without a panic", name)
				}
			}()
			RegisterStagingNeed("StagingNeedPanicKind", need)
		}()
	}
	if StagedAtStagingRegistered() {
		t.Fatal("a refused need was registered")
	}
}
