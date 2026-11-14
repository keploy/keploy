//go:build linux

package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// givingProxy is a wire proxy that keeps the test variables a replay binds:
// it records each given it was handed for the test it serves.
type givingProxy struct {
	*wireProxy
	gave    []models.VariableBinding
	giveErr error
	// between are the bindings and ends (Name "end") of the givens in
	// force for a worker's calls outside its tests, in order
	// (GiveBetweenTests, EndBetweenTests); betweenErr fails both.
	between    []models.VariableBinding
	betweenErr error
}

func (p *givingProxy) GiveExactTest(test string, b models.VariableBinding) error {
	p.mu.Lock()
	serving := p.exactTest
	p.mu.Unlock()
	if test != serving {
		return fmt.Errorf("%s is not the test keploy serves now", test)
	}
	if p.giveErr != nil {
		return p.giveErr
	}
	p.gave = append(p.gave, b)
	return nil
}

func giving(a *Agent, p *wireProxy) *givingProxy {
	g := &givingProxy{wireProxy: p}
	a.Proxy = g
	return g
}

func give(name, value, source string) models.GivenReq {
	return models.GivenReq{Name: name, Value: value, Source: source}
}

// In a record a test's givens are numbered per name in the order it gave
// them, kept with its window, and bound nowhere; a given outside a test, or
// one keploy cannot hold, is refused.
func TestARecordedTestsGivensAreKeptWithItsWindow(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	g := giving(a, p)
	ctx := context.Background()

	// No process holds the run: no test would carry the value.
	_, serr := a.ProvenGive(ctx, give("app", "app-1", "a_test.go:9"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	require.Contains(t, serr.Message, "app (a_test.go:9) while no test was open and no test process held the run")
	require.Contains(t, serr.Remedy, "in a test file's beforeAll, after the run's claim")
	claimSelf(t, a)

	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	var vars []string
	for _, r := range []models.GivenReq{give("app", "app-1", "a_test.go:20"), give("nonce", "1761609600123456789", "a_test.go:21"), give("app", "app-2", "a_test.go:22")} {
		resp, serr := a.ProvenGive(ctx, r)
		require.Nil(t, serr)
		require.Equal(t, "ok", resp.Status)
		require.Equal(t, models.ScopeProtocolVersion, resp.V)
		vars = append(vars, resp.Variable)
	}
	require.Equal(t, []string{"given:app", "given:nonce", "given:app#2"}, vars)
	for _, bad := range []models.GivenReq{give("app.name", "v", ""), give("", "v", ""), give("app", "", "")} {
		_, serr := a.ProvenGive(ctx, bad)
		require.NotNil(t, serr, "%+v", bad)
		require.Equal(t, models.ScopeErrBadRequest, serr.Code, "%+v", bad)
	}
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, g.gave, "a record binds no variable")

	// The next test numbers its own from the first: a test's givens end
	// with it.
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	resp, serr := a.ProvenGive(ctx, give("app", "app-4", ""))
	require.Nil(t, serr)
	require.Equal(t, "given:app", resp.Variable)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)

	require.Equal(t, []models.GivenValue{
		{Name: "given:app", Value: "app-1", Source: "a_test.go:20"},
		{Name: "given:nonce", Value: "1761609600123456789", Source: "a_test.go:21"},
		{Name: "given:app#2", Value: "app-2", Source: "a_test.go:22"},
	}, windowOf(t, a, "o::TestA").Given)
	require.Equal(t, []models.GivenValue{{Name: "given:app", Value: "app-4"}}, windowOf(t, a, "o::TestB").Given)
}

