package generic

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// statedOf is each mock's stated lifetime and type, in order.
func statedOf(mocks []*models.Mock) []string {
	var out []string
	for _, m := range mocks {
		out = append(out, m.Spec.Metadata[models.MetadataLifetimeKey]+"/"+m.Spec.Metadata["type"])
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Both recorders state a connection's first exchange, its preamble, session
// and every later one per-test, and keep the type tag "config", as before they
// stated anything.
func TestTheRecordersStateAConnectionsPreambleSessionAndTheRestPerTest(t *testing.T) {
	want := []string{
		models.MetadataLifetimeSession + "/config",
		models.MetadataLifetimePerTest + "/config",
		models.MetadataLifetimePerTest + "/config",
	}
	t.Run("legacy", func(t *testing.T) {
		out := make(chan *models.Mock, 3)
		mgr := syncMock.New(zap.NewNop())
		mgr.SetOutputChannel(out)
		ctx := syncMock.NewContext(context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1"), mgr)
		clientCh, destCh := make(chan []byte), make(chan []byte)
		go func() {
			for _, x := range []string{"hello", "get a", "get b"} {
				clientCh <- []byte(x)
				destCh <- []byte("ok " + x)
			}
			close(clientCh)
			close(destCh)
		}()
		createGenericMocksAsync(ctx, zap.NewNop(), clientCh, destCh)
		var got []*models.Mock
		for len(got) < 3 {
			got = append(got, <-out)
		}
		if s := statedOf(got); !equal(s, want) {
			t.Fatalf("stated %v, want %v", s, want)
		}
	})
	t.Run("v2", func(t *testing.T) {
		clientCh := make(chan fakeconn.Chunk, 3)
		destCh := make(chan fakeconn.Chunk, 3)
		go func() {
			for i, x := range []string{"hello", "get a", "get b"} {
				at := time.Unix(int64(1000+10*i), 0)
				clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(x), ReadAt: at}
				time.Sleep(10 * time.Millisecond)
				destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok " + x), WrittenAt: at.Add(time.Second)}
				time.Sleep(10 * time.Millisecond)
			}
			close(clientCh)
			close(destCh)
		}()
		sess, mocks := newV2Session(t, clientCh, destCh)
		if err := dispatch(t, sess); err != nil {
			t.Fatal(err)
		}
		close(mocks)
		if s := statedOf(drainMocks(mocks)); !equal(s, want) {
			t.Fatalf("stated %v, want %v", s, want)
		}
	})
}

// A stated per-test exchange is a test mock (owned and judged under the
// proven switch); the preamble and an exchange recorded before the recorder
// stated anything are protocol-phase. Serving does not read the statement.
func TestAStatedPerTestExchangeIsATestMockServedAsBefore(t *testing.T) {
	mk := func(stated string) *models.Mock {
		m := &models.Mock{Kind: models.GENERIC, Spec: models.MockSpec{Metadata: map[string]string{"type": "config"}}}
		if stated != "" {
			m.Spec.Metadata[models.MetadataLifetimeKey] = stated
		}
		return m
	}
	for stated, protocol := range map[string]bool{models.MetadataLifetimePerTest: false, models.MetadataLifetimeSession: true, "": true} {
		m := mk(stated)
		if got := m.IsProtocolPhase(); got != protocol {
			t.Errorf("stated %q: IsProtocolPhase %v, want %v", stated, got, protocol)
		}
		m.DeriveLifetime()
		if m.TestModeInfo.Lifetime != models.LifetimeSession {
			t.Errorf("stated %q: derived %v, want session as before", stated, m.TestModeInfo.Lifetime)
		}
		if m.StatesLifetime() {
			t.Errorf("stated %q: StatesLifetime, which switch-off serving reads", stated)
		}
	}
}

// exactStore is a store under exact serving (the proven switch) whose open
// test owns own, with every mock staged: what keploy's mock manager tells.
type exactStore struct {
	integrations.MockMemDb
	exact  bool
	own    map[string]bool
	staged []*models.Mock
}

func (s exactStore) MarkCallRound() integrations.CallRound {
	if !s.exact {
		return integrations.CallRound{}
	}
	return integrations.NewCallRound(1, nil)
}

// HoldToRound implements integrations.RoundHolder: s never stages again, so
// it holds every call it marks as it is.
func (s exactStore) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return s, func() bool { return false }
}

func (s exactStore) FindStagedNotOwned(_ models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	if !s.exact || !call.Read() {
		return "", false
	}
	for _, m := range s.staged {
		if !s.own[m.Name] && match(m) {
			return m.Name, false
		}
	}
	return "", false
}
func (s exactStore) GetSessionMocks() ([]*models.Mock, error)         { return nil, nil }
func (s exactStore) GetPerTestMocksInWindow() ([]*models.Mock, error) { return nil, nil }

// Under exact serving, a request no mock answers that is a later exchange of
// a connection the open test does not own is named, with what likely happened
// and what to do; one the test owns, a preamble, or no recorded exchange, is
// not, nor one merely like a recorded exchange (the per-test similarity): what
// a miss names, keploy record's judging bakes into the worker scope.
func TestAMissOnAnExchangeTheTestDoesNotOwnIsNamed(t *testing.T) {
	staged := func(name, req, stated string) *models.Mock {
		m := exchange(name, req)
		m.Spec.Metadata = map[string]string{"type": "config", models.MetadataLifetimeKey: stated}
		return m
	}
	frame := make([]byte, 64)
	for i := range frame {
		frame[i] = byte(0x80 + i)
	}
	store := exactStore{
		exact: true,
		own:   map[string]bool{"mock-get-y": true},
		staged: []*models.Mock{
			staged("mock-hello", "HELLO", models.MetadataLifetimeSession),
			staged("mock-auth-x", "AUTH step2", models.MetadataLifetimePerTest),
			staged("mock-get-y", "GET y", models.MetadataLifetimePerTest),
			staged("mock-frame", util.EncodeBase64(frame), models.MetadataLifetimePerTest),
		},
	}
	// A binary request one byte off mock-frame's: within the per-test
	// similarity, and another call.
	like := append([]byte(nil), frame...)
	like[len(like)-1] ^= 0x01
	if findBinaryMatch(store.staged[3:], [][]byte{like}, 0.9) != 0 {
		t.Fatal("the fixture's near request is not within the per-test similarity of mock-frame")
	}
	for req, want := range map[string]string{"AUTH step2": "mock-auth-x", string(frame): "mock-frame", string(like): "", "GET y": "", "HELLO": "", "SET z": ""} {
		if got, _ := notOwnedExchange([][]byte{[]byte(req)}, store, store.MarkCallRound()); got != want {
			t.Errorf("%q: %q, want %q", req, got, want)
		}
	}
	store.exact = false
	if got, _ := notOwnedExchange([][]byte{[]byte("AUTH step2")}, store, store.MarkCallRound()); got != "" {
		t.Fatalf("without exact serving: %q", got)
	}
	// The miss the parser files names it, says it is not the test's, and
	// what likely happened.
	store.exact = true
	r := missReport(t, store, "AUTH step2")
	if r.ClosestMock != "mock-auth-x" || !r.NotOwned || !strings.Contains(r.NextSteps, "Likely, at replay this test made the call on a connection it opened itself") {
		t.Fatalf("the miss names %q (not owned %v) and says %q", r.ClosestMock, r.NotOwned, r.NextSteps)
	}
}

// Under the proven switch a connection whose whole lies inside one test (its
// first request in the test's span, no chunk after the test's answer) is that
// test's: the v2 recorder states its first exchange per-test too, decided by
// recorded times alone, whether the test's window ends before the recorder
// reads the connection or after the connection ended. A connection with a
// chunk after the answer, in a window that ended no test case, or opened
// outside every window, keeps its first exchange session, as its preamble.
// Every exchange goes out in recorded order, with its recorded times.
func TestV2AConnectionWhollyInsideATestIsThatTestsWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	at := func(i int) time.Time { return t0.Add(time.Duration(i+1) * time.Second) }
	for _, tc := range []struct {
		name string
		// windowAt opens a window on the manager (none when zero); answer
		// ends it a test case answered then (none when zero).
		windowAt, answer time.Time
		want             string // the first exchange's stated lifetime
	}{
		{"inside a test", t0, t0.Add(10 * time.Second), models.MetadataLifetimePerTest},
		{"a chunk after the test's answer", t0, at(0).Add(500 * time.Millisecond), models.MetadataLifetimeSession},
		{"the window ends no test case", t0, time.Time{}, models.MetadataLifetimeSession},
		{"the window opens after the connection", at(0).Add(500 * time.Millisecond), at(5), models.MetadataLifetimeSession},
		{"no window", time.Time{}, time.Time{}, models.MetadataLifetimeSession},
	} {
		for _, windowEndsFirst := range []bool{true, false} {
			name := tc.name + ", the window ends after the connection"
			if windowEndsFirst {
				name = tc.name + ", the window ends before the recorder reads the connection"
			}
			t.Run(name, func(t *testing.T) {
				mgr := syncMock.New(zap.NewNop())
				var w *syncMock.Window
				if !tc.windowAt.IsZero() {
					w = mgr.OpenWindow(tc.windowAt, nil)
				}
				end := func() {
					if w == nil {
						return
					}
					if !tc.answer.IsZero() {
						w.Yield(tc.answer)
						w.Tested(tc.windowAt, tc.answer)
					}
					w.Close()
				}
				clientCh := make(chan fakeconn.Chunk, 4)
				destCh := make(chan fakeconn.Chunk, 4)
				// Numbered as the relay numbers them: each answer after
				// its request, the second request after the first answer.
				for i, x := range []string{"publish 1", "publish 2"} {
					clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(x), ConnSeq: uint32(2*i + 1), ReadAt: at(i)}
					destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok"), ConnSeq: uint32(2*i + 2), WrittenAt: at(i).Add(time.Millisecond)}
				}
				close(clientCh)
				close(destCh)
				if windowEndsFirst {
					end()
				}
				sess, mocks := newV2Session(t, clientCh, destCh)
				sess.Ctx = syncMock.NewContext(context.Background(), mgr)
				done := make(chan error, 1)
				go func() { done <- dispatch(t, sess) }()
				if !windowEndsFirst {
					end()
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				close(mocks)
				got := drainMocks(mocks)
				if len(got) != 2 {
					t.Fatalf("emitted %d mocks, want 2", len(got))
				}
				for i, m := range got {
					if data := m.Spec.GenericRequests[0].Message[0].Data; data != []string{"publish 1", "publish 2"}[i] {
						t.Fatalf("mock %d is %q: the exchanges went out out of order", i, data)
					}
					if !m.Spec.ReqTimestampMock.Equal(at(i)) || !m.Spec.ResTimestampMock.Equal(at(i).Add(time.Millisecond)) {
						t.Fatalf("mock %d has times %v..%v, want its recorded %v..%v", i, m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock, at(i), at(i).Add(time.Millisecond))
					}
				}
				if l := got[0].Spec.Metadata[models.MetadataLifetimeKey]; l != tc.want {
					t.Fatalf("the first exchange is stated %q, want %q", l, tc.want)
				}
				if l := got[1].Spec.Metadata[models.MetadataLifetimeKey]; l != models.MetadataLifetimePerTest {
					t.Fatalf("the second exchange is stated %q, want per-test", l)
				}
			})
		}
	}
}

