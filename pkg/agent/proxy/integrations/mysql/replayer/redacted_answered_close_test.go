package replayer_test

import (
	"context"
	"fmt"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

const (
	closeSecret = "sk9LIVEsecret77"
	closePh1    = "xq7Lm2Pw9Rt4Zk"
	closePh2    = "Vb3Nc8Qd1Ws6Jh"
	// closeFirst is the earlier session's recording: it redacted the key.
	closeFirst = "SELECT v FROM kv WHERE api_key = '" + closePh1 + "' AND org = 'acme'"
	// closeSecond is the later session's: it redacted the org, and kept
	// the key the call sent.
	closeSecond    = "SELECT v FROM kv WHERE api_key = '" + closeSecret + "' AND org = '" + closePh2 + "'"
	closeLiveFirst = "SELECT v FROM kv WHERE api_key = '" + closeSecret + "' AND org = 'acme'"
	closeLive      = "SELECT v FROM kv WHERE api_key = '" + closeSecret + "' AND org = 'globex'"
)

// closeMocks are the earlier and the later session's recordings, both at at.
func closeMocks(at time.Time) (own, next *models.Mock) {
	own = diffMock("kv-1", "c1", []mysql.Request{queryReq(closeFirst)}, at, models.LifetimePerTest, "mocks")
	own.Noise = []string{"^" + regexp.QuoteMeta(closePh1) + "$"}
	next = diffMock("kv-2", "c1", []mysql.Request{queryReq(closeSecond)}, at, models.LifetimePerTest, "mocks")
	next.Noise = []string{"^" + regexp.QuoteMeta(closePh2) + "$"}
	return own, next
}

func closeDctx() *wire.DecodeContext {
	return &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
}

var closeConn = context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")

// closeAnswer answers sql on db, or fails.
func closeAnswer(t *testing.T, db integrations.MockMemDb, session, sql string) {
	t.Helper()
	if ok, miss, _, err := replayer.MatchShownForTest(closeConn, zap.NewNop(), queryReq(sql), db, closeDctx()); err != nil || !ok {
		t.Fatalf("precondition: the %s session's call was not answered: ok %v, err %v, miss %s", session, ok, err, miss)
	}
}

// closeMiss is how a miss of sql on db shows the command, or fails.
func closeMiss(t *testing.T, db integrations.MockMemDb, session, sql string) (shown, miss string) {
	t.Helper()
	ok, miss, shown, err := replayer.MatchShownForTest(closeConn, zap.NewNop(), queryReq(sql), db, closeDctx())
	if err != nil || ok || miss == "" {
		t.Fatalf("precondition: the %s session's call was not a miss: ok %v, err %v, miss %q", session, ok, err, miss)
	}
	return shown, miss
}

// The places of the recordings a store's test answered are that store's:
// a later session, on another store or a new replay session on the same
// one, is redacted by the places of its own recordings, answered ones
// included, and shows as sent a value only the earlier session's
// recording redacted (its first miss, before its recordings are staged,
// shows the call as sent). A long-lived agent closes its store at each
// record session and starts a new replay session at each test set, and a
// later session can open a window of the same start as the earlier one
// (the same recordings replayed again), none at all, or serve its tests
// exactly, where no window is open either. A staging that kept the note
// (the window's end alone moved) lets the store be asked again, so a new
// replay session lets it go all the same; each of two new sessions in turn
// does.
func TestMySQLAnsweredPlacesOfAStoreRedactNoLaterSession(t *testing.T) {
	// A stage stages a session's test with own as its recording, or, with
	// none, opens its window (if any) with no recording.
	type stage func(mm *proxy.MockManager, own *models.Mock)
	s1, e1 := diffWindow(1)
	window := func(mm *proxy.MockManager, own *models.Mock) {
		if own == nil {
			mm.SetMocksWithWindow(nil, nil, s1, e1)
			return
		}
		mm.SetMocksWithWindow([]*models.Mock{own}, nil, s1, e1)
	}
	noWindow := func(mm *proxy.MockManager, own *models.Mock) {
		if own != nil {
			mm.SetFilteredMocks([]*models.Mock{own})
		}
	}
	exact := func(mm *proxy.MockManager, own *models.Mock) {
		if own != nil {
			mm.SetMocksWithWindow(nil, []*models.Mock{own}, models.BaseTime, time.Now().Add(time.Hour))
			mm.ServeExactTest("T", []*models.Mock{own})
		}
	}
	for _, tc := range []struct {
		name string
		// earlier stages the earlier session's recording and later the
		// later ones'; sameStore starts the later sessions on the earlier
		// session's store, without closing it (two of them, in turn);
		// endMoved moves the earlier window's end first.
		earlier, later      stage
		sameStore, endMoved bool
	}{
		{"the store closed, a new one's window of the same start", window, window, false, false},
		{"the store closed, no window open in either session", noWindow, noWindow, false, false},
		{"the store closed, the new one served exactly", noWindow, exact, false, false},
		{"new replay sessions on the same store, windows of the same start", window, window, true, false},
		{"the store's window end moved, then new replay sessions on it", window, window, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			own, next := closeMocks(s1.Add(time.Millisecond))
			earlier := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(earlier.Close)
			tc.earlier(earlier, own)
			closeAnswer(t, earlier, "earlier", closeLiveFirst)
			if tc.endMoved {
				earlier.SetMocksWithWindow(nil, nil, s1, e1.Add(time.Millisecond))
				if shown, _ := closeMiss(t, earlier, "earlier", closeLiveFirst); strings.Contains(shown, closeSecret) {
					t.Fatalf("precondition: the earlier session's retry, its window's end moved, shows the secret: %q", shown)
				}
			}
			sessions := 1
			if tc.sameStore {
				sessions = 2
			}
			later := earlier
			for i := 0; i < sessions; i++ {
				if tc.sameStore {
					// The agent's next replay session on the store it keeps
					// (Proxy.Mock): a new set's start-up staging, then its test.
					later.ResetForReplaySession()
					later.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now().Add(time.Hour))
				} else {
					earlier.Close()
					later = proxy.NewMockManager(nil, nil, zap.NewNop())
					t.Cleanup(later.Close)
				}
				session := fmt.Sprintf("later (%d)", i+1)
				// Before its test's recording is staged, the later session's
				// call misses with no place to redact by.
				tc.later(later, nil)
				if shown, _ := closeMiss(t, later, session, closeLiveFirst); shown != closeLiveFirst {
					t.Errorf("the %s session's first miss shows the command as %q; want it as sent, %q: no recording of its session redacted it", session, shown, closeLiveFirst)
				}
				tc.later(later, next.DeepCopy())
				closeAnswer(t, later, session, closeLive)
				if shown, _ := closeMiss(t, later, session, closeLive); strings.Contains(shown, "globex") || !strings.Contains(shown, closeSecret) {
					t.Errorf("the %s session's miss shows the command as %q; want the org its own recording redacted redacted, and the key only the earlier session's recording redacted as sent", session, shown)
				}
			}
		})
	}
}

