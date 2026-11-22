package agent

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A set's repeated reads replace the last set's, held as copies of what
// the replay sent.
func TestASetsRepeatedReadsReplaceTheLastSets(t *testing.T) {
	a := &Agent{}
	in := map[string][]models.RepeatRead{"o::TestA": {{Name: "onboarding", Mocks: []string{"mock-1", "mock-3"}}}, "o::TestB": nil, "o::TestC": {{Name: "list", Mocks: []string{"mock-5", "mock-6"}}}}
	if err := a.SetScopeRepeatReads(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	in["o::TestA"][0].Mocks[0] = "changed"
	if r := a.testRepeats["o::TestA"]; len(r) != 1 || r[0].Name != "onboarding" || r[0].Mocks[0] != "mock-1" {
		t.Fatalf("installed %v", a.testRepeats)
	}
	if r := a.testRepeats["o::TestC"]; len(r) != 1 || r[0].Name != "list" {
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

// A keploy mock replay test opens with its repeated reads marked on copies
// of its own recordings, each read one mark its recordings share, two reads
// named alike two marks; another test opens with none.
func TestAMockReplayTestOpensWithItsRepeatedReadsMarked(t *testing.T) {
	_, _ = orderAgent(t)
	p := &ownedProxy{}
	a := newReplayAgent()
	a.Proxy = p
	stored := []*models.Mock{{Name: "m1", Kind: models.HTTP}, {Name: "m2", Kind: models.HTTP}, {Name: "m3", Kind: models.HTTP}, {Name: "m4", Kind: models.HTTP}, {Name: "m5", Kind: models.HTTP}}
	a.clientMocks.Store(uint64(0), &ClientMockStorage{filtered: stored})
	ctx := context.Background()
	_ = a.SetScopeTable(ctx, map[string][]string{"pkg::TestA": {"m1", "m2", "m3", "m4"}, "pkg::TestB": {"m5"}})
	_ = a.SetScopeRepeatReads(ctx, map[string][]models.RepeatRead{"pkg::TestA": {{Name: "list", Mocks: []string{"m1", "m2"}}, {Name: "list", Mocks: []string{"m3", "m4"}}}})
	if err := a.BeginScopeAt(ctx, "pkg::TestA", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(p.owned) != 4 {
		t.Fatalf("TestA opened with %v", p.owned)
	}
	for i, mk := range p.owned {
		if r := mk.TestModeInfo.RepeatRead; !r.Marked() || r.Name != "list" {
			t.Fatalf("TestA's recording %d opened with read %+v; want one named list", i, r)
		}
	}
	if r := p.owned; r[0].TestModeInfo.ReadMark() != r[1].TestModeInfo.ReadMark() || r[2].TestModeInfo.ReadMark() != r[3].TestModeInfo.ReadMark() {
		t.Fatal("the recordings of one read opened as two reads")
	}
	if p.owned[0].TestModeInfo.ReadMark() == p.owned[2].TestModeInfo.ReadMark() {
		t.Fatal("two reads named alike opened as one read")
	}
	if stored[0].TestModeInfo.RepeatRead.Marked() {
		t.Fatal("the store's recording was marked")
	}
	_ = a.EndScopeAt(ctx, "pkg::TestA", 0, time.Now())
	if err := a.BeginScopeAt(ctx, "pkg::TestB", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(p.owned) != 1 || p.owned[0].TestModeInfo.RepeatRead.Marked() {
		t.Fatalf("TestB opened with %v", p.owned)
	}
}
