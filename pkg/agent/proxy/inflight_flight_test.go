package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// beforeWrite is a conn whose Write runs before first, as the application
// reading the answer's first bytes the instant they go out would.
type beforeWrite struct {
	net.Conn
	before func()
}

func (b *beforeWrite) Write(p []byte) (int, error) {
	if f := b.before; f != nil {
		b.before = nil
		f()
	}
	return b.Conn.Write(p)
}

// flyingPair is a kept connection whose requests are told to tr, and the
// application's end of it. Writes on the kept end are drained, reads of it
// fed from the application's.
func flyingPair(t *testing.T, tr *learn.Tracker, wrap func(net.Conn) net.Conn) (*trackedConn, net.Conn) {
	t.Helper()
	app, kept := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = kept.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := app.Read(buf); err != nil {
				return
			}
		}
	}()
	var c net.Conn = kept
	if wrap != nil {
		c = wrap(kept)
	}
	return (&inflight{}).trackTold(c, tr), app
}

// request is the application sending one byte on app and keploy reading it
// on c.
func request(t *testing.T, c *trackedConn, app net.Conn) {
	t.Helper()
	go func() { _, _ = app.Write([]byte{'q'}) }()
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
}

// A kept connection tells the learn step each request the application sends
// on it (learn.Flight): a read that returns bytes lifts it, the answer's
// first write lands it before its bytes go out, and its close ends it
// (untold, with no answer started) and lifts none again. Two requests aloft
// at once on parallel connections leave F's serves in them in an order
// keploy cannot tell; one sent once the other's answer went out keeps F's
// order.
func TestAKeptConnectionTellsTheLearnStepItsRequests(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	t.Run("one after the other", func(t *testing.T) {
		tr := learn.NewTracker()
		c1, app1 := flyingPair(t, tr, nil)
		c2, app2 := flyingPair(t, tr, nil)
		request(t, c1, app1)
		tr.ServedMock(a)
		_, _ = c1.Write([]byte("A"))
		request(t, c2, app2)
		tr.ServedMock(b)
		_, _ = c2.Write([]byte("B"))
		o := tr.Placed().Order
		if fa, _ := o.Of(a); fa != 1 {
			t.Fatalf("A at %d; want 1", fa)
		}
		if fb, _ := o.Of(b); fb != 2 {
			t.Fatalf("B at %d; want 2", fb)
		}
	})
	t.Run("both aloft", func(t *testing.T) {
		tr := learn.NewTracker()
		c1, app1 := flyingPair(t, tr, nil)
		c2, app2 := flyingPair(t, tr, nil)
		request(t, c1, app1)
		request(t, c2, app2)
		tr.ServedMock(a)
		_, _ = c1.Write([]byte("A"))
		tr.ServedMock(b)
		_, _ = c2.Write([]byte("B"))
		o := tr.Placed().Order
		if fa, la := o.Of(a); fa != 0 || la != 0 {
			t.Fatalf("A at %d,%d; want 0,0", fa, la)
		}
		if fb, lb := o.Of(b); fb != 0 || lb != 0 {
			t.Fatalf("B at %d,%d; want 0,0", fb, lb)
		}
	})
	t.Run("the next request sent as the answer goes out", func(t *testing.T) {
		tr := learn.NewTracker()
		var c2 *trackedConn
		var app2 net.Conn
		hook := &beforeWrite{}
		c1, app1 := flyingPair(t, tr, func(c net.Conn) net.Conn { hook.Conn = c; return hook })
		c2, app2 = flyingPair(t, tr, nil)
		request(t, c1, app1)
		tr.ServedMock(a)
		// The application reads A's answer and sends B on its other
		// connection before keploy's write of A returns.
		hook.before = func() { request(t, c2, app2) }
		_, _ = c1.Write([]byte("A"))
		tr.ServedMock(b)
		_, _ = c2.Write([]byte("B"))
		o := tr.Placed().Order
		if fa, _ := o.Of(a); fa != 1 {
			t.Fatalf("A at %d; want 1: its request landed before its answer went out", fa)
		}
		if fb, _ := o.Of(b); fb != 2 {
			t.Fatalf("B at %d; want 2", fb)
		}
	})
	t.Run("a close ends its request and lifts no more", func(t *testing.T) {
		tr := learn.NewTracker()
		c1, app1 := flyingPair(t, tr, nil)
		c2, app2 := flyingPair(t, tr, nil)
		request(t, c1, app1)
		_ = c1.Close()
		c1.lift() // the close's own read (pairClosing) is no request
		request(t, c2, app2)
		tr.ServedMock(b)
		_, _ = c2.Write([]byte("B"))
		if fb, _ := tr.Placed().Order.Of(b); fb != 1 {
			t.Fatalf("B at %d; want 1: the closed connection holds no request aloft", fb)
		}
	})
	t.Run("told to no tracker", func(t *testing.T) {
		c, app := flyingPair(t, nil, nil)
		request(t, c, app)
		if _, err := c.Write([]byte("A")); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

// A parser that says whose request it serves (integrations.ServingIn) has
// the mock manager tell the learn step the serve is that connection's
// request's: a heartbeat aloft on a parallel connection, which reads no
// recording, leaves it in F's order, through each consume door, through a
// worker's view made before exact serving began, and through a round scope
// too. A connection told to no tracker says nothing.
func TestAServeSaidInItsRequestIsOrderedPastAHeartbeat(t *testing.T) {
	doors := map[string]func(db integrations.MockMemDb, m models.Mock) bool{
		"MarkMockAsUsed":       func(db integrations.MockMemDb, m models.Mock) bool { return db.MarkMockAsUsed(m) },
		"UpdateUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { return db.UpdateUnFilteredMock(&m, &m) },
		"DeleteFilteredMock":   func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteFilteredMock(m) },
		"DeleteUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteUnFilteredMock(m) },
		"DeleteStartupMock":    func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteStartupMock(m) },
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			tr := learn.NewTracker()
			a := doorMock(name, "mock-a", 0, models.HTTP)
			mm := stagedForDoor(t, name, a)
			mm.SetServedObserver(tr.ServedMock)
			mm.SetServedInObserver(tr.ServedMockIn)
			c1, app1 := flyingPair(t, tr, nil)
			heartbeat, app2 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			request(t, heartbeat, app2)
			ctx := integrations.WithRequestHolder(context.Background(), c1)
			if !door(integrations.ServingIn(ctx, mm), *a) {
				t.Fatal("the door refused a mock its tier holds")
			}
			_, _ = heartbeat.Write([]byte("H"))
			_, _ = c1.Write([]byte("A"))
			if fa, la := tr.Placed().Order.Of(a); fa != 1 || la != 1 {
				t.Fatalf("A at %d,%d; want 1,1: the heartbeat read no recording", fa, la)
			}
		})
	}
	for name, door := range doors {
		t.Run("under exact serving, through the proxy's observers: "+name, func(t *testing.T) {
			provenswitch.Pin(true)
			t.Cleanup(provenswitch.Unpin)
			tr := learn.NewTracker()
			p := &Proxy{learnTracker: tr}
			a := exactMock("a", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{a})
			mm.ServeExactPool("-", nil)
			mm.ServeExactTest("T", []*models.Mock{a})
			p.setMockManager(mm)
			c1, app1 := flyingPair(t, tr, nil)
			heartbeat, app2 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			request(t, heartbeat, app2)
			pt, _ := mm.GetPerTestMocksInWindow()
			if len(pt) != 1 {
				t.Fatalf("the test shows %d mocks; want a", len(pt))
			}
			door(integrations.ServingIn(integrations.WithRequestHolder(context.Background(), c1), mm), *pt[0])
			_, _ = heartbeat.Write([]byte("H"))
			_, _ = c1.Write([]byte("A"))
			if fa, la := tr.Placed().Order.Of(pt[0]); fa != 1 || la != 1 {
				t.Fatalf("a at %d,%d; want 1,1: the heartbeat read no recording", fa, la)
			}
		})
	}
	for name, door := range doors {
		// A connection whose worker view was made before exact serving began
		// keeps that view for its life (scopedFor): the view tells the
		// serve's request as the manager does, and stays the view.
		t.Run("under exact serving, on a worker's view made before it began: "+name, func(t *testing.T) {
			provenswitch.Pin(true)
			t.Cleanup(provenswitch.Unpin)
			tr := learn.NewTracker()
			p := &Proxy{learnTracker: tr}
			a := exactMock("a", 0, models.LifetimeSession)
			mm := stagedManager(t, nil, []*models.Mock{a})
			mm.ServeExactPool("-", nil)
			mm.ServeExactTest("T", []*models.Mock{a})
			p.setMockManager(mm)
			view := &scopedMockDb{MockMemDb: mm, src: p}
			c1, app1 := flyingPair(t, tr, nil)
			heartbeat, app2 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			request(t, heartbeat, app2)
			pt, _ := view.GetPerTestMocksInWindow()
			if len(pt) != 1 {
				t.Fatalf("the test shows %d mocks; want a", len(pt))
			}
			got := integrations.ServingIn(integrations.WithRequestHolder(context.Background(), c1), view)
			if sv, ok := got.(*scopedMockDb); !ok || sv.src != p || sv == view {
				t.Fatalf("the worker's view serving c1's request is %T; want a copy of the view over the manager's teller", got)
			}
			door(got, *pt[0])
			_, _ = heartbeat.Write([]byte("H"))
			_, _ = c1.Write([]byte("A"))
			if fa, la := tr.Placed().Order.Of(pt[0]); fa != 1 || la != 1 {
				t.Fatalf("a at %d,%d; want 1,1: the heartbeat read no recording", fa, la)
			}
		})
	}
	t.Run("a round scope", func(t *testing.T) {
		tr := learn.NewTracker()
		mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
		defer mm.Close()
		c1, app1 := flyingPair(t, tr, nil)
		request(t, c1, app1)
		scope := mm.WithinRound(integrations.RoundAt{})
		got := integrations.ServingIn(integrations.WithRequestHolder(context.Background(), c1), scope)
		rs, ok := got.(*roundScope)
		if !ok || got != integrations.MockMemDb(scope) || rs.g.in != c1.InFlight() || rs.g.free {
			t.Fatalf("a round scope serving c1's request is %#v; want the scope itself, guarded, told c1's request", got)
		}
	})
	t.Run("told to no tracker", func(t *testing.T) {
		mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
		defer mm.Close()
		c, app := flyingPair(t, nil, nil)
		request(t, c, app)
		if got := integrations.ServingIn(integrations.WithRequestHolder(context.Background(), c), mm); got != integrations.MockMemDb(mm) {
			t.Fatalf("a connection told to no tracker serves through %T; want the manager itself", got)
		}
	})
}

