package proxy

import (
	"fmt"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/internal/testnet"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/agent/starts"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A connection the kernel stamped with a live test worker's instance is
// that worker's; once the worker has ended, one stamped with its value is
// from no live worker: it is given no instance, so it is never the use of
// the test of a later claim of the same id, and it names the ended worker.
// One stamped with no instance names none.
func TestARealConnectionOfAnEndedWorkerIsNoWorkers(t *testing.T) {
	v := agent.InstanceValue(7, 3)
	starts.Default.InstanceBegan(v)
	t.Cleanup(func() { starts.Default.InstanceEnded(v) })
	dest := &agent.NetworkAddress{Instance: v, KernelPid: 42}
	require.Equal(t, realdeps.Origin{Instance: v, Pid: 42}, realOrigin(dest))
	starts.Default.InstanceEnded(v)
	require.Equal(t, realdeps.Origin{Pid: 42, Ended: "7 (claim 4 of that id)"}, realOrigin(dest))
	require.Equal(t, realdeps.Origin{Pid: 42}, realOrigin(&agent.NetworkAddress{KernelPid: 42}))
	require.Equal(t, realdeps.Origin{}, realOrigin(nil))
}

// liveInstances makes vs live test workers' instances while t runs
// (starts.Default): a connection stamped with one is its worker's.
func liveInstances(t *testing.T, vs ...uint32) {
	for _, v := range vs {
		starts.Default.InstanceBegan(v)
		t.Cleanup(func() { starts.Default.InstanceEnded(v) })
	}
}

// survivorFrom makes a and b live test workers' instances, each with a test
// open on p's tracker, and returns the origin a connection from a's process
// (pid 4242) has at its accept, while a lives: a's own.
func survivorFrom(t *testing.T, p *Proxy, a, b uint32) realdeps.Origin {
	t.Helper()
	liveInstances(t, a, b)
	p.realTracker.BeginTestOf(a)
	p.realTracker.BeginTestOf(b)
	from := realOrigin(&agent.NetworkAddress{Instance: a, KernelPid: 4242})
	require.Equal(t, realdeps.Origin{Instance: a, Pid: 4242}, from, "opened while its worker lives")
	return from
}

// endWorkerA ends worker a's test, then worker a, and returns a's test's
// end: a process a left behind stays, with the connections it opened.
func endWorkerA(t *testing.T, p *Proxy, a uint32) realdeps.TestEnd {
	t.Helper()
	end := p.realTracker.EndTestOf(a)
	require.Empty(t, end.Unattributed, "worker a's own uses are never unattributed")
	starts.Default.InstanceEnded(a)
	return end
}

// requireCountedAsEndedWorkers ends worker b's test and requires that it
// counts n connections' uses keploy cannot attribute (a process of ended
// worker a's), naming a, and none as b's own.
func requireCountedAsEndedWorkers(t *testing.T, p *Proxy, a, b uint32, n uint64, what string) {
	t.Helper()
	end := p.realTracker.EndTestOf(b)
	require.Empty(t, end.Uses, what+": never worker b's own use")
	require.Len(t, end.Unattributed, 1, what)
	require.Equal(t, models.CaptureLossUnattributed, end.Unattributed[0].Kind, what)
	require.Equal(t, n, end.Unattributed[0].Count, what)
	require.Contains(t, end.Unattributed[0].What, "from process 4242 of ended test worker "+agent.InstanceLabel(a), what)
}

// readOn has peer send c bytes, and reads them through c.
func readOn(t *testing.T, c net.Conn, peer net.Conn) {
	t.Helper()
	go func() { _, _ = peer.Write([]byte("a query")) }()
	if _, err := c.Read(make([]byte, 64)); err != nil {
		t.Fatal(err)
	}
}

// A mock.real connection is charged by who it is from as keploy reads each
// use, not as it was accepted. Bytes a process of worker a sends while a
// lives are a's test's use. Once a has ended, the bytes that process sends
// on that same connection (one it opened before the end) are a use from no
// live worker: counted to worker b's open test as a call keploy cannot
// attribute, naming the ended worker, never dropped as an ended test's,
// exactly as on a connection the process opens after the end. Whether the
// live handler serves the connection or not.
func TestARealUseAfterItsWorkerEndedOnAConnectionOpenedBeforeIsCounted(t *testing.T) {
	for i, live := range []bool{false, true} {
		a, b := agent.InstanceValue(uint32(51+2*i), 1), agent.InstanceValue(uint32(52+2*i), 0)
		what := fmt.Sprintf("live %v", live)
		p := &Proxy{logger: zap.NewNop()}
		from := survivorFrom(t, p, a, b)
		app, peer := net.Pipe()
		t.Cleanup(func() { _ = app.Close(); _ = peer.Close() })
		pre := &treeRealConn{Conn: app, p: p, label: "db:5432", id: 1, from: from, live: live}
		readOn(t, pre, peer)
		require.Equal(t, []models.RealUse{{Dest: "db:5432", Conns: 1}}, endWorkerA(t, p, a).Uses, what+": a's use while a lives")
		readOn(t, pre, peer)
		requireCountedAsEndedWorkers(t, p, a, b, 1, what+": the connection opened before the end")

		// Control: a connection the process opens after the end.
		p.realTracker.BeginTestOf(b)
		app2, peer2 := net.Pipe()
		t.Cleanup(func() { _ = app2.Close(); _ = peer2.Close() })
		post := &treeRealConn{Conn: app2, p: p, label: "db:5432", id: 2, from: realOrigin(&agent.NetworkAddress{Instance: a, KernelPid: 4242}), live: live}
		readOn(t, post, peer2)
		requireCountedAsEndedWorkers(t, p, a, b, 1, what+": a connection opened after the end")
	}
}

// The same holds for a mock.real connection keploy relays to the real
// destination itself: accepted while worker a lived, the bytes it sends
// after a has ended, and a dial that fails after the end, are uses from no
// live worker, counted to worker b's open test, naming a.
func TestARelayedRealUseAfterItsWorkerEndedIsCounted(t *testing.T) {
	dst := realServer(t)
	p := realProxy(t, []string{dst}, nil)
	a, b := agent.InstanceValue(61, 2), agent.InstanceValue(62, 0)
	from := survivorFrom(t, p, a, b)
	endWorkerA(t, p, a)
	app, accepted := realConnPair(t)
	done := serveRealFrom(p, accepted, dst, from)
	require.Equal(t, "real:hello", sendAndRead(t, app, "hello"))
	require.True(t, (<-done).handled)
	requireCountedAsEndedWorkers(t, p, a, b, 1, "relayed")

	refused := testnet.RefusedAddr(t, "tcp")
	p = realProxy(t, []string{refused}, nil)
	a, b = agent.InstanceValue(63, 1), agent.InstanceValue(64, 0)
	from = survivorFrom(t, p, a, b)
	endWorkerA(t, p, a)
	_, accepted = realConnPair(t)
	require.True(t, (<-serveRealFrom(p, accepted, refused, from)).handled)
	requireCountedAsEndedWorkers(t, p, a, b, 1, "refused at the dial")
}

// A name check that ends a mock.real connection is charged by who the
// connection is from when keploy ends it, not as it was accepted: a process
// an ended worker left behind, refused on a connection it opened before the
// end, gives its capture_incomplete to the test open then (worker b's),
// naming the ended worker, exactly as on a connection it opens after the
// end; never to the run alone, outside every test.
func TestARealNameCheckAfterItsWorkerEndedIsTheOpenTestsNamingIt(t *testing.T) {
	dst := realServer(t)
	_, portText, _ := net.SplitHostPort(dst)
	named := "api.test:" + portText
	two := "GET / HTTP/1.1\r\nHost: api.test\r\n\r\nGET / HTTP/1.1\r\nHost: web.test\r\n\r\n"
	for i, before := range []bool{true, false} {
		what := fmt.Sprintf("opened before the end: %v", before)
		p := realProxy(t, []string{named}, map[string]string{"api.test": "127.0.0.1", "web.test": "127.0.0.1"})
		a, b := agent.InstanceValue(uint32(71+2*i), 1), agent.InstanceValue(uint32(72+2*i), 0)
		from := survivorFrom(t, p, a, b)
		endWorkerA(t, p, a)
		if !before {
			from = realOrigin(&agent.NetworkAddress{Instance: a, KernelPid: 4242})
		}
		app, accepted := realConnPair(t)
		done := serveRealFrom(p, accepted, dst, from)
		sendAndRead(t, app, two)
		require.True(t, (<-done).handled)
		requireIncompleteOfEndedWorker(t, p, a, b, "web.test", what)
	}
}

// requireIncompleteOfEndedWorker ends worker b's test and requires that it
// was charged one capture_incomplete, saying why (with want) and naming the
// process 4242 of ended worker a, and that the run holds none of it.
func requireIncompleteOfEndedWorker(t *testing.T, p *Proxy, a, b uint32, want, what string) {
	t.Helper()
	end := p.realTracker.EndTestOf(b)
	require.Len(t, end.Incomplete, 1, what)
	require.Contains(t, end.Incomplete[0].Why, want, what)
	require.Contains(t, end.Incomplete[0].Why, "from process 4242 of ended test worker "+agent.InstanceLabel(a), what)
	require.Empty(t, p.RealRun().Incomplete, what+": never the run's alone")
}

// A connection keploy is still reading a name from is charged, when its
// client closes it unnamed, to the test open for who it is from then: one a
// process of worker a opened while a lived, still unnamed when a's test and
// a ended (a's end was charged with it), is from no live worker when it
// closes, so worker b's open test is charged with it, naming a, exactly as
// with one the process opens after the end.
func TestARealConnectionClosedUnnamedAfterItsWorkerEndedIsTheOpenTests(t *testing.T) {
	for i, before := range []bool{true, false} {
		what := fmt.Sprintf("opened before the end: %v", before)
		p := realProxy(t, []string{"db.test:3306"}, map[string]string{"db.test": "127.0.0.1"})
		a, b := agent.InstanceValue(uint32(81+2*i), 1), agent.InstanceValue(uint32(82+2*i), 0)
		from := survivorFrom(t, p, a, b)
		app, accepted := realConnPair(t)
		reading := &readSignal{Conn: accepted, started: make(chan struct{})}
		var done chan served
		if before {
			done = serveRealFrom(p, reading, "127.0.0.1:3306", from)
			// serveReal reads the client for its name once the connection
			// is pending in the tracker.
			awaitEvent(t, reading.started, "serveReal reading the client for its name")
			require.Len(t, endWorkerA(t, p, a).Incomplete, 1, "undecided while a's test was open")
		} else {
			endWorkerA(t, p, a)
			done = serveRealFrom(p, reading, "127.0.0.1:3306", realOrigin(&agent.NetworkAddress{Instance: a, KernelPid: 4242}))
			awaitEvent(t, reading.started, "serveReal reading the client for its name")
		}
		require.NoError(t, app.Close())
		require.True(t, (<-done).handled)
		requireIncompleteOfEndedWorker(t, p, a, b, "before naming it", what)
	}
}

// A worker's instance can end while its test is still open (its process
// exited: keploy ends the instance, then fails the test). Bytes a process it
// left behind sends after that, on an h2c connection opened before, are a
// use from no live worker: counted to every open test as a call keploy
// cannot attribute, and no h2 note goes on the ended worker's own row,
// which its own earlier connection, not h2, filled.
func TestAnH2UseAfterItsWorkersInstanceEndedNotesNoRowOfThatWorker(t *testing.T) {
	dst := realServer(t)
	_, portText, _ := net.SplitHostPort(dst)
	named := "api.test:" + portText
	p := realProxy(t, []string{named}, map[string]string{"api.test": "127.0.0.1"})
	p.real.Load().book.Answered("api.test", []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	a, b := agent.InstanceValue(91, 1), agent.InstanceValue(92, 0)
	from := survivorFrom(t, p, a, b)
	app, accepted := realConnPair(t)
	done := serveRealFrom(p, accepted, dst, from)
	sendAndRead(t, app, "GET / HTTP/1.1\r\nHost: api.test\r\n\r\n")
	require.True(t, (<-done).handled)

	starts.Default.InstanceEnded(a)
	app, accepted = realConnPair(t)
	done = serveRealFrom(p, accepted, dst, from)
	preface := "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	require.Equal(t, "real:"+preface, sendAndRead(t, app, preface))
	require.True(t, (<-done).handled)

	end := p.realTracker.EndTestOf(a)
	require.Equal(t, []models.RealUse{{Dest: named, Conns: 1}}, end.Uses, "its own connection's use, with no h2 note")
	require.Len(t, end.Unattributed, 1)
	requireCountedAsEndedWorkers(t, p, a, b, 1, "the h2c connection's bytes")
}
