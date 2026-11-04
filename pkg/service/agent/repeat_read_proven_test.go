package agent

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A test's repeated reads mark copies of its recordings, never the store's,
// and a set's replace the last set's.
func TestRepeatedReadsMarkCopiesOfTheTestsRecordings(t *testing.T) {
	m1, m2, m3 := &models.Mock{Name: "mock-1"}, &models.Mock{Name: "mock-2"}, &models.Mock{Name: "mock-3"}
	owned := []*models.Mock{m1, nil, m2, m3}
	got := markRepeatReads(owned, []models.RepeatRead{{Name: "onboarding", Mocks: []string{"mock-1", "mock-3"}}, {Name: "list", Mocks: []string{"mock-3", "mock-9"}}})
	if len(got) != 4 || got[1] != nil || got[2] != m2 {
		t.Fatalf("an unnamed recording is not served as it is: %v", got)
	}
	if got[0] == m1 || got[0].TestModeInfo.RepeatRead != "onboarding" || got[3] == m3 || got[3].TestModeInfo.RepeatRead != "list" {
		t.Fatalf("named recordings are not marked copies: %+v %+v", got[0].TestModeInfo, got[3].TestModeInfo)
	}
	if m1.TestModeInfo.RepeatRead != "" || m3.TestModeInfo.RepeatRead != "" || got[2].TestModeInfo.RepeatRead != "" {
		t.Fatal("the store's recordings were marked")
	}
	if same := markRepeatReads(owned, nil); &same[0] != &owned[0] {
		t.Fatal("a test with no repeated read is not served its recordings as they are")
	}

	a := &Agent{}
	in := map[string][]models.RepeatRead{"o::TestA": {{Name: "onboarding", Mocks: []string{"mock-1", "mock-3"}}}, "o::TestB": nil}
	if err := a.SetScopeRepeatReads(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	in["o::TestA"][0].Mocks[0] = "changed"
	if r := a.testRepeats["o::TestA"]; len(r) != 1 || r[0].Name != "onboarding" || r[0].Mocks[0] != "mock-1" {
		t.Fatalf("installed %v", a.testRepeats)
	}
	if _, ok := a.testRepeats["o::TestB"]; ok {
		t.Fatal("a test with no repeated read holds an entry")
	}
	if err := a.SetScopeRepeatReads(context.Background(), nil); err != nil || a.testRepeats != nil {
		t.Fatalf("a set with none keeps the last set's: %v", a.testRepeats)
	}
}

// ownedProxy keeps what each test was opened with.
type ownedProxy struct {
	orderProxy
	owned []*models.Mock
}

func (p *ownedProxy) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	p.owned = owned
	return p.orderProxy.OpenExactTest(test, owned, given)
}

// A keploy mock replay test opens with its repeated reads marked on its own
// recordings, and another test with none.
func TestAMockReplayTestOpensWithItsRepeatedReadsMarked(t *testing.T) {
	_, _ = orderAgent(t)
	p := &ownedProxy{}
	a := newReplayAgent()
	a.Proxy = p
	stored := []*models.Mock{{Name: "m1", Kind: models.HTTP}, {Name: "m2", Kind: models.HTTP}, {Name: "m3", Kind: models.HTTP}}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{filtered: stored})
	ctx := context.Background()
	_ = a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m1", "m2"}, "pkg::TestB": {"m3"}})
	_ = a.SetScopeRepeatReads(ctx, map[string][]models.RepeatRead{"pkg::TestA": {{Name: "onboarding", Mocks: []string{"m1", "m2"}}}})
	if err := a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(p.owned) != 2 || p.owned[0].TestModeInfo.RepeatRead != "onboarding" || p.owned[1].TestModeInfo.RepeatRead != "onboarding" {
		t.Fatalf("TestA opened with %v", p.owned)
	}
	if stored[0].TestModeInfo.RepeatRead != "" {
		t.Fatal("the store's recording was marked")
	}
	_ = a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now())
	if err := a.BeginScopeAt(ctx, "pkg::TestB", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(p.owned) != 1 || p.owned[0].TestModeInfo.RepeatRead != "" {
		t.Fatalf("TestB opened with %v", p.owned)
	}
}