// A parser that serves in no request of the application's (a server push
// nothing it sent caused) tells the store so (ServingFor(learn.NoRequest())):
// through each consume door, the push makes neither the request nor the
// heartbeat aloft beside it a serve, so the request's own serve keeps its
// place in F's order, and the push itself is in an order keploy cannot tell.
// A push a request caused, consumed once that request's answer went out, is
// told in it (learn.Flight.LiftCaused): after the request's serve, in F's
// order once it landed.
func TestAServeToldInNoRequestOrInItsCauseThroughTheStore(t *testing.T) {
	doors := map[string]func(db integrations.MockMemDb, m models.Mock) bool{
		"MarkMockAsUsed":       func(db integrations.MockMemDb, m models.Mock) bool { return db.MarkMockAsUsed(m) },
		"UpdateUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { return db.UpdateUnFilteredMock(&m, &m) },
		"DeleteFilteredMock":   func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteFilteredMock(m) },
		"DeleteUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteUnFilteredMock(m) },
		"DeleteStartupMock":    func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteStartupMock(m) },
	}
	for name, door := range doors {
		a, push := doorMock(name, "mock-a", 0, models.REDIS), doorMock(name, "mock-push", time.Second, models.REDIS)
		t.Run("in no request: "+name, func(t *testing.T) {
			tr := learn.NewTracker()
			mm := stagedForDoor(t, name, a, push)
			mm.SetServedObserver(tr.ServedMock)
			mm.SetServedInObserver(tr.ServedMockIn)
			c1, app1 := flyingPair(t, tr, nil)
			heartbeat, app2 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			request(t, heartbeat, app2)
			if !door(mm.ServingFor(learn.NoRequest()), *push) ||
				!door(integrations.ServingIn(integrations.WithRequestHolder(context.Background(), c1), mm), *a) {
				t.Fatal("the door refused a mock its tier holds")
			}
			_, _ = heartbeat.Write([]byte("H"))
			_, _ = c1.Write([]byte("A"))
			o := tr.Placed().Order
			if f, l := o.Of(a); f != 2 || l != 2 {
				t.Fatalf("A at %d,%d; want 2,2: the push was made in no request", f, l)
			}
			if f, l := o.Of(push); f != 0 || l != 0 {
				t.Fatalf("the push at %d,%d; want 0,0", f, l)
			}
		})
		t.Run("in its cause, once the cause's answer went out: "+name, func(t *testing.T) {
			tr := learn.NewTracker()
			mm := stagedForDoor(t, name, a, push)
			mm.SetServedObserver(tr.ServedMock)
			mm.SetServedInObserver(tr.ServedMockIn)
			c1, app1 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			ctx := integrations.WithRequestHolder(context.Background(), c1)
			cause := integrations.InFlightOf(ctx)
			if !door(integrations.ServingIn(ctx, mm), *a) {
				t.Fatal("the door refused a mock its tier holds")
			}
			_, _ = c1.Write([]byte("A"))
			p := cause.LiftCaused()
			if !door(mm.ServingFor(p), *push) {
				t.Fatal("the door refused a mock its tier holds")
			}
			p.Land()
			o := tr.Placed().Order
			if f, l := o.Of(a); f != 1 || l != 1 {
				t.Fatalf("A at %d,%d; want 1,1", f, l)
			}
			if f, l := o.Of(push); f != 2 || l != 2 {
				t.Fatalf("the push at %d,%d; want 2,2: told in its cause", f, l)
			}
		})
	}
}

