package generic

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
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
	// mock-frame holds frame as the recorder writes a binary request
	// (encodePayload): its base64, typed binary.
	frameMock := staged("mock-frame", "", models.MetadataLifetimePerTest)
	frameMock.Spec.GenericRequests[0] = encodePayload(frame, models.FromClient)
	store := exactStore{
		exact: true,
		own:   map[string]bool{"mock-get-y": true},
		staged: []*models.Mock{
			staged("mock-hello", "HELLO", models.MetadataLifetimeSession),
			staged("mock-auth-x", "AUTH step2", models.MetadataLifetimePerTest),
			staged("mock-get-y", "GET y", models.MetadataLifetimePerTest),
			frameMock,
			staged("mock-ping", "PING", models.MetadataLifetimePerTest),
		},
	}
	// A binary request one byte off mock-frame's: within the per-test
	// similarity, and another call.
	like := append([]byte(nil), frame...)
	like[len(like)-1] ^= 0x01
	if findBinaryMatch(store.staged[3:], [][]byte{like}, 0.9) != 0 {
		t.Fatal("the fixture's near request is not within the per-test similarity of mock-frame")
	}
	// frame's base64 sent as text is other bytes than frame, and 3c 83 46
	// other bytes than the text PING, its base64: neither is named.
	for req, want := range map[string]string{"AUTH step2": "mock-auth-x", string(frame): "mock-frame", string(like): "", util.EncodeBase64(frame): "", "PING": "mock-ping", "\x3c\x83\x46": "", "GET y": "", "HELLO": "", "SET z": ""} {
		if got, _ := notOwnedExchange([][]byte{[]byte(req)}, store, store.MarkCallRound()); got != want {
			t.Errorf("%q: %q, want %q", req, got, want)
		}
	}
	// An exchange whose token buffer the obfuscator redacted is named only
	// for its recorded bytes, as the pool matches it: not even the token the
	// recording redacted is named (fail-closed), so neither is another.
	tok := staged("mock-token", "", models.MetadataLifetimePerTest)
	tok.Spec.GenericRequests[0].Message[0].Data = redactedToken
	tok.Noise = []string{"^" + redactedToken + "$"}
	withTok := store
	withTok.staged = []*models.Mock{tok}
	if got, _ := notOwnedExchange([][]byte{[]byte(recordedToken)}, withTok, withTok.MarkCallRound()); got != "" {
		t.Errorf("the token the recording redacted, not its recorded bytes, is named %q", got)
	}
	if got, _ := notOwnedExchange([][]byte{[]byte(redactedToken)}, withTok, withTok.MarkCallRound()); got != "mock-token" {
		t.Errorf("the redacted exchange's own bytes: %q, want mock-token", got)
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
// Every exchange goes out in recorded order, with its recorded times. A test
// keploy mock record knows by its harness's begin and end (a test scope: it
// opens no window) is decided as a window's test case is.
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
		for _, scope := range []bool{false, true} {
			for _, windowEndsFirst := range []bool{true, false} {
				name := tc.name + ", the window ends after the connection"
				if windowEndsFirst {
					name = tc.name + ", the window ends before the recorder reads the connection"
				}
				if scope {
					name = "a test scope: " + name
				}
				t.Run(name, func(t *testing.T) {
					mgr := syncMock.New(zap.NewNop())
					var w *syncMock.Window
					var sc *syncMock.TestScope
					if !tc.windowAt.IsZero() {
						if scope {
							sc = mgr.BeginTestScope(tc.windowAt, 0)
						} else {
							w = mgr.OpenWindow(tc.windowAt, nil)
						}
					}
					end := func() {
						if sc != nil {
							if !tc.answer.IsZero() {
								sc.End(tc.answer)
							} else {
								sc.Abandon()
							}
							return
						}
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
					if windowEndsFirst {
						if err := dispatch(t, sess); err != nil {
							t.Fatal(err)
						}
					} else if err := dispatchEndingAfter(t, sess, 2, end); err != nil {
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
}

// A connection's chunks are timed by when the relay read them, never by when
// its write returned. Under load the relay's write of a destination's answer
// returned after the client had read it, ended its test and the next test had
// begun (seen at PLG_LOAD=8: read 1.0 ms before the test's end, written 4.7 ms
// after it): timed by that return, a connection wholly inside its test read
// as outliving it, its only exchange went to the session pool, and the test
// passed at replay with its call skipped. An answer the relay read after the
// test's end still makes the connection outlive it; one with no read time
// does not.
func TestV2AnAnswerWrittenOnAfterItsTestEndedIsStillThatTestsWhole(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	for _, tc := range []struct {
		name               string
		answerRead, answer time.Time // the destination's answer read; the test's end
		want               string
	}{
		{"the answer read inside the test, its write returned after the next test began", ms(19.2), ms(20.8), models.MetadataLifetimePerTest},
		{"the answer read after the test ended", ms(21.5), ms(20.8), models.MetadataLifetimeSession},
		// No read time shows nothing: its write's return is no time the
		// connection was used.
		{"the answer with no read time, its write returned after the next test began", time.Time{}, ms(20.8), models.MetadataLifetimePerTest},
	} {
		for _, scope := range []bool{true, false} {
			for _, endsFirst := range []bool{true, false} {
				name := tc.name
				if scope {
					name = "a test scope: " + name
				}
				if endsFirst {
					name += ", the test ends before the recorder reads the connection"
				}
				t.Run(name, func(t *testing.T) {
					mgr := syncMock.New(zap.NewNop())
					var sc *syncMock.TestScope
					var w *syncMock.Window
					if scope {
						sc = mgr.BeginTestScope(t0, 0)
					} else {
						w = mgr.OpenWindow(t0, nil)
					}
					end := func() {
						if sc != nil {
							sc.End(tc.answer)
						} else {
							w.Yield(tc.answer)
							w.Tested(t0, tc.answer)
							w.Close()
						}
						// The next test begins before the relay's write returns.
						if scope {
							mgr.BeginTestScope(ms(21), 0)
						} else {
							mgr.OpenWindow(ms(21), nil)
						}
					}
					clientCh := make(chan fakeconn.Chunk, 1)
					destCh := make(chan fakeconn.Chunk, 1)
					clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("QUOTE item=book"), ConnSeq: 1, ReadAt: ms(19), WrittenAt: ms(19.05)}
					destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("CENTS 1250"), ConnSeq: 2, ReadAt: tc.answerRead, WrittenAt: ms(25.5)}
					close(clientCh)
					close(destCh)
					if endsFirst {
						end()
					}
					sess, mocks := newV2Session(t, clientCh, destCh)
					sess.Ctx = syncMock.NewContext(context.Background(), mgr)
					if endsFirst {
						if err := dispatch(t, sess); err != nil {
							t.Fatal(err)
						}
					} else if err := dispatchEndingAfter(t, sess, 1, end); err != nil {
						t.Fatal(err)
					}
					close(mocks)
					got := drainMocks(mocks)
					if len(got) != 1 {
						t.Fatalf("emitted %d mocks, want 1", len(got))
					}
					if !got[0].Spec.ResTimestampMock.Equal(ms(25.5)) {
						t.Fatalf("the exchange's answer is stamped %v, want its write's return %v: the mock's own times are unchanged", got[0].Spec.ResTimestampMock, ms(25.5))
					}
					if l := got[0].Spec.Metadata[models.MetadataLifetimeKey]; l != tc.want {
						t.Fatalf("the connection's only exchange is stated %q, want %q", l, tc.want)
					}
				})
			}
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
		if holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(1.001), 0) != nil {
			t.Fatal("a connection opened after its test was answered is held")
		}
	})
	t.Run("a chunk after the answer", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(1.001), 0)
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
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(2), 0)
		if h == nil || !h.decided() || !h.wholly() || h.changes() != nil {
			t.Fatal("a connection whose chunks all came before its test's answer is not the test's")
		}
	})
	t.Run("ends before its test is answered", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(2), 0)
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
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(1), 0)
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
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(sec(1)), sec(1), sec(2), 0)
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
		// An open window holds its request, and still it is not marked:
		// with the switch off the metadata is as it was.
		if v, ok := m.Spec.Metadata[models.MetadataLifetimeUndecidedKey]; ok {
			t.Errorf("marked %q with the switch off", v)
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

// markOf is m's lifetimeUndecided mark: its reason and test start, or "" when
// it has none.
func markOf(t *testing.T, m *models.Mock) string {
	t.Helper()
	v, has := m.Spec.Metadata[models.MetadataLifetimeUndecidedKey]
	u, ok := m.LifetimeUndecided()
	if has != ok {
		t.Fatalf("the mark %q does not read back", v)
	}
	if !ok {
		return ""
	}
	return u.Why + "@" + startsOf(u.TestStarts)
}

// A connection's first exchange that the recorder puts in the session pool
// only because it could not tell that the connection lay wholly inside the
// test whose window held its request is marked, with why and that test (its
// window's start): it may be that test's own call, and a test that owns no
// call and held one is not to be judged on its outcome (keploy mock record's
// judging reads the mark; keploy record's does not). Two paths go by recorded times: the client wrote
// on after the test's answer (a trailing write the relay read late, or any
// write after the answer), and a later test reused the connection. A
// connection wholly inside its test, or whose first request no test's window
// holds, carries no mark.
func TestV2APreambleStatedForWantOfADecisionNamesItsTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	marked := models.LifetimeUndecidedReadAfterAnswer + "@" + t0.UTC().Format(time.RFC3339Nano)
	type chunk struct {
		fromClient bool
		data       string
		readAt     time.Time
	}
	for _, tc := range []struct {
		name string
		// windowAt is when the test's window (or scope) opens; tested says
		// whether it ends a test case answered at ms(20.8).
		windowAt time.Time
		tested   bool
		// more are the chunks after the first exchange's; exchanges is how
		// many exchanges the connection has, the first included.
		more      []chunk
		exchanges int
		want      []string // each exchange's stated lifetime, then its mark
	}{
		{"the client writes on after the test's answer", t0, true,
			[]chunk{{true, "BYE", ms(21.5)}}, 1,
			[]string{models.MetadataLifetimeSession, marked}},
		{"a later test reuses the connection", t0, true,
			[]chunk{{true, "QUOTE item=pen", ms(22)}, {false, "CENTS 80", ms(22.2)}}, 2,
			[]string{models.MetadataLifetimeSession, marked, models.MetadataLifetimePerTest, ""}},
		{"wholly inside its test", t0, true,
			nil, 1,
			[]string{models.MetadataLifetimePerTest, ""}},
		{"its test's window ends no test case", t0, false,
			[]chunk{{true, "BYE", ms(21.5)}}, 1,
			[]string{models.MetadataLifetimeSession, ""}},
		{"no test's window holds its request", ms(19.5), true,
			[]chunk{{true, "BYE", ms(21.5)}}, 1,
			[]string{models.MetadataLifetimeSession, ""}},
	} {
		for _, scope := range []bool{false, true} {
			for _, endsFirst := range []bool{true, false} {
				name := tc.name
				if scope {
					name = "a test scope: " + name
				}
				if endsFirst {
					name += ", the test ends before the recorder reads the connection"
				}
				t.Run(name, func(t *testing.T) {
					mgr := syncMock.New(zap.NewNop())
					var sc *syncMock.TestScope
					var w *syncMock.Window
					if scope {
						sc = mgr.BeginTestScope(tc.windowAt, 0)
					} else {
						w = mgr.OpenWindow(tc.windowAt, nil)
					}
					end := func() {
						switch {
						case sc != nil && tc.tested:
							sc.End(ms(20.8))
						case sc != nil:
							sc.Abandon()
						default:
							if tc.tested {
								w.Yield(ms(20.8))
								w.Tested(tc.windowAt, ms(20.8))
							}
							w.Close()
						}
						// The next test begins.
						if scope {
							mgr.BeginTestScope(ms(21), 0)
						} else {
							mgr.OpenWindow(ms(21), nil)
						}
					}
					clientCh := make(chan fakeconn.Chunk, 4)
					destCh := make(chan fakeconn.Chunk, 4)
					clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("QUOTE item=book"), ConnSeq: 1, ReadAt: ms(19)}
					destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("CENTS 1250"), ConnSeq: 2, ReadAt: ms(19.2)}
					for i, c := range tc.more {
						if c.fromClient {
							clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte(c.data), ConnSeq: uint32(i + 3), ReadAt: c.readAt}
						} else {
							destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte(c.data), ConnSeq: uint32(i + 3), ReadAt: c.readAt}
						}
					}
					close(clientCh)
					close(destCh)
					if endsFirst {
						end()
					}
					sess, mocks := newV2Session(t, clientCh, destCh)
					sess.Ctx = syncMock.NewContext(context.Background(), mgr)
					if endsFirst {
						if err := dispatch(t, sess); err != nil {
							t.Fatal(err)
						}
					} else if err := dispatchEndingAfter(t, sess, tc.exchanges, end); err != nil {
						t.Fatal(err)
					}
					close(mocks)
					var got []string
					for _, m := range drainMocks(mocks) {
						got = append(got, m.Spec.Metadata[models.MetadataLifetimeKey], markOf(t, m))
					}
					if !equal(got, tc.want) {
						t.Fatalf("the exchanges are stated and marked %q, want %q", got, tc.want)
					}
				})
			}
		}
	}
}