// In a replay each given binds in the open test's scope with the value its
// recording gave under the same variable; one the recording did not give
// binds with none. One the proxy refuses is refused, and not kept.
func TestAReplayedGivenBindsWithItsRecordedValue(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	g := giving(a, p)
	ctx := context.Background()
	require.NoError(t, a.SetScopeGiven(ctx, map[string][]models.GivenValue{
		"orders::TestA": {{Name: "given:app", Value: "app-1", Source: "a_test.go:20"}},
		"orders::TestB": {},
	}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)

	resp, serr := a.ProvenGive(ctx, give("app", "app-9", "a_test.go:20"))
	require.Nil(t, serr)
	require.Equal(t, "given:app", resp.Variable)
	resp, serr = a.ProvenGive(ctx, give("app", "app-10", "a_test.go:30"))
	require.Nil(t, serr)
	require.Equal(t, "given:app#2", resp.Variable)
	require.Equal(t, []models.VariableBinding{
		{Name: "given:app", Value: "app-9", Recorded: "app-1", Mock: "test orders::TestA", Place: "a_test.go:20", Given: true},
		{Name: "given:app#2", Value: "app-10", Mock: "test orders::TestA", Place: "a_test.go:30", Given: true},
	}, g.gave)

	g.giveErr = errors.New("variable given:other is bound already")
	_, serr = a.ProvenGive(ctx, give("other", "x", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrInternal, serr.Code)
	require.Contains(t, serr.Message, "could not hold given:other for orders::TestA")
	g.giveErr = nil
	resp, serr = a.ProvenGive(ctx, give("other", "y", ""))
	require.Nil(t, serr)
	require.Equal(t, "given:other", resp.Variable, "a refused given is not counted")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, windowOf(t, a, "orders::TestA").Given, "a replay holds a test's givens only while it is open")
}

// A value the test process gives while none of its tests is open (a test
// file's beforeAll) is given to each test it begins from then on, first,
// under the same variable: a record keeps it in each such test's window,
// and a test's own givens are numbered after it. A test that began before
// it does not carry it.
func TestARecordGivesAWorkerValueToEachLaterTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_RECORD)
	g := giving(a, p)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestEarly"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestEarly", Outcome: "passed"})
	require.Nil(t, serr)

	resp, serr := a.ProvenGive(ctx, give("uniq", "1761609600123-ab12", "s.spec.ts:24"))
	require.Nil(t, serr)
	require.Equal(t, "ok", resp.Status)
	require.Equal(t, "given:uniq", resp.Variable)
	for _, name := range []string{"TestA", "TestB"} {
		_, serr = a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		resp, serr = a.ProvenGive(ctx, give("uniq", "own-"+name, "s.spec.ts:40"))
		require.Nil(t, serr)
		require.Equal(t, "given:uniq#2", resp.Variable, "a test's own givens come after its worker's")
		_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
		require.Nil(t, serr)
	}
	// A second setup's value joins the first for the tests after it.
	resp, serr = a.ProvenGive(ctx, give("uniq", "1761609600999-cd34", "s.spec.ts:60"))
	require.Nil(t, serr)
	require.Equal(t, "given:uniq#2", resp.Variable)
	_, serr = a.ProvenBegin(ctx, begin("TestC"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestC", Outcome: "passed"})
	require.Nil(t, serr)
	require.Empty(t, g.gave, "a record binds no variable")

	require.Empty(t, windowOf(t, a, "o::TestEarly").Given, "a test begun before the value does not carry it")
	for _, name := range []string{"TestA", "TestB"} {
		require.Equal(t, []models.GivenValue{
			{Name: "given:uniq", Value: "1761609600123-ab12", Source: "s.spec.ts:24"},
			{Name: "given:uniq#2", Value: "own-" + name, Source: "s.spec.ts:40"},
		}, windowOf(t, a, "o::"+name).Given)
	}
	require.Equal(t, []models.GivenValue{
		{Name: "given:uniq", Value: "1761609600123-ab12", Source: "s.spec.ts:24"},
		{Name: "given:uniq#2", Value: "1761609600999-cd34", Source: "s.spec.ts:60"},
	}, windowOf(t, a, "o::TestC").Given)
}

// givingOpener is a giving proxy that opens a test's view with its variables
// begun, as keploy's own proxy does (coreAgent.ExactTestOpener): it records
// the bindings each test opened with.
type givingOpener struct {
	*givingProxy
	opened map[string][]models.VariableBinding
}

func (p *givingOpener) OpenExactTest(test string, owned []*models.Mock, given []models.VariableBinding) error {
	if err := p.ServeExactTest(test, owned); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.opened == nil {
		p.opened = map[string][]models.VariableBinding{}
	}
	p.opened[test] = append(p.opened[test], given...)
	return nil
}

// In a replay a value the test process gives outside its tests binds in
// each test it begins from then on, as the test's view opens, with the
// value that test's recording gave under the same variable: a setup-minted
// name used by two tests is followed in both. A test whose recording lacks
// it binds it with none, and its end names it.
func TestAReplayBindsAWorkerValueInEachLaterTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	o := &givingOpener{givingProxy: &givingProxy{wireProxy: p}}
	a.Proxy = o
	ctx := context.Background()
	require.NoError(t, a.SetScopeGiven(ctx, map[string][]models.GivenValue{
		"o::TestA": {{Name: "given:uniq", Value: "rec-1", Source: "s.spec.ts:24"}},
		"o::TestB": {{Name: "given:uniq", Value: "rec-1", Source: "s.spec.ts:24"}, {Name: "given:uniq#2", Value: "rec-own"}},
	}))
	claimSelf(t, a)
	resp, serr := a.ProvenGive(ctx, give("uniq", "new-9", "s.spec.ts:24"))
	require.Nil(t, serr)
	require.Equal(t, "given:uniq", resp.Variable)
	require.Empty(t, o.gave, "no test is open to bind it in")

	var last models.ScopeEndResp
	for _, name := range []string{"TestA", "TestB", "TestC"} {
		_, serr = a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		if name == "TestB" {
			resp, serr = a.ProvenGive(ctx, give("uniq", "new-own", ""))
			require.Nil(t, serr)
			require.Equal(t, "given:uniq#2", resp.Variable)
		}
		if name == "TestC" {
			p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /query", Destination: "127.0.0.1:8083"}}
		}
		last, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
		require.Nil(t, serr)
	}
	require.Equal(t, map[string][]models.VariableBinding{
		"o::TestA": {{Name: "given:uniq", Value: "new-9", Recorded: "rec-1", Mock: "test o::TestA", Place: "s.spec.ts:24", Given: true}},
		"o::TestB": {{Name: "given:uniq", Value: "new-9", Recorded: "rec-1", Mock: "test o::TestB", Place: "s.spec.ts:24", Given: true}},
		"o::TestC": {{Name: "given:uniq", Value: "new-9", Mock: "test o::TestC", Place: "s.spec.ts:24", Given: true}},
	}, o.opened)
	require.Equal(t, []models.VariableBinding{
		{Name: "given:uniq#2", Value: "new-own", Recorded: "rec-own", Mock: "test o::TestB", Given: true},
	}, o.gave, "a test's own given is bound after its worker's, numbered after it")
	require.Equal(t, models.ScopeActionFail, last.Action)
	require.True(t, strings.HasPrefix(last.Reason, "keploy: the test gave given:uniq, which its recording does not hold (recorded before the test gave it): re-record the test. POST /query"), last.Reason)
}

