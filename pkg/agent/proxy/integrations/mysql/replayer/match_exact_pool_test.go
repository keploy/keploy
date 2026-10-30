package replayer_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// poolQuery is the prepared statement a pool of connections runs.
const poolQuery = "SELECT id, name FROM small WHERE id = ?"

// poolRecording is one test's recording of poolQuery run on conns
// connections, statement 1 on each: a PREPARE per connection, then, on
// connection c<i+1>, the EXECUTEs with params[i], in recorded order. The
// EXECUTEs are the test's own (per-test), and each answers with its name,
// exec-c<i+1>-<param>, or exec-c<i+1>-<param>-<n> for the n-th recording
// of the same parameter on the same connection.
func poolRecording(params [][]string) (preps, execs []*models.Mock) {
	ts := diffBase.Add(time.Millisecond)
	for i, ps := range params {
		conn := fmt.Sprintf("c%d", i+1)
		preps = append(preps, diffPrepareMock("prep-"+conn, conn, poolQuery, 1, ts))
		seen := map[string]int{}
		for _, p := range ps {
			ts = ts.Add(100 * time.Microsecond)
			name := fmt.Sprintf("exec-%s-%s", conn, p)
			if seen[p]++; seen[p] > 1 {
				name = fmt.Sprintf("%s-%d", name, seen[p])
			}
			execs = append(execs, diffMock(name, conn, []mysql.Request{executeReq(1, p)}, ts, models.LifetimePerTest, "mocks"))
		}
	}
	return preps, execs
}

// stagePool stages a recording as keploy test does (exact false: the test's
// window holds it) or as the proven switch serves it (exact: test T's
// mapping is its EXECUTEs).
func stagePool(t *testing.T, preps, execs []*models.Mock, exact bool) *proxy.MockManager {
	t.Helper()
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	all := append(append([]*models.Mock{}, preps...), execs...)
	if exact {
		mm.SetMocksWithWindow(nil, all, models.BaseTime, time.Now().Add(time.Hour))
		mm.ServeExactTest("T", execs)
		return mm
	}
	start, end := diffWindow(0)
	mm.SetMocksWithWindow(nil, all, start, end.Add(time.Hour))
	return mm
}

func poolConn(conn string) (context.Context, *wire.DecodeContext) {
	ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, conn)
	return ctx, &wire.DecodeContext{
		PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
		StmtIDToQuery:      map[uint32]string{1: poolQuery},
		NextStmtID:         100,
	}
}

// One connection runs a prepared statement with each parameter it recorded,
// first in recorded order, then in reverse: each EXECUTE is answered with the
// recording of its own parameters, by keploy test and under exact serving.
// Exact serving opens no window, and the matcher, ranking by the window, took
// the first recording of the statement whose parameters differ over the
// command's own (in recorded order the id 9 got id 10's rows, and id 10 got
// id 9's).
func TestAPreparedStatementRunWithEachParameterIsAnsweredByThatParametersRecording(t *testing.T) {
	params := []string{"9", "10", "11", "12", "13", "14", "15", "16"}
	for _, exact := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("exact=%v/reverse=%v", exact, reverse), func(t *testing.T) {
				preps, execs := poolRecording([][]string{params})
				mm := stagePool(t, preps, execs, exact)
				ctx, dctx := poolConn("c1")
				for i := range params {
					p := params[i]
					if reverse {
						p = params[len(params)-1-i]
					}
					resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(1, p), mm, dctx)
					if err != nil || !ok {
						t.Fatalf("EXECUTE id=%s: ok=%v err=%v miss=%s", p, ok, err, miss)
					}
					if want := "exec-c1-" + p; resp.Payload != want {
						t.Fatalf("EXECUTE id=%s was answered with %s; want %s", p, resp.Payload, want)
					}
				}
			})
		}
	}
}