// A connection whose first request an open test's window holds, and whose
// test has not been answered when the recording's stop begins, is handed on
// as the session preamble, marked as stated for want of a decision, with
// that test's window start; one whose test was answered first is the test's,
// unmarked.
func TestV2APreambleUndecidedAtTheStopNamesItsTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	stop := t0.Add(10 * time.Second)
	for _, tc := range []struct {
		name     string
		scope    bool
		answered bool
		want     []string
	}{
		{"its window still open", false, false, []string{models.MetadataLifetimeSession, models.LifetimeUndecidedStoppedFirst + "@" + t0.UTC().Format(time.RFC3339Nano)}},
		{"its test scope still open", true, false, []string{models.MetadataLifetimeSession, models.LifetimeUndecidedStoppedFirst + "@" + t0.UTC().Format(time.RFC3339Nano)}},
		{"its test answered first", false, true, []string{models.MetadataLifetimePerTest, ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := syncMock.New(zap.NewNop())
			mgr.SetWatermark(neverBehind{})
			if tc.scope {
				sc := mgr.BeginTestScope(t0, 0)
				defer sc.Abandon()
			} else {
				w := mgr.OpenWindow(t0, nil)
				if tc.answered {
					w.Yield(t0.Add(3 * time.Second))
					w.Tested(t0, t0.Add(3*time.Second))
					w.Close()
				}
				defer w.Close()
			}
			clientCh := make(chan fakeconn.Chunk, 2)
			destCh := make(chan fakeconn.Chunk, 2)
			clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("publish"), ReadAt: t0.Add(time.Second)}
			destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok"), ReadAt: t0.Add(time.Second + time.Millisecond)}
			sess, _ := newV2Session(t, clientCh, destCh)
			sess.Ctx = syncMock.NewContext(context.Background(), mgr)
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
			select {
			case m := <-mocks:
				if got := []string{m.Spec.Metadata[models.MetadataLifetimeKey], markOf(t, m)}; !equal(got, tc.want) {
					t.Fatalf("handed on at the stop stated and marked %q, want %q", got, tc.want)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the held exchange was not handed on as the stop began")
			}
			close(clientCh)
			close(destCh)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A connection with more exchanges than the recorder holds is the preamble
// whatever its times, marked as stated for want of a decision; exchanges a
// recorder that leaves holding hands on as left out are not marked when
// nothing showed the connection outliving its test.
func TestAPreambleHoldMarksOnlyAnUndecidedPreamble(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	sec := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Second))) }
	first := func() *models.Mock {
		return &models.Mock{Name: "first", Spec: models.MockSpec{ReqTimestampMock: sec(1), Metadata: map[string]string{}}}
	}
	t.Run("more exchanges than the recorder holds", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		defer w.Close()
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(), sec(1), sec(1), 0)
		for i := 0; i < maxHeldExchanges; i++ {
			h.add(&models.Mock{})
		}
		got := h.release(false)[0]
		want := models.LifetimeUndecidedManyExchanges + "@" + t0.UTC().Format(time.RFC3339Nano)
		if m := markOf(t, got); m != want {
			t.Fatalf("marked %q, want %q", m, want)
		}
	})
	t.Run("its window, open as it held, ends no test case", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(), sec(1), sec(2), 0)
		if h == nil {
			t.Fatal("a connection opened inside an open window is not held")
		}
		w.Close()
		<-h.changes()
		h.refresh()
		if !h.outlived() {
			t.Fatal("a connection whose window ended no test case is not the preamble")
		}
		got := h.release(false)[0]
		if l := got.Spec.Metadata[models.MetadataLifetimeKey]; l != models.MetadataLifetimeSession {
			t.Fatalf("stated %q, want session", l)
		}
		if m := markOf(t, got); m != "" {
			t.Fatalf("a preamble no test's window holds any more is marked %q", m)
		}
	})
	t.Run("released wholly inside its answered test", func(t *testing.T) {
		mgr := syncMock.New(zap.NewNop())
		w := mgr.OpenWindow(t0, nil)
		w.Yield(sec(3))
		w.Tested(t0, sec(3))
		w.Close()
		h := holdPreamble(syncMock.NewContext(context.Background(), mgr), first(), sec(1), sec(2), 0)
		if m := markOf(t, h.release(false)[0]); m != "" {
			t.Fatalf("a connection nothing showed outliving its answered test is marked %q", m)
		}
	})
}