// What a test process gave outside its tests is its own: another process's
// claim of the run starts with none, and a new session forgets them.
func TestAWorkerValueEndsWithItsClaim(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	gone := exec.Command("sleep", "60")
	require.NoError(t, gone.Start())
	t.Cleanup(func() { _ = gone.Process.Kill(); _ = gone.Wait() })
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, gone.Process.Pid)
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("uniq", "u-1", ""))
	require.Nil(t, serr)
	require.NoError(t, gone.Process.Kill())
	_ = gone.Wait()

	claimSelf(t, a)
	resp, serr := a.ProvenGive(ctx, give("uniq", "u-2", ""))
	require.Nil(t, serr)
	require.Equal(t, "given:uniq", resp.Variable, "the gone process's value is not counted")
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, []models.GivenValue{{Name: "given:uniq", Value: "u-2"}}, windowOf(t, a, "o::TestA").Given)

	a.resetScopeState()
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	require.Empty(t, a.workerGiven, "a new session starts with none")
}

// What a test worker of a run of several gave outside its tests ends with
// its instance, when its process exits or its claim is taken back: no later
// claim is given that instance's value, so nothing else would drop them.
func TestAWorkersOutsideValuesEndWithItsInstance(t *testing.T) {
	a, h := workerAgent(t, models.MODE_RECORD, 4)
	ctx := context.Background()
	gone, stays := child(t), child(t)
	claimWorker(t, a, gone.Process.Pid, "a", 2)
	claimWorker(t, a, stays.Process.Pid, "b", 2)
	_, serr := a.ProvenGive(as(gone.Process.Pid), give("uniq", "u-gone", ""))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(as(stays.Process.Pid), give("uniq", "u-stays", ""))
	require.Nil(t, serr)
	held := func() map[uint32][]models.GivenValue {
		a.scopeMu.Lock()
		defer a.scopeMu.Unlock()
		return maps.Clone(a.workerGiven)
	}
	require.Len(t, held(), 2)

	kill(t, gone)
	require.NoError(t, a.wire.lock(ctx))
	a.endGoneWorkers(ctx)
	a.wire.unlock()
	require.Equal(t, []uint32{1}, h.endedIDs())
	require.Equal(t, map[uint32][]models.GivenValue{2: {{Name: "given:uniq", Value: "u-stays"}}}, held(),
		"the gone worker's value went with its instance; the live one's stays")

	a.dropWorker(2)
	require.Empty(t, held(), "a claim taken back takes its worker's values with it")
}