// A consume the view refuses, in a request a parser says it serves
// (integrations.ServingIn), makes that request no serve: a serve made in a
// request aloft beside it keeps its place in F's order once it lands,
// through each consume door. Before, the refused consume was told to the
// tracker, which numbered it in its request and left the other serve in no
// order keploy can tell.
func TestAConsumeTheViewRefusedMakesItsRequestNoServe(t *testing.T) {
	doors := map[string]func(db integrations.MockMemDb, m models.Mock) bool{
		"MarkMockAsUsed":       func(db integrations.MockMemDb, m models.Mock) bool { return db.MarkMockAsUsed(m) },
		"UpdateUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { c := m; return db.UpdateUnFilteredMock(&m, &c) },
		"DeleteFilteredMock":   func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteFilteredMock(m) },
		"DeleteUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { return db.DeleteUnFilteredMock(m) },
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			provenswitch.Pin(true)
			t.Cleanup(provenswitch.Unpin)
			tr := learn.NewTracker()
			p := &Proxy{learnTracker: tr}
			a := exactMock("a", 0, models.LifetimePerTest)
			other := exactMock("other", time.Second, models.LifetimePerTest)
			mm := stagedManager(t, []*models.Mock{a, other}, nil)
			mm.ServeExactPool("-", nil)
			mm.ServeExactTest("T", []*models.Mock{a})
			p.setMockManager(mm)
			c1, app1 := flyingPair(t, tr, nil)
			c2, app2 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			request(t, c2, app2)
			if door(integrations.ServingIn(integrations.WithRequestHolder(context.Background(), c1), mm), *other) {
				t.Fatal("precondition: another test's recording answered in the open test")
			}
			if !door(integrations.ServingIn(integrations.WithRequestHolder(context.Background(), c2), mm), *a) {
				t.Fatal("the open test's own mock was refused")
			}
			_, _ = c1.Write([]byte("R"))
			_, _ = c2.Write([]byte("A"))
			o := tr.Placed().Order
			if fa, la := o.Of(a); fa != 1 || la != 1 {
				t.Fatalf("a at %d,%d; want 1,1: the request beside it was served nothing", fa, la)
			}
			if fo, lo := o.Of(other); fo != 0 || lo != 0 {
				t.Fatalf("the refused recording at %d,%d; want no serve of it", fo, lo)
			}
		})
	}
}