// A prepared EXECUTE stops its scan at the open test's own recording of its
// query and parameters, under exact serving as under keploy test. The scan
// stops at a recording that is the test's own (stmtMatched); read by the
// window, exact serving had none, so every EXECUTE walked the whole pool, twice
// (at 10k recordings some 11 ms a call on the box), and then served another
// parameters' recording. Counted, not timed: the first id of 1000 is
// answered after the PREPARE and its own recording; read by the window, the
// scan visited 2002 candidates of the 1001.
func TestAPreparedExecuteStopsItsScanAtItsOwnRecording(t *testing.T) {
	params := make([]string, 1000)
	for i := range params {
		params[i] = fmt.Sprint(i + 1)
	}
	for _, exact := range []bool{false, true} {
		t.Run(fmt.Sprintf("exact=%v", exact), func(t *testing.T) {
			preps, execs := poolRecording([][]string{params})
			mm := stagePool(t, preps, execs, exact)
			ctx, dctx := poolConn("c1")
			resp, ok, examined, err := replayer.MatchExaminedForTest(ctx, zap.NewNop(), executeReq(1, "1"), mm, dctx)
			if err != nil || !ok {
				t.Fatalf("EXECUTE id=1: ok=%v err=%v", ok, err)
			}
			if resp.Payload != "exec-c1-1" {
				t.Errorf("EXECUTE id=1 was answered with %s; want exec-c1-1", resp.Payload)
			}
			if want := len(preps) + 1; examined > want {
				t.Errorf("EXECUTE id=1 examined %d candidates of %d; want at most %d (the PREPARE and its own recording)", examined, len(preps)+len(execs), want)
			}
		})
	}
}

// heldHook is the store a command's match holds (HoldToRound), with its
// consume doors watched: before the first, the command arrives at a barrier
// and waits there until every connection's command has, so all of them have
// read the pool before any consumes. It forwards what the matcher asks the
// held store under exact serving.
type heldHook struct {
	integrations.MockMemDb
	arrive  func()
	barrier *sync.WaitGroup
	once    sync.Once
	// treeDoorsFail has DeleteFilteredMock and UpdateUnFilteredMock turn down
	// every consume without asking the store, as a tree that does not hold
	// the view's copy does.
	treeDoorsFail bool
	// refuse are the recordings MarkMockAsUsed turns down without asking
	// the store, which goes on listing them, each at most refuseAtMost times
	// (so a matcher that asks again ends), counted by name in refusals.
	refuse       []string
	refuseAtMost int
	mu           sync.Mutex
	deletes      []bool
	refusals     map[string]int
}

func (h *heldHook) wait() {
	h.once.Do(func() {
		if h.arrive != nil {
			h.arrive()
			h.barrier.Wait()
		}
	})
}

func (h *heldHook) ExactServing() bool { return integrations.ExactServing(h.MockMemDb) }

func (h *heldHook) OwnedByOpenTest(m *models.Mock) bool {
	return integrations.OwnedByOpenTest(h.MockMemDb, m)
}

func (h *heldHook) DeleteFilteredMock(m models.Mock) bool {
	h.wait()
	ok := !h.treeDoorsFail && h.MockMemDb.DeleteFilteredMock(m)
	h.mu.Lock()
	h.deletes = append(h.deletes, ok)
	h.mu.Unlock()
	return ok
}

func (h *heldHook) UpdateUnFilteredMock(old, nw *models.Mock) bool {
	h.wait()
	return !h.treeDoorsFail && h.MockMemDb.UpdateUnFilteredMock(old, nw)
}

func (h *heldHook) MarkMockAsUsed(m models.Mock) bool {
	h.wait()
	if slices.Contains(h.refuse, m.Name) && h.refusals[m.Name] < h.refuseAtMost {
		if h.refusals == nil {
			h.refusals = map[string]int{}
		}
		h.refusals[m.Name]++
		return false
	}
	return h.MockMemDb.MarkMockAsUsed(m)
}

// hookedStore is the agent's store whose held stores are heldHooks.
type hookedStore struct {
	*proxy.MockManager
	hook *heldHook
}

func (s *hookedStore) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	held, restaged := s.MockManager.HoldToRound(call)
	s.hook.MockMemDb = held
	return s.hook, restaged
}

type poolAnswer struct {
	payload string
	ok      bool
	miss    string
	err     error
	deletes []bool
	held    bool
}

// runPoolAtOnce runs, on connection c<i+1>, an EXECUTE of statement 1 with
// params[i], every connection's at once: each reads the pool before any
// consumes.
func runPoolAtOnce(mm *proxy.MockManager, params []string) []poolAnswer {
	reqs := make([]mysql.Request, len(params))
	for i, p := range params {
		reqs[i] = executeReq(1, p)
	}
	return runAtOnce(mm, reqs)
}

