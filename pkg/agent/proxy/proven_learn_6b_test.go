package proxy

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// The replay reaches Mock once per test set: from there, what the previous
// set served is no earlier place of the learn step's in the new set, whose
// mocks reuse its names (mock-0 again).
func TestALearnStepForgetsThePreviousSetAtMock(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := New(zap.NewNop(), fakeDestInfo{}, config.New())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.learnTracker.ServedMock(&models.Mock{Name: "mock-0", Spec: models.MockSpec{ReqTimestampMock: time.Now().Add(-time.Hour)}}) // set 1's start-up
	p.learnTracker.Begin()
	p.learnTracker.End()
	if err := p.Mock(ctx, models.OutgoingOptions{Mocking: true}); err != nil {
		t.Fatal(err)
	}
	p.learnTracker.Begin() // set 2, test 1
	if _, _, _, before := p.learnTracker.Test(); len(before) != 0 {
		t.Fatalf("set 2's first test has %d earlier places of set 1's", len(before))
	}
}

// The learn step's earlier places are the recording's: the manager stages
// every mock of the set, whatever the open view shows, and a mock exact
// serving served is still staged (it is spent, not taken out of its tree).
func TestStagedMocksAreTheWholeStagingWhateverTheViewShows(t *testing.T) {
	startUp := exactMock("startup", 0, models.LifetimeSession)
	own := exactMock("own", time.Second, models.LifetimePerTest)
	other := exactMock("other", 2*time.Second, models.LifetimePerTest)
	hs := configMock("handshake", 3*time.Second)
	mm := stagedManager(t, []*models.Mock{own, other}, []*models.Mock{startUp, hs})
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("T", []*models.Mock{own})
	if sess, _ := mm.GetSessionMocks(); len(exactNames(sess)) != 2 {
		t.Fatalf("precondition: the open view shows %v, not the pool's data-plane start-up", exactNames(sess))
	}
	if !mm.DeleteFilteredMock(*own) {
		t.Fatal("precondition: the test's own mock was not served")
	}
	got := map[string]bool{}
	for _, m := range mm.StagedMocks() {
		got[m.Name] = true
	}
	for _, name := range []string{"startup", "own", "other", "handshake"} {
		if !got[name] {
			t.Errorf("%s is not staged: %v", name, got)
		}
	}
	// A worker's view forwards them, whatever its allowlist shows.
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	sr, ok := view.(integrations.StagedReader)
	if !ok || len(sr.StagedMocks()) != len(mm.StagedMocks()) {
		t.Fatalf("a scoped view does not forward the staged mocks (implements %v)", ok)
	}
}