// A test file's afterAll ends what its setup gave outside the tests (an
// end, models.GivenReq.End): no test the process begins after it carries
// those values, and the next file's setup numbers its own from the first,
// as it would in a worker of its own. An end names no value, comes only
// between tests, and only from a process that holds the run.
func TestAnEndOfAFilesValuesKeepsThemFromTheNextFile(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	end := models.GivenReq{End: true}

	_, serr := a.ProvenGive(ctx, end)
	require.NotNil(t, serr, "no process holds the run")
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)
	require.Equal(t, http.StatusUnprocessableEntity, serr.Status)
	require.Contains(t, serr.Message, "while no test process held the run")
	claimSelf(t, a)

	for _, bad := range []models.GivenReq{{End: true, Name: "uniq"}, {End: true, Value: "a-1"}, {End: true, Source: "a.spec.ts:9"}} {
		_, serr := a.ProvenGive(ctx, bad)
		require.NotNil(t, serr, "%+v", bad)
		require.Equal(t, models.ScopeErrBadRequest, serr.Code, "%+v", bad)
		require.Contains(t, serr.Message, "carries no name, value or source", "%+v", bad)
	}

	// File a: its setup's value, its tests, its end.
	resp, serr := a.ProvenGive(ctx, give("uniq", "a-1", "a.spec.ts:9"))
	require.Nil(t, serr)
	require.Equal(t, "given:uniq", resp.Variable)
	_, serr = a.ProvenBegin(ctx, begin("TestA1"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, end)
	require.NotNil(t, serr, "an end while a test is open")
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, "were ended while o::TestA1 was open")
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA1", Outcome: "passed"})
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestA2"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA2", Outcome: "passed"})
	require.Nil(t, serr)
	resp, serr = a.ProvenGive(ctx, end)
	require.Nil(t, serr)
	require.Equal(t, models.GivenResp{V: models.ScopeProtocolVersion, Status: "ok"}, resp)

	// File b gives the same name: numbered from the first again.
	resp, serr = a.ProvenGive(ctx, give("uniq", "b-1", "b.spec.ts:9"))
	require.Nil(t, serr)
	require.Equal(t, "given:uniq", resp.Variable)
	_, serr = a.ProvenBegin(ctx, begin("TestB1"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB1", Outcome: "passed"})
	require.Nil(t, serr)
	// File c has no setup: its tests carry nothing of b's.
	_, serr = a.ProvenGive(ctx, end)
	require.Nil(t, serr)
	_, serr = a.ProvenBegin(ctx, begin("TestC1"))
	require.Nil(t, serr)
	_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestC1", Outcome: "passed"})
	require.Nil(t, serr)

	for _, id := range []string{"TestA1", "TestA2"} {
		require.Equal(t, []models.GivenValue{{Name: "given:uniq", Value: "a-1", Source: "a.spec.ts:9"}}, windowOf(t, a, "o::"+id).Given, id)
	}
	require.Equal(t, []models.GivenValue{{Name: "given:uniq", Value: "b-1", Source: "b.spec.ts:9"}}, windowOf(t, a, "o::TestB1").Given,
		"a value from file a is not given to file b's tests")
	require.Empty(t, windowOf(t, a, "o::TestC1").Given)

	a.provenMu.Lock()
	a.proven.closed = true
	a.provenMu.Unlock()
	_, serr = a.ProvenGive(ctx, end)
	require.NotNil(t, serr, "the run's results were read")
	require.Equal(t, models.ScopeErrNotInRun, serr.Code)
}

// Two files recorded in two workers each gave the same name once; replayed
// one after the other in one worker, with the first file's end between
// them, each file's test binds its own file's value with what its own
// recording gave under that variable, and both stay green.
func TestTwoFilesGivingOneNameReplayInOneWorker(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	o := &givingOpener{givingProxy: &givingProxy{wireProxy: p}}
	a.Proxy = o
	ctx := context.Background()
	require.NoError(t, a.SetScopeGiven(ctx, map[string][]models.GivenValue{
		"o::TestA": {{Name: "given:uniq", Value: "rec-a", Source: "a.spec.ts:9"}},
		"o::TestB": {{Name: "given:uniq", Value: "rec-b", Source: "b.spec.ts:9"}},
	}))
	claimSelf(t, a)
	for _, f := range []struct{ test, value, source string }{{"TestA", "new-a", "a.spec.ts:9"}, {"TestB", "new-b", "b.spec.ts:9"}} {
		resp, serr := a.ProvenGive(ctx, give("uniq", f.value, f.source))
		require.Nil(t, serr)
		require.Equal(t, "given:uniq", resp.Variable, f.test)
		_, serr = a.ProvenBegin(ctx, begin(f.test))
		require.Nil(t, serr)
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: f.test, Outcome: "passed"})
		require.Nil(t, serr)
		require.Empty(t, e.Action, "%s: %s", f.test, e.Reason)
		_, serr = a.ProvenGive(ctx, models.GivenReq{End: true})
		require.Nil(t, serr)
	}
	require.Equal(t, map[string][]models.VariableBinding{
		"o::TestA": {{Name: "given:uniq", Value: "new-a", Recorded: "rec-a", Mock: "test o::TestA", Place: "a.spec.ts:9", Given: true}},
		"o::TestB": {{Name: "given:uniq", Value: "new-b", Recorded: "rec-b", Mock: "test o::TestB", Place: "b.spec.ts:9", Given: true}},
	}, o.opened)
}

// In a run of several workers an end drops only the values of the worker
// whose process sent it.
func TestAnEndDropsOnlyItsOwnWorkersValues(t *testing.T) {
	a, _ := workerAgent(t, models.MODE_RECORD, 4)
	ends, stays := child(t), child(t)
	claimWorker(t, a, ends.Process.Pid, "a", 2)
	claimWorker(t, a, stays.Process.Pid, "b", 2)
	_, serr := a.ProvenGive(as(ends.Process.Pid), give("uniq", "u-ends", ""))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(as(stays.Process.Pid), give("uniq", "u-stays", ""))
	require.Nil(t, serr)
	resp, serr := a.ProvenGive(as(ends.Process.Pid), models.GivenReq{End: true})
	require.Nil(t, serr)
	require.Equal(t, "ok", resp.Status)
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	require.Equal(t, map[uint32][]models.GivenValue{2: {{Name: "given:uniq", Value: "u-stays"}}}, a.workerGiven)
}

// What a test process gives outside its tests is held only under a live
// claim of the run, and within the bounds a test's givens have: at most
// MaxGivensPerTest values, and in a record MaxGivenBytesPerRecord with the
// tests' own.
func TestAWorkerValueIsHeldOnlyUnderALiveClaimAndBounded(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	gone := exec.Command("sleep", "60")
	require.NoError(t, gone.Start())
	t.Cleanup(func() { _ = gone.Process.Kill(); _ = gone.Wait() })
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "o"}, gone.Process.Pid)
	require.Nil(t, serr)
	require.NoError(t, gone.Process.Kill())
	_ = gone.Wait()
	_, serr = a.ProvenGive(ctx, give("uniq", "u-1", ""))
	require.NotNil(t, serr, "the claim's process exited")
	require.Equal(t, models.ScopeErrNotOpen, serr.Code)

	claimSelf(t, a)
	for i := 0; i < MaxGivensPerTest; i++ {
		_, serr := a.ProvenGive(ctx, give("n", "v", ""))
		require.Nil(t, serr)
	}
	_, serr = a.ProvenGive(ctx, give("n", "v", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("gave more than %d values outside its tests", MaxGivensPerTest))

	b, _ := wireAgent(t, models.MODE_RECORD)
	claimSelf(t, b)
	v := models.GivenValue{Name: "given:n", Value: "v"}
	b.scopeMu.Lock()
	b.givenBytes = MaxGivenBytesPerRecord - givenSize(v)
	b.scopeMu.Unlock()
	_, serr = b.ProvenGive(ctx, give("n", "v", ""))
	require.Nil(t, serr, "exactly at the bound")
	_, serr = b.ProvenGive(ctx, give("m", "v", ""))
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "more than 64 MiB of values")
}

