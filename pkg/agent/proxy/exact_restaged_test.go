package proxy

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// What a parser asks the manager to run once it lets its recordings go
// (integrations.Restaged) runs once, at the next staging, and at Close; at
// once when asked after Close; a later ask for a slot replaces the earlier;
// and through a worker's view, as through the manager. A store into the
// trees runs nothing: a parser's changed copy of another kind's recording
// (MySQL's matcher stores one on a match) leaves the HTTP indexes' keys as
// they were, and what they keep is of the set staged.
func TestWhatAParserAskedToRunRunsOnceWhenTheManagerLetsItsRecordingsGo(t *testing.T) {
	now := time.Now()
	var cfg *models.Mock
	stage := func(mm *MockManager) {
		cfg = newMockForTest("mysql-handshake", now.Add(-time.Second), models.LifetimeSession)
		cfg.Kind = models.MySQL
		cfg.Spec.Metadata = map[string]string{"type": "config"}
		mm.SetMocksWithWindow(nil, []*models.Mock{r3Mock("mock-0", now, "GET", "/a", "", "{}"), cfg}, models.BaseTime, now.Add(time.Hour))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	ran := map[string]int{}
	ask := func(db integrations.MockMemDb, slot, name string) {
		if !integrations.WhenRestaged(db, slot, func() { ran[name]++ }) {
			t.Fatalf("%T does not tell when it lets its recordings go", db)
		}
	}
	ask(mm, "a", "first")
	ask(mm, "a", "replaced")
	ask(mm, "b", "b")
	if len(ran) != 0 {
		t.Fatalf("ran before any staging: %v", ran)
	}
	stage(mm)
	if ran["first"] != 0 || ran["replaced"] != 1 || ran["b"] != 1 {
		t.Fatalf("after a staging ran %v, want replaced and b once", ran)
	}
	stage(mm)
	if ran["replaced"] != 1 || ran["b"] != 1 {
		t.Fatalf("a staging ran again what an earlier one ran: %v", ran)
	}
	ask(mm, "c", "kept")
	stored := mm.Revision()
	cp := cfg.ShallowCopy()
	cp.Spec.Metadata = map[string]string{"type": "config", "rev": "1"}
	if !mm.UpdateUnFilteredMock(cfg, cp) || mm.Revision() == stored {
		t.Fatal("the changed copy was not stored")
	}
	if ran["kept"] != 0 {
		t.Fatalf("a store into the trees ran what a parser asked %d times", ran["kept"])
	}
	ask(&scopedMockDb{MockMemDb: mm}, "a", "view")
	stage(mm)
	if ran["view"] != 1 || ran["kept"] != 1 {
		t.Fatalf("asked through a worker's view, a staging ran it %d times; asked before a store, %d", ran["view"], ran["kept"])
	}
	ask(mm, "a", "closing")
	mm.Close()
	if ran["closing"] != 1 {
		t.Fatalf("Close ran it %d times", ran["closing"])
	}
	ask(mm, "a", "closed")
	if ran["closed"] != 1 {
		t.Fatalf("asked after Close, it ran %d times", ran["closed"])
	}
}

// A parser's release runs only once the staging moved every generation, so a
// parser that samples a generation from its release, or builds again and asks
// again there, files what it builds under the set staged, never under the
// generation of the set the staging replaced (that index would then be kept,
// and served from, until the next move).
func TestAParsersReleaseRunsOnceTheStagingMovedTheGenerations(t *testing.T) {
	now := time.Now()
	mm := NewMockManager(nil, nil, zap.NewNop())
	stage := func() {
		mm.SetMocksWithWindow(nil, []*models.Mock{r3Mock("mock-0", now, "GET", "/a", "", "{}")}, models.BaseTime, now.Add(time.Hour))
	}
	stage()
	type gens struct{ all, http, rev uint64 }
	sample := func() gens {
		return gens{mm.StagingGeneration(), mm.StagingGenerationByKind(models.HTTP), mm.Revision()}
	}
	before := sample()
	var atRelease gens
	ran := 0
	if !integrations.WhenRestaged(mm, "a", func() { ran++; atRelease = sample() }) {
		t.Fatal("the manager does not tell when it lets its recordings go")
	}
	stage()
	after := sample()
	if ran != 1 {
		t.Fatalf("the staging ran the release %d times, want once", ran)
	}
	if atRelease.all == before.all || atRelease.http == before.http || atRelease.rev == before.rev {
		t.Fatalf("the release ran before the staging moved the generations: at the release %+v, before the staging %+v, after it %+v", atRelease, before, after)
	}
	if atRelease != after {
		t.Fatalf("the release saw %+v, not the staging's final generations %+v", atRelease, after)
	}
}

// Every store and view a parser can be handed that tells when it lets its
// recordings go (integrations.Restaged) tells whether it closed
// (integrations.Closer), so the parser tells a release run by the Close from
// one run by a staging by asking, never by when it ran: the manager, a
// worker's view of it, and the store a call is held to.
func TestAReleaseIsToldWhetherItIsTheStoresClose(t *testing.T) {
	now := time.Now()
	stage := func(mm *MockManager) {
		mm.SetMocksWithWindow(nil, []*models.Mock{r3Mock("mock-0", now, "GET", "/a", "", "{}")}, models.BaseTime, now.Add(time.Hour))
	}
	mm := NewMockManager(nil, nil, zap.NewNop())
	stage(mm)
	held, _ := mm.HoldToRound(mm.MarkCallRound())
	if _, ok := held.(*heldScope); !ok {
		t.Fatalf("precondition: the call is not held to its staging (%T)", held)
	}
	views := map[string]integrations.MockMemDb{
		"the manager":                   mm,
		"a worker's view":               &scopedMockDb{MockMemDb: mm},
		"a held call's store":           held,
		"a view of a held call's store": &scopedMockDb{MockMemDb: held},
	}
	saw := map[string][]bool{}
	ask := func() {
		for name, db := range views {
			if _, ok := db.(integrations.Closer); !ok {
				t.Fatalf("%s (%T) tells its Close but not whether it closed", name, db)
			}
			integrations.WhenRestaged(db, name, func() { saw[name] = append(saw[name], integrations.IsClosed(db)) })
		}
	}
	ask()
	stage(mm)
	ask()
	mm.Close()
	ask()
	for name := range views {
		if got := saw[name]; len(got) != 3 || got[0] || !got[1] || !got[2] {
			t.Errorf("%s: closed as told at a staging, at Close, after Close: %v, want [false true true]", name, got)
		}
	}
	if integrations.IsClosed(struct{ integrations.MockMemDb }{mm}) {
		t.Error("a store that does not tell whether it closed is taken as closed")
	}
}