// A store keeps the note of the places its test's recordings answered by
// once it is closed: a connection opened on it keeps it (the agent hands a
// connection the store in force when it opens, and closes that store when a
// record session takes over), and a retry there is still redacted, also
// when a later store's test has answered at the same start since.
func TestMySQLAClosedStoresConnectionsStayRedacted(t *testing.T) {
	for _, laterAnswered := range []bool{false, true} {
		t.Run(map[bool]string{false: "the retry on the closed store", true: "the retry on the closed store after a later store answered"}[laterAnswered], func(t *testing.T) {
			replayer.ForgetAnsweredForTest()
			t.Cleanup(replayer.ForgetAnsweredForTest)
			s1, e1 := diffWindow(1)
			own, next := closeMocks(s1.Add(time.Millisecond))
			earlier := proxy.NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(earlier.Close)
			earlier.SetMocksWithWindow([]*models.Mock{own}, nil, s1, e1)
			closeAnswer(t, earlier, "earlier", closeLiveFirst)
			earlier.Close()
			if laterAnswered {
				later := proxy.NewMockManager(nil, nil, zap.NewNop())
				t.Cleanup(later.Close)
				later.SetMocksWithWindow([]*models.Mock{next}, nil, s1, e1)
				closeAnswer(t, later, "later", closeLive)
			}
			if shown, miss := closeMiss(t, earlier, "earlier", closeLiveFirst); strings.Contains(shown, closeSecret) || strings.Contains(miss, closeSecret) {
				t.Errorf("the closed store's retry shows the secret its answered recording redacted: %q", shown)
			}
		})
	}
}

// untoldStore is a store that tells when it lets go of its recordings
// (integrations.Restaged) but keeps nothing (integrations.ParserKeep): it
// hides the manager's Keep.
type untoldStore struct {
	integrations.MockMemDb
	mm *proxy.MockManager
}

func (u untoldStore) WhenRestaged(slot any, release func()) { u.mm.WhenRestaged(slot, release) }

// Stores that keep nothing share one note: one store's release, at a
// staging that finds its window is not its note's, lets go of the note it
// was asked for alone, not one another store began since.
func TestMySQLAStoresReleaseLetsGoOfNoOtherStoresNote(t *testing.T) {
	replayer.ForgetAnsweredForTest()
	t.Cleanup(replayer.ForgetAnsweredForTest)
	s1, e1 := diffWindow(1)
	s2, e2 := diffWindow(2)
	own, _ := closeMocks(s1.Add(time.Millisecond))
	_, next := closeMocks(s2.Add(time.Millisecond))
	mmA := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mmA.Close)
	mmB := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mmB.Close)
	a, b := untoldStore{MockMemDb: mmA, mm: mmA}, untoldStore{MockMemDb: mmB, mm: mmB}
	if _, keeps := integrations.Keep(a, struct{}{}, func() any { return 0 }); keeps {
		t.Fatalf("precondition: the store keeps a parser's data")
	}
	mmA.SetMocksWithWindow([]*models.Mock{own}, nil, s1, e1)
	closeAnswer(t, a, "earlier", closeLiveFirst)
	mmB.SetMocksWithWindow([]*models.Mock{next}, nil, s2, e2)
	closeAnswer(t, b, "later", closeLive)
	// The earlier store's new replay session: its release finds no window.
	mmA.ResetForReplaySession()
	mmA.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now().Add(time.Hour))
	if shown, miss := closeMiss(t, b, "later", closeLive); strings.Contains(shown, "globex") || strings.Contains(miss, "globex") {
		t.Errorf("once the earlier store let its note go, the later store's miss shows the org its own answered recording redacted: %q", shown)
	}
}