// runAtOnce runs reqs[i] on connection c<i+1>, every connection's at once:
// each reads the pool before any consumes.
func runAtOnce(mm *proxy.MockManager, reqs []mysql.Request) []poolAnswer {
	return atOnce(mm, len(reqs), func(i int, db integrations.MockMemDb) (*mysql.Response, bool, string, error) {
		ctx, dctx := poolConn(fmt.Sprintf("c%d", i+1))
		return replayer.MatchForTest(ctx, zap.NewNop(), reqs[i], db, dctx)
	})
}

// atOnce runs match(i, db) for each i below n, all at once, each against the
// store mm holds for it (heldHook): each reads the pool before any consumes.
func atOnce(mm *proxy.MockManager, n int, match func(i int, db integrations.MockMemDb) (*mysql.Response, bool, string, error)) []poolAnswer {
	var barrier, done sync.WaitGroup
	barrier.Add(n)
	out := make([]poolAnswer, n)
	for i := 0; i < n; i++ {
		done.Add(1)
		arrive := sync.OnceFunc(barrier.Done)
		hook := &heldHook{arrive: arrive, barrier: &barrier}
		go func() {
			defer done.Done()
			// A command that consumes nothing arrives too, so no other waits
			// for it.
			defer arrive()
			resp, ok, miss, err := match(i, &hookedStore{MockManager: mm, hook: hook})
			a := poolAnswer{ok: ok, miss: miss, err: err, held: hook.MockMemDb != nil}
			if resp != nil {
				a.payload = resp.Payload
			}
			hook.mu.Lock()
			a.deletes = append(a.deletes, hook.deletes...)
			hook.mu.Unlock()
			out[i] = a
		}()
	}
	done.Wait()
	return out
}

// Eight connections run one prepared statement at once under exact serving,
// each with its own parameter: each is answered with its own recording,
// consumed once, and no consume is refused. All eight read the pool before
// any consumed, and each took the first recording of the statement (whose
// parameters were another connection's): all but one were refused, and the
// connection closed.
func TestConnectionsRunningOnePreparedStatementAtOnceAreEachAnsweredByTheirOwnRecording(t *testing.T) {
	params := []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	recorded := make([][]string, len(params))
	for i, p := range params {
		recorded[i] = []string{p}
	}
	preps, execs := poolRecording(recorded)
	mm := stagePool(t, preps, execs, true)
	for i, a := range runPoolAtOnce(mm, params) {
		if !a.held {
			t.Fatal("precondition: the command's match held no store")
		}
		want := fmt.Sprintf("exec-c%d-%s", i+1, params[i])
		if a.err != nil || !a.ok || a.payload != want {
			t.Errorf("connection c%d was answered with %q (ok=%v err=%v miss=%s); want %s", i+1, a.payload, a.ok, a.err, a.miss, want)
		}
		if len(a.deletes) != 1 || !a.deletes[0] {
			t.Errorf("connection c%d consumed %v; want its own recording, consumed at the first try", i+1, a.deletes)
		}
	}
}

// Eight connections run one prepared statement at once under exact serving,
// all with the same parameter, which the test recorded eight times: each
// recording answers exactly one of them. All eight read the first recording;
// the store refuses all consumes of it but one, and each refused command is
// matched again against what the store shows now, never answered twice by one
// recording, nor refused.
func TestConnectionsRunningOneStatementWithTheSameParametersAtOnceEachTakeOneRecording(t *testing.T) {
	const n = 8
	params := make([]string, n)
	recorded := make([][]string, n)
	for i := range params {
		params[i] = "7"
		recorded[i] = []string{"7"}
	}
	preps, execs := poolRecording(recorded)
	mm := stagePool(t, preps, execs, true)
	served := map[string]int{}
	for i, a := range runPoolAtOnce(mm, params) {
		if !a.held {
			t.Fatal("precondition: the command's match held no store")
		}
		if a.err != nil || !a.ok {
			t.Errorf("connection c%d: ok=%v err=%v miss=%s; want an answer", i+1, a.ok, a.err, a.miss)
			continue
		}
		served[a.payload]++
	}
	for _, mk := range execs {
		if served[mk.Name] != 1 {
			t.Errorf("recording %s answered %d command(s); want 1 (served: %v)", mk.Name, served[mk.Name], served)
		}
	}
}