// A test holds at most MaxGivensPerTest values until its end.
func TestATestGivesABoundedNumberOfValues(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	for i := 0; i < MaxGivensPerTest; i++ {
		_, serr := a.ProvenGive(ctx, give("n", "v", ""))
		require.Nil(t, serr)
	}
	_, serr = a.ProvenGive(ctx, give("n", "v", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, fmt.Sprintf("more than %d values", MaxGivensPerTest))
	_, serr = a.ProvenGive(ctx, give("n", string(make([]byte, models.MaxGivenValue+1)), ""))
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "longer than")
	_, serr = a.ProvenGive(ctx, give("m", "v", string(make([]byte, models.MaxGivenSource+1))))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, "source is longer than")
}

// A record holds each test's givens with its window until the run's results
// are read: at most MaxGivenBytesPerRecord in a session, counted as they are
// given and let go with the session's windows.
func TestARecordHoldsABoundedTotalOfGivens(t *testing.T) {
	a, _ := wireAgent(t, models.MODE_RECORD)
	ctx := context.Background()
	claimSelf(t, a)
	_, serr := a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("n", "v", "a_test.go:9"))
	require.Nil(t, serr)
	a.scopeMu.Lock()
	require.Equal(t, len("given:n")+len("v")+len("a_test.go:9"), a.givenBytes)
	a.givenBytes = MaxGivenBytesPerRecord - len("given:m")
	a.scopeMu.Unlock()
	_, serr = a.ProvenGive(ctx, give("m", "v", ""))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrBadRequest, serr.Code)
	require.Contains(t, serr.Message, "more than 64 MiB of values")
	a.resetScopeState()
	a.scopeMu.Lock()
	require.Zero(t, a.givenBytes, "a new session holds none")
	a.scopeMu.Unlock()
}

// A replayed test that fails while it gave a value its recording does not
// hold (recorded before the test gave it) says so first, naming the given
// and the step that fixes it; one its recording holds is not named, and a
// test that passes is not failed for it.
func TestAGivenItsRecordingLacksSaysToReRecordTheTest(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	giving(a, p)
	ctx := context.Background()
	require.NoError(t, a.SetScopeGiven(ctx, map[string][]models.GivenValue{
		"orders::TestB": {{Name: "given:app", Value: "app-1"}},
		"orders::TestC": {{Name: "given:app", Value: "app-1"}},
		"orders::TestD": {{Name: "given:app", Value: "app-1"}},
	}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	const lacks = "the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test"

	// Passing: nothing to say.
	_, serr = a.ProvenBegin(ctx, begin("TestA"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
	require.Nil(t, serr)
	e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: "TestA", Outcome: "passed"})
	require.Nil(t, serr)
	require.NotEqual(t, models.ScopeActionFail, e.Action, "a given the recording lacks fails no test by itself")

	// Failing: the given the recording lacks is named first, the recorded one not.
	_, serr = a.ProvenBegin(ctx, begin("TestB"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("app", "app-9", ""))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestB", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: "+lacks+". POST /audit to 127.0.0.1:18282 had no recorded answer"), e.Reason)
	require.NotContains(t, e.Reason, "given:app")
	reasonB := e.Reason

	// Failing with every given recorded: no such sentence.
	_, serr = a.ProvenBegin(ctx, begin("TestC"))
	require.Nil(t, serr)
	_, serr = a.ProvenGive(ctx, give("app", "app-10", ""))
	require.Nil(t, serr)
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestC", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.NotContains(t, e.Reason, "recording does not hold")

	// Failing after giving a name twice that its recording holds once: the
	// second value (given:app#2) is the one it lacks, named by its number
	// and alone. Its name starts with the recorded one's; it is not that one.
	_, serr = a.ProvenBegin(ctx, begin("TestD"))
	require.Nil(t, serr)
	for _, v := range []string{"app-11", "app-12"} {
		_, serr = a.ProvenGive(ctx, give("app", v, ""))
		require.Nil(t, serr)
	}
	p.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
	e, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: "TestD", Outcome: "passed"})
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: the test gave given:app#2, which its recording does not hold (recorded before the test gave it): re-record the test. POST /audit to 127.0.0.1:18282 had no recorded answer"), e.Reason)
	require.Equal(t, 1, strings.Count(e.Reason, "re-record the test"), e.Reason)
	require.Equal(t, reasonB, windowOf(t, a, "orders::TestB").Evidence.Reason, "the window keeps the reason")
}

func TestNotRecordedTextNamesTheFirstFew(t *testing.T) {
	require.Equal(t, "", notRecordedText(nil))
	require.Equal(t, "the test gave given:a and given:b, which its recording does not hold (recorded before the test gave them): re-record the test",
		notRecordedText([]string{"given:a", "given:b"}))
	require.Equal(t, "the test gave given:a, given:b, given:c, given:d, given:e and 2 more, which its recording does not hold (recorded before the test gave them): re-record the test",
		notRecordedText([]string{"given:a", "given:b", "given:c", "given:d", "given:e", "given:f", "given:g"}))
}