// The mark reads back as written, to the nanosecond, with every test it names
// in order, and a value that is not one reads as none.
func TestTheLifetimeUndecidedMarkReadsBack(t *testing.T) {
	at := time.Date(2026, 10, 30, 0, 6, 11, 123456789, time.FixedZone("IST", 19800))
	other := at.Add(1500 * time.Microsecond)
	for _, starts := range [][]time.Time{{at}, {at, other}} {
		m := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{
			models.MetadataLifetimeUndecidedKey: models.MockLifetimeUndecided{Why: models.LifetimeUndecidedManyTests, TestStarts: starts}.String(),
		}}}
		u, ok := m.LifetimeUndecided()
		if !ok || u.Why != models.LifetimeUndecidedManyTests || len(u.TestStarts) != len(starts) {
			t.Fatalf("read back %+v (ok %v), want %s at %v", u, ok, models.LifetimeUndecidedManyTests, starts)
		}
		for i := range starts {
			if !u.TestStarts[i].Equal(starts[i]) {
				t.Fatalf("read back %+v, want %s at %v", u, models.LifetimeUndecidedManyTests, starts)
			}
		}
	}
	m := &models.Mock{Spec: models.MockSpec{Metadata: map[string]string{}}}
	for _, v := range []string{"", "stoppedFirst", "stoppedFirst ", " 2026-10-30T00:06:11Z", "stoppedFirst yesterday", "manyTests 2026-10-30T00:06:11Z yesterday", "manyTests 2026-10-30T00:06:11Z  2026-10-30T00:06:12Z"} {
		m.Spec.Metadata[models.MetadataLifetimeUndecidedKey] = v
		if u, ok := m.LifetimeUndecided(); ok {
			t.Fatalf("%q read as a mark: %+v", v, u)
		}
	}
	if _, ok := (&models.Mock{}).LifetimeUndecided(); ok {
		t.Fatal("a mock with no metadata read as marked")
	}
}