// Under exact serving a consume the update doors turn down without the view
// refusing it (the tree they update does not hold the view's copy) is no
// refusal: MarkMockAsUsed says so, and the command is answered with the
// recording it matched, never matched again to another one.
func TestAConsumeTheTreeTurnsDownButTheViewAcceptsAnswersWithTheMatchedRecording(t *testing.T) {
	preps, execs := poolRecording([][]string{{"1", "2"}})
	mm := stagePool(t, preps, execs, true)
	ctx, dctx := poolConn("c1")
	hook := &heldHook{treeDoorsFail: true}
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(1, "2"), &hookedStore{MockManager: mm, hook: hook}, dctx)
	if hook.MockMemDb == nil {
		t.Fatal("precondition: the command's match held no store")
	}
	if err != nil || !ok || resp.Payload != "exec-c1-2" {
		t.Fatalf("EXECUTE id=2 was answered with %v (ok=%v err=%v miss=%s); want exec-c1-2", resp, ok, err, miss)
	}
}

// A consume the store refuses while it goes on listing the copy (a view
// changed under the read, before its next read shows the change) is not
// asked again: the command is matched again without that copy, and answered
// with the next recording it may take.
func TestARefusedCopyTheStoreStillListsIsNotTriedAgain(t *testing.T) {
	preps, execs := poolRecording([][]string{{"7", "7"}})
	mm := stagePool(t, preps, execs, true)
	ctx, dctx := poolConn("c1")
	hook := &heldHook{treeDoorsFail: true, refuse: []string{"exec-c1-7"}, refuseAtMost: 3}
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(1, "7"), &hookedStore{MockManager: mm, hook: hook}, dctx)
	if hook.MockMemDb == nil {
		t.Fatal("precondition: the command's match held no store")
	}
	if n := hook.refusals[hook.refuse[0]]; n != 1 {
		t.Fatalf("the refused copy was tried %d time(s); want once", n)
	}
	if err != nil || !ok || resp.Payload != "exec-c1-7-2" {
		t.Fatalf("EXECUTE id=7 was answered with %v (ok=%v err=%v miss=%s); want exec-c1-7-2", resp, ok, err, miss)
	}
}

// Each match again passes over every copy the store refused before, not
// only the last one: two copies it refuses while it goes on listing them are
// each asked once, and the third answers.
func TestEachRefusedCopyIsAskedOnce(t *testing.T) {
	preps, execs := poolRecording([][]string{{"7", "7", "7"}})
	mm := stagePool(t, preps, execs, true)
	ctx, dctx := poolConn("c1")
	hook := &heldHook{treeDoorsFail: true, refuse: []string{"exec-c1-7", "exec-c1-7-2"}, refuseAtMost: 3}
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(1, "7"), &hookedStore{MockManager: mm, hook: hook}, dctx)
	if hook.MockMemDb == nil {
		t.Fatal("precondition: the command's match held no store")
	}
	for _, name := range hook.refuse {
		if n := hook.refusals[name]; n != 1 {
			t.Errorf("the refused copy %s was asked %d time(s); want once", name, n)
		}
	}
	if err != nil || !ok || resp.Payload != "exec-c1-7-3" {
		t.Fatalf("EXECUTE id=7 was answered with %v (ok=%v err=%v miss=%s); want exec-c1-7-3", resp, ok, err, miss)
	}
}

// Under exact serving a connection's recording (a PREPARE, lifetime
// connection) whose tree update is turned down while the view accepts its
// consume answers the command: MarkMockAsUsed says so at the session and
// connection door too. Taken for a refusal, the PREPARE was matched again
// and answered with another connection's PREPARE, or missed.
func TestAConnectionRecordingTheTreeTurnsDownButTheViewAcceptsAnswersTheCommand(t *testing.T) {
	preps, execs := poolRecording([][]string{{"1"}, {"2"}})
	mm := stagePool(t, preps, execs, true)
	ctx, dctx := poolConn("c1")
	hook := &heldHook{treeDoorsFail: true}
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), prepareReq(poolQuery), &hookedStore{MockManager: mm, hook: hook}, dctx)
	if hook.MockMemDb == nil {
		t.Fatal("precondition: the command's match held no store")
	}
	if err != nil || !ok || resp.Payload != "prep-c1" {
		t.Fatalf("PREPARE on c1 was answered with %v (ok=%v err=%v miss=%s); want prep-c1", resp, ok, err, miss)
	}
}

// failingDoors is a store without exact serving whose consume doors all fail:
// failed bookkeeping, not a refusal.
type failingDoors struct {
	*proxy.MockManager
	mu    sync.Mutex
	calls int
}