// givingLossProxy is a giving proxy that reports the calls it could not
// capture, as the proxy does.
type givingLossProxy struct {
	*givingProxy
	losses models.CaptureLosses
}

func (p *givingLossProxy) CaptureLosses() models.CaptureLosses { return p.losses }
func (p *givingLossProxy) SetMappedUniverse([]string)          {}

// The sentence that says to re-record comes with a replayed failure that has
// a call that missed or a recorded call the test never made, and with no
// other: not with a loss of capture alone. It stays first where the test is
// charged a loss too, whose text writes the evidence's again.
func TestTheReRecordSentenceComesWithAMissOrAnUnmadeCall(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &givingLossProxy{givingProxy: giving(a, wp)}
	a.Proxy = p
	a.proven.losses = a.markCaptureLosses(context.Background())
	ctx := context.Background()
	t0 := time.Date(2026, 10, 22, 11, 14, 17, 0, time.UTC)
	require.NoError(t, a.StoreMocks(ctx, nil, []*models.Mock{pingMock("mock-u", "/audit", "ok", t0)}))
	require.NoError(t, a.SetScopeTable(ctx, map[string][]string{"orders::TestUnmade": {"mock-u"}}))
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	const lacks = "keploy: the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test. "
	run := func(name string, during func()) models.ScopeEndResp {
		t.Helper()
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
		require.Nil(t, serr)
		during()
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
		require.Nil(t, serr)
		require.Equal(t, models.ScopeActionFail, e.Action, name)
		return e
	}
	lose := func(n uint64) {
		p.losses = models.CaptureLosses{{Kind: models.CaptureLossTLSAbort, Count: n, What: "to 10.0.0.1:443"}}
	}

	// Only a recorded call the test never made.
	e := run("TestUnmade", func() {})
	require.True(t, strings.HasPrefix(e.Reason, lacks), e.Reason)
	require.Contains(t, e.Reason, "mock-u")

	// Only a loss of capture: no call missed and none is unmade.
	e = run("TestLost", func() { lose(1) })
	require.Contains(t, e.Reason, "TLS handshake")
	require.NotContains(t, e.Reason, "re-record the test", "a loss alone is no sign the recording lacks the given")

	// A miss and a loss: the sentence first, the loss after it.
	e = run("TestMissedAndLost", func() {
		lose(2)
		wp.mu.Lock()
		wp.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
		wp.mu.Unlock()
	})
	require.True(t, strings.HasPrefix(e.Reason, lacks), e.Reason)
	require.Contains(t, e.Reason, "TLS handshake")
	require.Contains(t, e.Reason, "POST /audit to 127.0.0.1:18282 had no recorded answer")
	require.Equal(t, 1, strings.Count(e.Reason, "re-record the test"), e.Reason)
	require.Equal(t, e.Reason, windowOf(t, a, "orders::TestMissedAndLost").Evidence.Reason, "the window keeps the reason")
}

// givingUnreadProxy is a giving proxy whose read of the misses returns the
// ones it found with an error, as the agent client does when the agent says
// its list is incomplete (models.ErrUnmatchedCallsUnread).
type givingUnreadProxy struct {
	*givingProxy
	readErr error
}

func (p *givingUnreadProxy) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	misses, err := p.givingProxy.GetMockErrors(ctx)
	if err == nil {
		err = p.readErr
	}
	return misses, err
}

// A replayed test keploy could not read is unread, not judged: it is not
// told to re-record, though its read returned a miss beside the error and
// it gave a value its recording does not hold. Read whole, the same test is.
func TestAnUnreadTestIsNotToldToReRecord(t *testing.T) {
	a, wp := wireAgent(t, models.MODE_TEST)
	p := &givingUnreadProxy{givingProxy: giving(a, wp)}
	a.Proxy = p
	ctx := context.Background()
	_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
	require.Nil(t, serr)
	run := func(name string, readErr error) (*models.ScopeError, models.ScopeEndResp) {
		t.Helper()
		_, serr := a.ProvenBegin(ctx, begin(name))
		require.Nil(t, serr)
		_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
		require.Nil(t, serr)
		p.readErr = readErr
		wp.mu.Lock()
		wp.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
		wp.mu.Unlock()
		e, serr := a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
		return serr, e
	}

	serr, e := run("TestRead", nil)
	require.Nil(t, serr)
	require.Equal(t, models.ScopeActionFail, e.Action)
	require.True(t, strings.HasPrefix(e.Reason, "keploy: the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test. "), e.Reason)

	serr, _ = run("TestUnread", fmt.Errorf("%w (agent: the drain did not finish)", models.ErrUnmatchedCallsUnread))
	require.NotNil(t, serr, "a test keploy could not read fails its end")
	w := windowOf(t, a, "orders::TestUnread")
	require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
	require.True(t, w.Evidence.Unread)
	require.Len(t, w.Evidence.Misses, 1, "the miss read beside the error is the test's")
	require.True(t, strings.HasPrefix(w.Evidence.Reason, "keploy: keploy could not tell what orders::TestUnread did: reading its misses: "), w.Evidence.Reason)
	require.NotContains(t, w.Evidence.Reason, "re-record the test", "an unread test is not judged, so it is not told to re-record")
}