// A held first exchange is decided by recorded times, never by when the
// recorder's goroutines run, and only once the test's answer is known: a
// connection opened after its test was answered is never held; one with a
// chunk after its test's answer is the preamble, decided once the answer is
// known; one with none is the test's, however late the recorder sees it, and
// one that ends first waits for the answer; one with more exchanges than a
// test may wholly own is the preamble whatever its times; and a recording that
// stops first leaves it the preamble. Released, the held exchanges keep their
// order.
func TestAPreambleHoldIsDecidedByRecordedTimes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Second))) }
	first := func(at time.Time) *models.Mock {
		return &models.Mock{Name: "first", Spec: models.MockSpec{ReqTimestampMock: at, Metadata: map[string]string{}}}
	}
	// test ends w a test case answered at answer.
	test := func(w *syncMock.Window, answer time.Time) {
		w.Yield(answer)
		w.Tested(t0, answer)
		w.Close()
	}

	t.Run("opened after its test was answered", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		test(mgr.OpenWindow(t0, nil), sec(0.5))
		if holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(1.001)) != nil {
			t.Fatal("a connection opened after its test was answered is held")
		}
	})
	t.Run("a chunk after the answer", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(1.001))
		if h == nil {
			t.Fatal("a connection opened inside a test is not held")
		}
		h.add(&models.Mock{Name: "second"})
		h.saw(sec(4))
		if h.outlived() || h.decided() {
			t.Fatal("decided before the test's answer is known")
		}
		test(w, sec(3))
		<-h.changes()
		h.refresh()
		if !h.outlived() || h.wholly() {
			t.Fatal("a chunk after the test's answer did not make the first exchange the preamble")
		}
		got := h.release(false)
		if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" || got[0].Spec.Metadata[models.MetadataLifetimeKey] != models.MetadataLifetimeSession {
			t.Fatalf("released %v, want first (session) then second", got)
		}
	})
	t.Run("seen after its test ended", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		test(mgr.OpenWindow(t0, nil), sec(3))
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(2))
		if h == nil || !h.decided() || !h.wholly() || h.changes() != nil {
			t.Fatal("a connection whose chunks all came before its test's answer is not the test's")
		}
	})
	t.Run("ends before its test is answered", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(2))
		settled := make(chan struct{})
		go func() {
			h.settle(context.Background())
			close(settled)
		}()
		if !w.Keep() {
			t.Fatal("the window was given up")
		}
		// With --sync: the resolve ends the window a test case.
		mgr.ResolveKept(w, t0, sec(3), "test-1", false)
		<-settled
		if !h.wholly() {
			t.Fatal("a connection that ended before its test was answered, inside it, is not the test's")
		}
	})
	t.Run("more exchanges than a test may wholly own", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		defer w.Close()
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(1))
		for i := 1; i < maxHeldExchanges; i++ {
			h.add(&models.Mock{})
		}
		if h.outlived() {
			t.Fatalf("%d exchanges outlived their test", maxHeldExchanges)
		}
		h.add(&models.Mock{})
		if !h.outlived() {
			t.Fatalf("%d exchanges did not make the first the preamble", maxHeldExchanges+1)
		}
	})
	t.Run("the recording stops first", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		defer w.Close()
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(2))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h.settle(ctx)
		if h.wholly() {
			t.Fatal("a connection undecided as the recording stopped is the test's")
		}
	})
}