// dispatchEndingAfter records sess, and calls end, the test's end, once the
// recorder has consumed the connection, never by how long it ran: it told the
// session of each of the connection's exchanges it held (Session.MockHeld)
// and, holding them as the connection ended, told it once more before it
// settles them (preambleHold.settle), so the end lands while the recorder
// waits on it there. A recorder that holds nothing returns first, and end is
// called after it. The connection's chunks are all sent and its streams
// closed before the call.
func dispatchEndingAfter(t *testing.T, sess *supervisor.Session, exchanges int, end func()) error {
	t.Helper()
	told := make(chan struct{}, 2*maxHeldExchanges)
	sess.OnPendingCleared = func() { told <- struct{}{} }
	done := make(chan error, 1)
	go func() { done <- dispatch(t, sess) }()
	for range exchanges + 1 {
		select {
		case <-told:
		case err := <-done:
			end()
			return err
		}
	}
	end()
	return <-done
}

// startsOf is a mark's tests' starts as markOf shows them: UTC, RFC 3339 with
// nanoseconds, joined by "+".
func startsOf(starts []time.Time) string {
	var out []string
	for _, s := range starts {
		out = append(out, s.UTC().Format(time.RFC3339Nano))
	}
	return strings.Join(out, "+")
}

// When more than one test's window holds a connection's first request
// (parallel workers' tests, nested scopes, concurrent requests), no time tells
// which test made the call: the connection is the preamble, marked with every
// one of them, so none of them that owns no call is judged on it. Only the
// calling process tells: a connection whose process is, or descends from, the
// worker of exactly one of them is that test's alone, and judged as any
// other. The probe: test A (another worker's) begins at t0 and ends at 19.1
// ms; test B begins at 1 ms, makes the call at 19 ms, its answer is read at
// 19.2 ms, and ends at 30 ms. Picking the earliest test would mark A alone
// and leave B, which owns no call, judged: a pass with its call skipped.
func TestV2AConnectionInOverlappingTestsIsMarkedWithEveryOne(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	self := uint32(os.Getpid())
	const other = 4000000000 // no such process
	both := models.LifetimeUndecidedManyTests + "@" + startsOf([]time.Time{t0, ms(1)})
	for _, tc := range []struct {
		name         string
		windows      bool   // windows, not test scopes (pids are scopes')
		pidA, pidB   uint32 // the workers that began A and B
		src          uint32 // the connection's process
		lifetime, mk string
	}{
		{"two workers' tests, the process known to neither", false, other, other + 1, self, models.MetadataLifetimeSession, both},
		{"two workers' tests, no process known", false, other, other + 1, 0, models.MetadataLifetimeSession, both},
		{"two tests reported with no worker", false, 0, 0, self, models.MetadataLifetimeSession, both},
		{"one worker's nested tests", false, self, self, self, models.MetadataLifetimeSession, both},
		{"two windows", true, 0, 0, self, models.MetadataLifetimeSession, both},
		{"the process is B's worker", false, other, self, self, models.MetadataLifetimePerTest, ""},
		{"the process is A's worker", false, self, other, self, models.MetadataLifetimeSession, models.LifetimeUndecidedReadAfterAnswer + "@" + startsOf([]time.Time{t0})},
	} {
		for _, endsFirst := range []bool{true, false} {
			name := tc.name
			if endsFirst {
				name += ", the tests end before the recorder reads the connection"
			}
			t.Run(name, func(t *testing.T) {
				mgr := syncMock.New(zap.NewNop())
				var end func()
				if tc.windows {
					a, b := mgr.OpenWindow(t0, nil), mgr.OpenWindow(ms(1), nil)
					end = func() {
						a.Yield(ms(19.1))
						a.Tested(t0, ms(19.1))
						a.Close()
						b.Yield(ms(30))
						b.Tested(ms(1), ms(30))
						b.Close()
					}
				} else {
					a, b := mgr.BeginTestScope(t0, tc.pidA), mgr.BeginTestScope(ms(1), tc.pidB)
					end = func() {
						a.End(ms(19.1))
						b.End(ms(30))
					}
				}
				clientCh := make(chan fakeconn.Chunk, 1)
				destCh := make(chan fakeconn.Chunk, 1)
				clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("QUOTE item=book"), ConnSeq: 1, ReadAt: ms(19)}
				destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("CENTS 1250"), ConnSeq: 2, ReadAt: ms(19.2)}
				close(clientCh)
				close(destCh)
				sess, mocks := newV2Session(t, clientCh, destCh)
				sess.Ctx = syncMock.NewContext(context.Background(), mgr)
				sess.Opts.SrcPid = tc.src
				if endsFirst {
					end()
					if err := dispatch(t, sess); err != nil {
						t.Fatal(err)
					}
				} else if err := dispatchEndingAfter(t, sess, 1, end); err != nil {
					t.Fatal(err)
				}
				close(mocks)
				got := drainMocks(mocks)
				if len(got) != 1 {
					t.Fatalf("emitted %d mocks, want 1", len(got))
				}
				if g := []string{got[0].Spec.Metadata[models.MetadataLifetimeKey], markOf(t, got[0])}; !equal(g, []string{tc.lifetime, tc.mk}) {
					t.Fatalf("the connection's only exchange is stated and marked %q, want %q", g, []string{tc.lifetime, tc.mk})
				}
			})
		}
	}
}

