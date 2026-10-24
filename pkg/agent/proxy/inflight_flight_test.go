package proxy

import (
	"context"
	"net"
	"testing"

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
// first write lands it before its bytes go out, and its close lands it and
// lifts none again. Two requests aloft at once on parallel connections leave
// F's serves in them in an order keploy cannot tell; one sent once the
// other's answer went out keeps F's order.
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
	t.Run("a close lands and lifts no more", func(t *testing.T) {
		tr := learn.NewTracker()
		c1, app1 := flyingPair(t, tr, nil)
		c2, app2 := flyingPair(t, tr, nil)
		request(t, c1, app1)
		_ = c1.Close()
		c1.lift() // the close's own read (pairClosing) is no request
		request(t, c2, app2)
		tr.ServedMock(b)
		_ = c2.CloseWrite()
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
		"UpdateUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { db.UpdateUnFilteredMock(&m, &m); return true },
		"DeleteFilteredMock":   func(db integrations.MockMemDb, m models.Mock) bool { db.DeleteFilteredMock(m); return true },
		"DeleteUnFilteredMock": func(db integrations.MockMemDb, m models.Mock) bool { db.DeleteUnFilteredMock(m); return true },
		"DeleteStartupMock":    func(db integrations.MockMemDb, m models.Mock) bool { db.DeleteStartupMock(m); return true },
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			tr := learn.NewTracker()
			mm := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
			defer mm.Close()
			mm.SetServedObserver(tr.ServedMock)
			mm.SetServedInObserver(tr.ServedMockIn)
			c1, app1 := flyingPair(t, tr, nil)
			heartbeat, app2 := flyingPair(t, tr, nil)
			request(t, c1, app1)
			request(t, heartbeat, app2)
			ctx := integrations.WithRequestHolder(context.Background(), c1)
			door(integrations.ServingIn(ctx, mm), models.Mock{Name: "mock-a", Kind: models.HTTP})
			_, _ = heartbeat.Write([]byte("H"))
			_, _ = c1.Write([]byte("A"))
			if fa, la := tr.Placed().Order.Of(&models.Mock{Name: "mock-a", Kind: models.HTTP}); fa != 1 || la != 1 {
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