// A replayed test whose misses keploy read is told to re-record, once,
// though keploy could not settle it, could not read its capture losses, or
// ended it itself: its reason still names those misses. After keploy's
// note when it could not settle the test or ended it (left open), before
// the capture-loss note, which writes the evidence's reason again.
func TestTheReRecordSentenceGoesWithTheMissesRead(t *testing.T) {
	const lacks = "the test gave given:nonce, which its recording does not hold (recorded before the test gave it): re-record the test"
	const missed = "POST /audit to 127.0.0.1:18282 had no recorded answer"
	for _, c := range []struct {
		name   string
		test   string
		unread bool
		// refused: the end call is refused, so the reason is the window's.
		refused bool
		// fault makes keploy unable to judge the test; nil leaves it be.
		fault func(h *countHooks)
		// end ends the test, or ends another, which leaves keploy to end it.
		end    func(a *Agent) (models.ScopeEndResp, *models.ScopeError)
		prefix string
	}{{
		name:    "unsettled",
		test:    "TestU",
		unread:  true,
		refused: true,
		fault: func(h *countHooks) {
			h.mu.Lock()
			h.err = models.ErrProxyConnsUnsupported
			h.mu.Unlock()
		},
		end: func(a *Agent) (models.ScopeEndResp, *models.ScopeError) {
			return a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestU", Outcome: "passed"})
		},
		prefix: "keploy: keploy could not tell that every call of orders::TestU reached it: keploy cannot count the calls still waiting for its proxy: " +
			models.ErrProxyConnsUnsupported.Error() + ". " + lacks + ". " + missed,
	}, {
		name:   "capture losses unread",
		test:   "TestL",
		unread: true,
		fault:  func(h *countHooks) { h.countingHooks.err = errors.New("the pid map cannot be read") },
		end: func(a *Agent) (models.ScopeEndResp, *models.ScopeError) {
			return a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestL", Outcome: "passed"})
		},
		prefix: "keploy: " + lacks + ". keploy could not tell whether every call of orders::TestL was captured: read the PID map refusals: the pid map cannot be read. " + missed,
	}, {
		name:    "left open",
		test:    "TestW",
		refused: true,
		end: func(a *Agent) (models.ScopeEndResp, *models.ScopeError) {
			return a.ProvenEnd(context.Background(), models.ScopeReq{Name: "TestOther", Outcome: "passed"})
		},
		prefix: "keploy: orders::TestOther ended while orders::TestW was the open test. " + lacks + ". " + missed,
	}} {
		t.Run(c.name, func(t *testing.T) {
			a, wp := wireAgent(t, models.MODE_TEST)
			giving(a, wp)
			ctx := context.Background()
			_, serr := a.ClaimInstance(ctx, models.InstanceReq{Key: "orders"}, os.Getpid())
			require.Nil(t, serr)
			_, serr = a.ProvenBegin(ctx, begin(c.test))
			require.Nil(t, serr)
			_, serr = a.ProvenGive(ctx, give("nonce", "1761609600123456789", ""))
			require.Nil(t, serr)
			wp.mu.Lock()
			wp.misses = []models.UnmatchedCall{{Protocol: "HTTP", ActualSummary: "POST /audit", Destination: "127.0.0.1:18282"}}
			wp.mu.Unlock()
			if c.fault != nil {
				c.fault(a.Hooks.(*countHooks))
			}
			e, serr := c.end(a)
			if c.refused {
				require.NotNil(t, serr)
			} else {
				require.Nil(t, serr)
				require.Equal(t, models.ScopeActionFail, e.Action)
				require.True(t, strings.HasPrefix(e.Reason, c.prefix), e.Reason)
			}
			w := windowOf(t, a, "orders::"+c.test)
			require.Equal(t, models.ScopeOutcomeFailed, w.Outcome)
			require.Equal(t, c.unread, w.Evidence.Unread)
			require.Len(t, w.Evidence.Misses, 1, "the test's misses were read")
			require.True(t, strings.HasPrefix(w.Evidence.Reason, c.prefix), w.Evidence.Reason)
			require.Equal(t, 1, strings.Count(w.Evidence.Reason, "re-record the test"), w.Evidence.Reason)
		})
	}
}

// GiveBetweenTests and EndBetweenTests make the giving proxy keep the
// givens in force for a worker's calls outside its tests, as keploy's own
// proxy does (coreAgent.ExactBetweenGiver): it records each binding and
// each end, in order.
func (p *givingProxy) GiveBetweenTests(b models.VariableBinding) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.betweenErr != nil {
		return p.betweenErr
	}
	p.between = append(p.between, b)
	return nil
}

func (p *givingProxy) EndBetweenTests() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.betweenErr != nil {
		return p.betweenErr
	}
	p.between = append(p.between, models.VariableBinding{Name: "end"})
	return nil
}