// While the v2 recorder holds a connection's exchanges, the session's pending
// work is cleared as it holds each: it has consumed them, and work left pending
// through an idle spell (a connection left open after its test) is taken for
// a hung parser, which the supervisor aborts.
func TestV2HoldingAnExchangeClearsThePendingWork(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := syncMock.New(zap.NewNop())
	w := mgr.OpenWindow(t0, nil)
	clientCh := make(chan fakeconn.Chunk, 2)
	destCh := make(chan fakeconn.Chunk, 2)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("publish"), ReadAt: t0.Add(time.Second)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok"), WrittenAt: t0.Add(2 * time.Second)}
	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.Ctx = syncMock.NewContext(context.Background(), mgr)
	cleared := make(chan struct{}, 4)
	sess.OnPendingCleared = func() { cleared <- struct{}{} }
	done := make(chan error, 1)
	go func() { done <- dispatch(t, sess) }()
	// The exchange is held while the window is open (the connection open
	// too): the pending work must be cleared without its emit. Guarded so
	// the test fails rather than hangs when it is not.
	select {
	case <-cleared:
	case <-time.After(30 * time.Second):
		t.Fatal("holding the exchange left the session's pending work armed")
	}
	select {
	case m := <-mocks:
		t.Fatalf("the exchange was emitted (%s) while its test may own the connection", m.Spec.Metadata[models.MetadataLifetimeKey])
	default:
	}
	close(clientCh)
	close(destCh)
	w.Yield(t0.Add(3 * time.Second))
	w.Tested(t0, t0.Add(3*time.Second))
	w.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m := <-mocks; m.Spec.Metadata[models.MetadataLifetimeKey] != models.MetadataLifetimePerTest {
		t.Fatalf("stated %q, want per-test", m.Spec.Metadata[models.MetadataLifetimeKey])
	}
}