// A held connection whose test is still open when the parser's context ends
// (the session aborted while the connection lives) is handed on as the
// preamble, marked as stopped first with its test's start: never as the
// test's own, which the recorder never knew it was.
func TestV2AHoldTheParserEndsOnIsThePreambleMarked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(5000, 0)
	mgr := syncMock.New(zap.NewNop())
	sc := mgr.BeginTestScope(t0, 0)
	defer sc.Abandon()
	clientCh := make(chan fakeconn.Chunk, 1)
	destCh := make(chan fakeconn.Chunk, 1)
	clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("publish"), ConnSeq: 1, ReadAt: t0.Add(time.Second)}
	destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("ok"), ConnSeq: 2, ReadAt: t0.Add(time.Second + time.Millisecond)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, mocks := newV2Session(t, clientCh, destCh)
	sess.Ctx = syncMock.NewContext(ctx, mgr)
	held := make(chan struct{}, 4)
	sess.OnPendingCleared = func() { held <- struct{}{} }
	done := make(chan error, 1)
	go func() { done <- dispatch(t, sess) }()
	<-held // the first exchange is held; the streams stay open
	cancel()
	m := <-mocks
	if got, want := []string{m.Spec.Metadata[models.MetadataLifetimeKey], markOf(t, m)}, []string{models.MetadataLifetimeSession, models.LifetimeUndecidedStoppedFirst + "@" + startsOf([]time.Time{t0})}; !equal(got, want) {
		t.Fatalf("handed on as the parser ended stated and marked %q, want %q", got, want)
	}
	close(clientCh)
	close(destCh)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A connection that ends while its test is still open is decided when the