// A request a kept connection gives no answer is ended untold
// (learn.Flight.EndUntold), so a serve made in it is in an order keploy
// cannot tell (0,0): the application never read it, so it read it before no
// later request. Its connection closes, or half-closes, before the answer
// starts, or the write that starts the answer fails. A request answered
// whole keeps its serve in F's order, closed after or not, and so does one
// whose answer had its first write: a later write that fails (the rest of
// the answer, or one after it once the application hung up) ends nothing,
// as the connection cannot tell the two apart.
func TestAKeptConnectionEndsUntoldARequestItGivesNoAnswer(t *testing.T) {
	a, b := &models.Mock{Name: "mock-a"}, &models.Mock{Name: "mock-b"}
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, c *trackedConn, app net.Conn)
		// told: A stays in F's order (1,1).
		told bool
	}{
		{"closed with it aloft", func(_ *testing.T, c *trackedConn, _ net.Conn) { _ = c.Close() }, false},
		{"half-closed with it aloft", func(_ *testing.T, c *trackedConn, _ net.Conn) { _ = c.CloseWrite() }, false},
		{"its answer's first write fails", func(t *testing.T, c *trackedConn, app net.Conn) {
			_ = app.Close()
			if _, err := c.Write([]byte("A")); err == nil {
				t.Fatal("a write to a closed pipe succeeded")
			}
		}, false},
		{"a later write of its answer fails", func(t *testing.T, c *trackedConn, app net.Conn) {
			if _, err := c.Write([]byte("A1")); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			if _, err := c.Write([]byte("A2")); err == nil {
				t.Fatal("a write to a closed pipe succeeded")
			}
		}, true},
		{"answered whole, the application hangs up, and a write after fails", func(t *testing.T, c *trackedConn, app net.Conn) {
			if _, err := c.Write([]byte("A")); err != nil {
				t.Fatal(err)
			}
			_ = app.Close()
			if _, err := c.Write([]byte("x")); err == nil {
				t.Fatal("a write to a closed pipe succeeded")
			}
			_ = c.Close()
		}, true},
		{"answered whole, then closed", func(t *testing.T, c *trackedConn, _ net.Conn) {
			if _, err := c.Write([]byte("A")); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
		}, true},
		{"answered whole, then half-closed", func(t *testing.T, c *trackedConn, _ net.Conn) {
			if _, err := c.Write([]byte("A")); err != nil {
				t.Fatal(err)
			}
			_ = c.CloseWrite()
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := learn.NewTracker()
			c1, app1 := flyingPair(t, tr, nil)
			c2, app2 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			tr.ServedMock(a)
			tc.end(t, c1, app1)
			// The application sends B once it is done with c1.
			request(t, c2, app2)
			tr.ServedMock(b)
			if _, err := c2.Write([]byte("B")); err != nil {
				t.Fatal(err)
			}
			o := tr.Placed().Order
			fa, la := o.Of(a)
			if tc.told && (fa != 1 || la != 1) {
				t.Fatalf("A at %d,%d; want 1,1: its answer's first write went out", fa, la)
			}
			if !tc.told && (fa != 0 || la != 0) {
				t.Fatalf("A at %d,%d; want 0,0: the application was given no answer to its request", fa, la)
			}
			if fb, lb := o.Of(b); fb != 2 || lb != 2 {
				t.Fatalf("B at %d,%d; want 2,2: its request lifted after A's ended", fb, lb)
			}
		})
	}
}