// A store that keeps nothing keeps the shared note at its Close, as at a
// staging that keeps its window: the release, run at once on the closed
// store, does not ask it again (it would run at once again, without end),
// and the retry is still redacted.
func TestMySQLAStoreThatKeepsNothingKeepsTheNoteAtItsClose(t *testing.T) {
	replayer.ForgetAnsweredForTest()
	t.Cleanup(replayer.ForgetAnsweredForTest)
	mm := proxy.NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	db := untoldStore{MockMemDb: mm, mm: mm}
	s1, e1 := diffWindow(1)
	own, _ := closeMocks(s1.Add(time.Millisecond))
	mm.SetMocksWithWindow([]*models.Mock{own}, nil, s1, e1)
	closeAnswer(t, db, "earlier", closeLiveFirst)
	mm.Close()
	if shown, miss := closeMiss(t, db, "earlier", closeLiveFirst); strings.Contains(shown, closeSecret) || strings.Contains(miss, closeSecret) {
		t.Errorf("the retry on a closed store that keeps nothing shows the secret: %q", shown)
	}
}

// A closed store is not kept by the note of the places of the recordings
// its tests answered (the note is kept with the store, and holds places,
// no recording): once nothing else holds the store, it, its note and the
// recordings it staged are collected, and the heap is back within a small
// part of what they took.
func TestMySQLAnsweredPlacesKeepNoClosedStore(t *testing.T) {
	const n, pad = 400, 64 << 10
	replayer.ForgetAnsweredForTest()
	t.Cleanup(replayer.ForgetAnsweredForTest)
	heap := func() uint64 {
		var ms runtime.MemStats
		runtime.GC()
		runtime.GC()
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}
	base := heap()
	// staged is a closed store whose test answered a redacting recording
	// beside n recordings of pad bytes each, and the heap it is kept in.
	staged := func() (weak.Pointer[proxy.MockManager], uint64) {
		mm := proxy.NewMockManager(nil, nil, zap.NewNop())
		start, end := diffWindow(1)
		at := start.Add(time.Millisecond)
		own := diffMock("kv", "c1", []mysql.Request{queryReq("SELECT v FROM kv WHERE api_key = 'xq7Lm2Pw9Rt4Zk'")}, at, models.LifetimePerTest, "mocks")
		own.Noise = []string{"^xq7Lm2Pw9Rt4Zk$"}
		pool := []*models.Mock{own}
		for i := 0; i < n; i++ {
			m := diffMock(fmt.Sprintf("bulk-%d", i), "c1", []mysql.Request{queryReq(fmt.Sprintf("SELECT v FROM bulk%d WHERE id = 1", i))}, at, models.LifetimePerTest, "mocks")
			m.Spec.MySQLResponses[0].Payload = strings.Repeat("x", pad)
			pool = append(pool, m)
		}
		mm.SetMocksWithWindow(pool, nil, start, end)
		conn := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
		dctx := &wire.DecodeContext{PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{}, StmtIDToQuery: map[uint32]string{}}
		if ok, miss, _, err := replayer.MatchShownForTest(conn, zap.NewNop(), queryReq("SELECT v FROM kv WHERE api_key = 'sk9LIVEsecret77'"), mm, dctx); err != nil || !ok {
			t.Fatalf("precondition: the redacting recording did not answer: ok %v, err %v, miss %s", ok, err, miss)
		}
		mm.Close()
		wp, h := weak.Make(mm), heap()
		runtime.KeepAlive(mm)
		return wp, h
	}
	wp, held := staged()
	if held < base+n*pad/2 {
		t.Fatalf("precondition: the store's recordings take %d bytes of heap; want at least %d", int64(held)-int64(base), n*pad/2)
	}
	// The closed store's idle sweeper ends on its own once Close stopped it:
	// collect until the store is gone, at most a few times.
	after := heap()
	for i := 0; i < 20 && wp.Value() != nil; i++ {
		runtime.Gosched()
		after = heap()
	}
	if wp.Value() != nil {
		t.Errorf("the closed store is still kept once nothing but the answered places could hold it")
	}
	if after > base+n*pad/8 {
		t.Errorf("the heap is %d bytes above where it was before the store, %d with it; want within %d once it is closed", int64(after)-int64(base), int64(held)-int64(base), n*pad/8)
	}
}