// In a replay a value a test file's setup gives is bound too for the calls
// the setup makes itself, outside every test (the proxy's gap), with the
// value the recording's setup gave at the same place; a file's end ends
// them, so the next file's setup calls follow only its own value. A record
// binds nothing: it keeps the value in each later test's window.
func TestASetupsOwnCallsFollowItsValuePerFile(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	o := &givingOpener{givingProxy: &givingProxy{wireProxy: p}}
	a.Proxy = o
	ctx := context.Background()
	require.NoError(t, a.SetScopeGiven(ctx, map[string][]models.GivenValue{
		"o::TestA1": {{Name: "given:suite", Value: "rec-a", Source: "a.spec.ts:9"}},
		"o::TestA2": {{Name: "given:suite", Value: "rec-a", Source: "a.spec.ts:9"}},
		"o::TestB":  {{Name: "given:suite", Value: "rec-b", Source: "b.spec.ts:9"}},
	}))
	claimSelf(t, a)
	for _, f := range []struct {
		value, source string
		tests         []string
	}{{"new-a", "a.spec.ts:9", []string{"TestA1", "TestA2"}}, {"new-b", "b.spec.ts:9", []string{"TestB"}}} {
		resp, serr := a.ProvenGive(ctx, give("suite", f.value, f.source))
		require.Nil(t, serr)
		require.Equal(t, "given:suite", resp.Variable)
		for _, name := range f.tests {
			_, serr = a.ProvenBegin(ctx, begin(name))
			require.Nil(t, serr)
			_, serr = a.ProvenEnd(ctx, models.ScopeReq{Name: name, Outcome: "passed"})
			require.Nil(t, serr)
		}
		_, serr = a.ProvenGive(ctx, models.GivenReq{End: true})
		require.Nil(t, serr)
	}
	require.Equal(t, []models.VariableBinding{
		{Name: "given:suite", Value: "new-a", Recorded: "rec-a", Mock: "test worker " + coreAgent.InstanceLabel(0), Place: "a.spec.ts:9", Given: true},
		{Name: "end"},
		{Name: "given:suite", Value: "new-b", Recorded: "rec-b", Mock: "test worker " + coreAgent.InstanceLabel(0), Place: "b.spec.ts:9", Given: true},
		{Name: "end"},
	}, o.between)

	r, rp := wireAgent(t, models.MODE_RECORD)
	ro := &givingOpener{givingProxy: &givingProxy{wireProxy: rp}}
	r.Proxy = ro
	claimSelf(t, r)
	_, serr := r.ProvenGive(ctx, give("suite", "rec-a", "a.spec.ts:9"))
	require.Nil(t, serr)
	_, serr = r.ProvenGive(ctx, models.GivenReq{End: true})
	require.Nil(t, serr)
	require.Empty(t, ro.between, "a record binds no variable")
}

// The value the recording's setup gave is read from the tests begun after
// it, by variable and place; none when the place is unknown, when no test
// holds it, or when two hold two values there: the binding then checks
// nothing. A proxy that cannot keep the givens of a worker's own calls, or
// fails to, refuses the value, which no later test then carries.
func TestASetupsValueIsReadFromItsPlaceOrRefused(t *testing.T) {
	a, p := wireAgent(t, models.MODE_TEST)
	a.testGiven = map[string][]models.GivenValue{
		"o::A": {{Name: "given:suite", Value: "rec-a", Source: "a.spec.ts:9"}, {Name: "given:suite#2", Value: "own", Source: "a.spec.ts:9"}},
		"o::B": {{Name: "given:suite", Value: "rec-a", Source: "a.spec.ts:9"}, {Name: "given:suite", Value: "rec-c", Source: "c.spec.ts:9"}},
		"o::C": {{Name: "given:twice", Value: "t-1", Source: "t.spec.ts:3"}},
		"o::D": {{Name: "given:twice", Value: "t-2", Source: "t.spec.ts:3"}},
		"o::E": {{Name: "given:suite", Value: "rec-e"}},
	}
	require.Equal(t, "rec-a", a.recordedOutside("given:suite", "a.spec.ts:9"))
	require.Equal(t, "rec-c", a.recordedOutside("given:suite", "c.spec.ts:9"))
	require.Equal(t, "own", a.recordedOutside("given:suite#2", "a.spec.ts:9"))
	require.Empty(t, a.recordedOutside("given:suite", ""), "no place")
	require.Empty(t, a.recordedOutside("given:suite", "z.spec.ts:9"), "no test holds it")
	require.Empty(t, a.recordedOutside("given:twice", "t.spec.ts:3"), "two values at one place")

	claimSelf(t, a)
	ctx := context.Background()
	_, serr := a.ProvenGive(ctx, give("suite", "new-a", "a.spec.ts:9"))
	require.NotNil(t, serr)
	require.Equal(t, models.ScopeErrInternal, serr.Code)
	require.Contains(t, serr.Message, "this keploy agent's proxy keeps no test variables")
	_, serr = a.ProvenGive(ctx, models.GivenReq{End: true})
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "the end of the values given outside tests")

	failing := &givingProxy{wireProxy: p, betweenErr: errors.New("table gone")}
	a.Proxy = failing
	_, serr = a.ProvenGive(ctx, give("suite", "new-a", "a.spec.ts:9"))
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "table gone")
	_, serr = a.ProvenGive(ctx, models.GivenReq{End: true})
	require.NotNil(t, serr)
	require.Contains(t, serr.Message, "table gone")
	a.scopeMu.Lock()
	defer a.scopeMu.Unlock()
	require.Empty(t, a.workerGiven, "a value the proxy did not bind is held for no test")
}