// test's end, or the recording's stop, or the parser's end comes, as the
// recorder settles it: the test's whole when none of its chunks was read
// after the test's answer; else the preamble, marked. Under load this is the
// usual order: the test's end is delivered after the client's last write was
// read and its connection closed.
func TestV2AConnectionThatEndsBeforeItsTestIsDecidedAsItSettles(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Unix(7000, 0)
	ms := func(f float64) time.Time { return t0.Add(time.Duration(f * float64(time.Millisecond))) }
	at := startsOf([]time.Time{t0})
	for _, tc := range []struct {
		name     string
		trailing bool   // the client's last write, read at 21.5 ms
		decide   string // "end" (at 20.8 ms), "stop", "ctx"
		want     []string
	}{
		{"wholly inside its test", false, "end", []string{models.MetadataLifetimePerTest, ""}},
		{"its last write read after its test's answer", true, "end", []string{models.MetadataLifetimeSession, models.LifetimeUndecidedReadAfterAnswer + "@" + at}},
		{"the recording stops first", false, "stop", []string{models.MetadataLifetimeSession, models.LifetimeUndecidedStoppedFirst + "@" + at}},
		{"the parser ends first", false, "ctx", []string{models.MetadataLifetimeSession, models.LifetimeUndecidedStoppedFirst + "@" + at}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := syncMock.New(zap.NewNop())
			mgr.SetWatermark(neverBehind{})
			sc := mgr.BeginTestScope(t0, 0)
			defer sc.Abandon()
			clientCh := make(chan fakeconn.Chunk, 2)
			destCh := make(chan fakeconn.Chunk, 1)
			clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("QUOTE item=book"), ConnSeq: 1, ReadAt: ms(19)}
			destCh <- fakeconn.Chunk{Dir: fakeconn.FromDest, Bytes: []byte("CENTS 1250"), ConnSeq: 2, ReadAt: ms(19.2)}
			if tc.trailing {
				clientCh <- fakeconn.Chunk{Dir: fakeconn.FromClient, Bytes: []byte("BYE"), ConnSeq: 3, ReadAt: ms(21.5)}
			}
			close(clientCh)
			close(destCh)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sess, mocks := newV2Session(t, clientCh, destCh)
			sess.Ctx = syncMock.NewContext(ctx, mgr)
			decide := func() {
				switch tc.decide {
				case "end":
					sc.End(ms(20.8))
				case "stop":
					mgr.BacklogBefore(t0.Add(10 * time.Second))
				default:
					cancel()
				}
			}
			if err := dispatchEndingAfter(t, sess, 1, decide); err != nil {
				t.Fatal(err)
			}
			close(mocks)
			got := drainMocks(mocks)
			if len(got) != 1 {
				t.Fatalf("emitted %d mocks, want 1", len(got))
			}
			if g := []string{got[0].Spec.Metadata[models.MetadataLifetimeKey], markOf(t, got[0])}; !equal(g, tc.want) {
				t.Fatalf("the connection's only exchange is stated and marked %q, want %q", g, tc.want)
			}
		})
	}
}