func (f *failingDoors) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return f, func() bool { return false }
}
func (f *failingDoors) ExactServing() bool { return false }
func (f *failingDoors) door() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return false
}
func (f *failingDoors) DeleteFilteredMock(models.Mock) bool         { return f.door() }
func (f *failingDoors) UpdateUnFilteredMock(_, _ *models.Mock) bool { return f.door() }
func (f *failingDoors) MarkMockAsUsed(models.Mock) bool             { return f.door() }

// Without exact serving a consume the store turns down is failed bookkeeping,
// as before: the command fails with the error it always did, after one try,
// and is never matched again to another recording.
func TestWithoutExactServingAFailedConsumeIsTheErrorItWas(t *testing.T) {
	preps, execs := poolRecording([][]string{{"1", "2"}})
	mm := stagePool(t, preps, execs, false)
	ctx, dctx := poolConn("c1")
	db := &failingDoors{MockManager: mm}
	resp, ok, _, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(1, "1"), db, dctx)
	if ok || err == nil || err.Error() != "failed to update matched mock" {
		t.Fatalf("a failed consume without exact serving: resp=%v ok=%v err=%v; want the error it was", resp, ok, err)
	}
	if db.calls != 2 {
		t.Fatalf("the consume doors were asked %d time(s); want 2 (one try: delete, then update)", db.calls)
	}
}

// poolTextQuery is the text query a pool of connections runs.
const poolTextQuery = "SELECT id, name FROM small WHERE id = 7"

// queryRecording is one test's recording of poolTextQuery: times[i]
// recordings on connection c<i+1>, in recorded order, each the test's own
// (per-test) and answering with its name, query-c<i+1>-<n> for the n-th.
func queryRecording(times []int) []*models.Mock {
	ts := diffBase.Add(time.Millisecond)
	var mocks []*models.Mock
	for i, n := range times {
		for k := 1; k <= n; k++ {
			ts = ts.Add(100 * time.Microsecond)
			name := fmt.Sprintf("query-c%d-%d", i+1, k)
			mocks = append(mocks, diffMock(name, fmt.Sprintf("c%d", i+1), []mysql.Request{queryReq(poolTextQuery)}, ts, models.LifetimePerTest, "mocks"))
		}
	}
	return mocks
}

// Eight connections run one text query at once under exact serving, which
// the test recorded once on each: each recording answers exactly one of them.
// All eight read the first recording, the store refuses all consumes of it
// but one, and each refused query is matched again against what the store
// shows now, never answered twice by one recording, nor refused.
func TestConnectionsRunningOneQueryAtOnceEachTakeOneRecording(t *testing.T) {
	const n = 8
	times := make([]int, n)
	reqs := make([]mysql.Request, n)
	for i := range times {
		times[i] = 1
		reqs[i] = queryReq(poolTextQuery)
	}
	mocks := queryRecording(times)
	mm := stagePool(t, nil, mocks, true)
	served := map[string]int{}
	for i, a := range runAtOnce(mm, reqs) {
		if !a.held {
			t.Fatal("precondition: the command's match held no store")
		}
		if a.err != nil || !a.ok {
			t.Errorf("connection c%d: ok=%v err=%v miss=%s; want an answer", i+1, a.ok, a.err, a.miss)
			continue
		}
		served[a.payload]++
	}
	for _, mk := range mocks {
		if served[mk.Name] != 1 {
			t.Errorf("recording %s answered %d quer(ies); want 1 (served: %v)", mk.Name, served[mk.Name], served)
		}
	}
}

// A text query whose consume the store refuses is matched again, without the
// refused copy, and answered with its next recording; it is never answered by
// the refused one, nor fails.
func TestARefusedQueryConsumeIsAnsweredByItsNextRecording(t *testing.T) {
	mocks := queryRecording([]int{2})
	mm := stagePool(t, nil, mocks, true)
	ctx, dctx := poolConn("c1")
	hook := &heldHook{treeDoorsFail: true, refuse: []string{"query-c1-1"}, refuseAtMost: 3}
	resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), queryReq(poolTextQuery), &hookedStore{MockManager: mm, hook: hook}, dctx)
	if hook.MockMemDb == nil {
		t.Fatal("precondition: the command's match held no store")
	}
	if n := hook.refusals[hook.refuse[0]]; n != 1 {
		t.Fatalf("the refused copy was tried %d time(s); want once", n)
	}
	if err != nil || !ok || resp.Payload != "query-c1-2" {
		t.Fatalf("the query was answered with %v (ok=%v err=%v miss=%s); want query-c1-2", resp, ok, err, miss)
	}
}