// neverBehind is a capture watermark whose parsers are never behind, so what
// a recording's stop finds pending is the manager's own.
type neverBehind struct{}

func (neverBehind) Settled(_, _ string, _, _ time.Time) bool { return true }
func (neverBehind) PendingBefore(time.Time) bool             { return false }

// A connection wholly inside its test so far, left open after it, is held
// until it is used again or closes. As the recording's stop begins it is
// decided with what is known, the test's then, and handed on; until the
// manager has it, the stop finds it pending, so its streams do not end
// without it.
func TestV2AHeldIdleConnectionIsHandedOnAsTheStopBegins(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	stop := t0.Add(10 * time.Second)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetWatermark(neverBehind{})
	w := mgr.OpenWindow(t0, nil)
	w.Yield(t0.Add(3 * time.Second))
	w.Tested(t0, t0.Add(3*time.Second))
	w.Close()
	clientCh := make(chan fakeconn.Chunk, 2)
	destCh := make(chan fakeconn.Chunk, 2)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("publish"), ReadAt: t0.Add(time.Second)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok"), WrittenAt: t0.Add(time.Second + time.Millisecond)}
	sess, _ := newV2Session(t, clientCh, destCh)
	sess.Ctx = syncMock.NewContext(context.Background(), mgr)
	// Unbuffered and not read yet: the hand-on waits in its emit, the
	// exchange still the parser's.
	mocks := make(chan *models.Mock)
	sess.Mocks = mocks
	held := make(chan struct{}, 4)
	sess.OnPendingCleared = func() { held <- struct{}{} }
	done := make(chan error, 1)
	go func() { done <- dispatch(t, sess) }()
	select {
	case <-held:
	case <-time.After(30 * time.Second):
		t.Fatal("the exchange was not held")
	}
	mgr.BacklogBefore(stop) // the stop begins
	if st := mgr.BacklogBefore(stop); !st.Pending {
		t.Fatalf("the stop found nothing pending while the parser still had the exchange: %+v", st)
	}
	select {
	case m := <-mocks:
		if l := m.Spec.Metadata[models.MetadataLifetimeKey]; l != models.MetadataLifetimePerTest {
			t.Fatalf("stated %q at the stop, want per-test", l)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the held exchange was not handed on as the stop began")
	}
	close(clientCh)
	close(destCh)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := mgr.BacklogBefore(stop); st.Pending {
		t.Fatalf("handed on, the stop still finds it pending: %+v", st)
	}
}

// A v2 recorder that leaves while it holds exchanges (a panic, which the
// supervisor recovers) reports them left out, as any exchange it could not
// record, and takes its hold off the stop's count: the stop does not wait on
// it forever, and the loss is not silent.
func TestV2ARecorderThatLeavesHoldingReportsItsExchangesLeftOut(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := syncMock.New(zap.NewNop())
	mgr.SetWatermark(neverBehind{})
	w := mgr.OpenWindow(t0, nil)
	defer w.Close()
	clientCh := make(chan fakeconn.Chunk, 2)
	destCh := make(chan fakeconn.Chunk, 2)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("publish"), ReadAt: t0.Add(time.Second)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok"), WrittenAt: t0.Add(time.Second + time.Millisecond)}
	sess, _ := newV2Session(t, clientCh, destCh)
	sess.Ctx = syncMock.NewContext(context.Background(), mgr)
	sess.Mgr = mgr
	// The first clear is the hold's (Session.MockHeld): the parser panics
	// there, holding the exchange.
	clears := 0
	sess.OnPendingCleared = func() {
		if clears++; clears == 1 {
			panic("the parser fails while it holds an exchange")
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the parser did not fail")
			}
		}()
		_ = dispatch(t, sess)
	}()
	if n := mgr.MocksLeftOut(); n != 1 {
		t.Fatalf("%d exchanges reported left out, want the 1 held", n)
	}
	if st := mgr.BacklogBefore(t0.Add(10 * time.Second)); st.Pending {
		t.Fatalf("the stop still waits on the hold of a parser that left: %+v", st)
	}
}

// With the switch off the recorder holds nothing: each exchange is emitted as
// it completes, the first session whatever the windows, as before.
func TestV2WithTheSwitchOffTheFirstExchangeIsEmittedAtOnce(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	mgr := syncMock.New(zap.NewNop())
	w := mgr.OpenWindow(time.Unix(4000, 0), nil)
	t.Cleanup(w.Close)
	clientCh := make(chan fakeconn.Chunk, 2)
	destCh := make(chan fakeconn.Chunk, 2)
	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.Ctx = syncMock.NewContext(context.Background(), mgr)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("hello"), ReadAt: time.Unix(5000, 0)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("hi"), WrittenAt: time.Unix(5001, 0)}
	go func() {
		// The first exchange is out before the connection ends.
		m := <-mocks
		if m.Spec.Metadata[models.MetadataLifetimeKey] != models.MetadataLifetimeSession {
			t.Errorf("stated %q, want session", m.Spec.Metadata[models.MetadataLifetimeKey])
		}
		close(clientCh)
		close(destCh)
	}()
	if err := dispatch(t, sess); err != nil {
		t.Fatal(err)
	}
}

// The legacy recorder holds a connection's first exchange the same way under
// the proven switch: inside a test it is stated per-test with the rest, the
// connection's end waiting for the test's answer, and owned by its test on the
// late path; with no window it is the session preamble.
func TestLegacyAConnectionWhollyInsideATestIsThatTestsWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	for _, inWindow := range []bool{true, false} {
		out := make(chan *models.Mock, 4)
		mgr := syncMock.New(zap.NewNop())
		mgr.SetOutputChannel(out)
		start := time.Now().Add(-time.Second)
		answer := start.Add(10 * time.Millisecond)
		var w *syncMock.Window
		if inWindow {
			w = mgr.OpenWindow(start, nil)
		}
		// Every chunk's recorded time, before the test's answer.
		ctx := context.WithValue(context.Background(), models.ClientConnectionIDKey, "c1")
		ctx = context.WithValue(ctx, models.CapturedReqTimeKey, func() time.Time { return start.Add(time.Millisecond) })
		ctx = context.WithValue(ctx, models.CapturedRespTimeKey, func() time.Time { return start.Add(2 * time.Millisecond) })
		ctx = syncMock.NewContext(ctx, mgr)
		clientCh, destCh := make(chan []byte), make(chan []byte)
		go func() {
			for _, x := range []string{"publish 1", "publish 2"} {
				clientCh <- []byte(x)
				destCh <- []byte("ok")
			}
			close(clientCh)
			close(destCh)
		}()
		done := make(chan struct{})
		go func() {
			createGenericMocksAsync(ctx, zap.NewNop(), clientCh, destCh)
			close(done)
		}()
		if inWindow {
			// A --sync recording's resolve ends the window a test case,
			// whenever the recorder reads the connection.
			if !w.Keep() {
				t.Fatal("the window was given up")
			}
			mgr.ResolveKept(w, start, answer, "test-1", false)
		}
		<-done
		if !inWindow {
			mgr.ResolveRange(start, answer, "test-1", true, false)
		}
		mgr.FlushOwnedWindows()
		close(out)
		got := map[string]string{}
		for m := range out {
			got[m.Spec.GenericRequests[0].Message[0].Data] = m.Spec.Metadata[models.MetadataLifetimeKey]
		}
		want := models.MetadataLifetimeSession
		if inWindow {
			want = models.MetadataLifetimePerTest
		}
		if got["publish 1"] != want || got["publish 2"] != models.MetadataLifetimePerTest {
			t.Fatalf("in a window %v: stated %v, want the first %s and the second per-test", inWindow, got, want)
		}
	}
}
