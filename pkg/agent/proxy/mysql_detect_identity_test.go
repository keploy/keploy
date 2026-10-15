package proxy

import (
	"context"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// silentClient is an application's end of a connection that says nothing. A
// read under a deadline fails at once, as a silent client's does once the
// deadline passes; a read with none waits until the connection closes or a
// deadline is set, and says on waiting that it waits. clocks counts the
// deadlines set in the future, each a clock the code under test waited on;
// what the code under test writes (a greeting) is kept in written.
type silentClient struct {
	mu       sync.Mutex
	deadline time.Time
	closed   bool
	wake     chan struct{}
	waiting  chan struct{}
	clocks   int
	written  strings.Builder
}

func newSilentClient() *silentClient {
	return &silentClient{wake: make(chan struct{}), waiting: make(chan struct{}, 1)}
}

func (c *silentClient) Read([]byte) (int, error) {
	for {
		c.mu.Lock()
		switch {
		case c.closed:
			c.mu.Unlock()
			return 0, io.EOF
		case !c.deadline.IsZero():
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		wake := c.wake
		c.mu.Unlock()
		select {
		case c.waiting <- struct{}{}:
		default:
		}
		<-wake
	}
}

func (c *silentClient) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.After(time.Now()) {
		c.clocks++
	}
	c.deadline = t
	close(c.wake)
	c.wake = make(chan struct{})
	return nil
}

func (c *silentClient) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.written.Write(b)
}

func (c *silentClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		close(c.wake)
		c.wake = make(chan struct{})
	}
	return nil
}

func (c *silentClient) clocksUsed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clocks
}

func (*silentClient) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 16789}
}
func (*silentClient) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000}
}
func (c *silentClient) SetDeadline(t time.Time) error  { return c.SetReadDeadline(t) }
func (*silentClient) SetWriteDeadline(time.Time) error { return nil }

// readOrVerdict waits until the probe reads the silent client (nil) or
// returns a verdict first, with awaitEvent's bound, so code that does neither
// fails the test instead of hanging it.
func readOrVerdict(t *testing.T, done <-chan *mysqlProbe, client *silentClient) *mysqlProbe {
	t.Helper()
	first := make(chan *mysqlProbe, 1)
	go func() {
		select {
		case probe := <-done:
			first <- probe
		case <-client.waiting:
			first <- nil
		}
	}()
	return awaitEvent(t, first, "the probe reading the client, or deciding")
}

// verdict runs a replay probe of client's connection to dstAddr and returns its
// verdict, failing the test at once if the probe reads the client instead.
func verdict(t *testing.T, p *Proxy, client *silentClient, dstAddr string) *mysqlProbe {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := replayProbe(ctx, p, client, dstAddr, zap.NewNop())
	probe := readOrVerdict(t, done, client)
	if probe == nil {
		cancel()
		awaitEvent(t, done, "the probe returning")
		t.Fatalf("the probe read the client of %s instead of deciding", dstAddr)
	}
	return probe
}

// recordedMySQL derives p's recorded MySQL destinations from one MySQL mock
// recorded at destAddr, stamped with container key key ("" for none).
func recordedMySQL(p *Proxy, destAddr, key string) {
	md := map[string]string{"destAddr": destAddr}
	if key != "" {
		md[containerkey.MetadataKey] = key
		md[containerkey.InstanceKey] = "instance-of-" + key
	}
	p.deriveMysqlPorts([]*models.Mock{{Kind: models.MySQL, Spec: models.MockSpec{Metadata: md}}})
}

// replayProbe probes client's connection to dstAddr in a replay, on a
// goroutine of its own.
func replayProbe(ctx context.Context, p *Proxy, client net.Conn, dstAddr string, logger *zap.Logger) <-chan *mysqlProbe {
	done := make(chan *mysqlProbe, 1)
	go func() {
		port, _ := portFromAddr(dstAddr)
		probe, _ := p.probeMysql(ctx, client, dstAddr, nil, port, models.MODE_TEST, models.OutgoingOptions{}, logger)
		done <- probe
	}()
	return done
}

// A silent client of a destination that is neither the host nor the
// container the recording reached MySQL at is not greeted as MySQL, however
// long it stays silent: no clock decides. Today's inference greeted any
// client silent past 250ms (2.25s on the first connection to an endpoint) on
// a port the recording never saw, so a connection pool's idle connection to
// any other service was answered with a MySQL handshake.
func TestReplayDoesNotGreetASilentClientOfAnotherDestination(t *testing.T) {
	p := testProxy(t)
	recordedMySQL(p, "127.0.0.1:3306", "")
	client := newSilentClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := replayProbe(ctx, p, client, "10.9.9.9:5672", zap.NewNop())
	if probe := readOrVerdict(t, done, client); probe != nil {
		t.Fatalf("the probe decided (IsMySQL=%v, reason %q) while the client had said nothing and the run was on", probe.IsMySQL, probe.Reason)
	}
	cancel()
	probe := awaitEvent(t, done, "the probe returning once the run ended")
	if probe.IsMySQL {
		t.Fatalf("reason %q: a silent client of another destination was greeted as MySQL", probe.Reason)
	}
	if n := client.clocksUsed(); n != 0 {
		t.Fatalf("the probe waited on %d clock(s)", n)
	}
	if len(client.written.String()) != 0 {
		t.Fatalf("the probe wrote %q to the client", client.written.String())
	}
}

// A MySQL endpoint whose port moved on the host the recording reached it at
// is MySQL by that identity: greeted at once, with no clock.
func TestReplayGreetsAMovedPortOnTheRecordedHostAtOnce(t *testing.T) {
	p := testProxy(t)
	recordedMySQL(p, "10.1.2.3:3306", "")
	client := newSilentClient()

	probe := verdict(t, p, client, "10.1.2.3:3307")
	if !probe.IsMySQL || probe.Reason != "recorded-endpoint-moved" {
		t.Fatalf("IsMySQL=%v reason %q, want true/recorded-endpoint-moved", probe.IsMySQL, probe.Reason)
	}
	if n := client.clocksUsed(); n != 0 {
		t.Fatalf("the probe waited on %d clock(s) before deciding", n)
	}
	if p.mysqlPorts.Has(3307) {
		t.Fatal("a moved endpoint must not enter the fast-path set: the next connection is judged by its own destination")
	}
}

// This machine is never the recorded host (founder decision 29): every local
// service shares a loopback address, so another port of it is not taken for
// the recorded MySQL, and no race to a client's first bytes can greet a local
// client-first client. The client is read instead; when it gives up, the miss
// says to list the port. A listed port is MySQL.
func TestReplayNeverTakesAnotherPortOfThisMachineForTheRecordedMySQL(t *testing.T) {
	for _, tc := range []struct{ recorded, live string }{
		{"127.0.0.1:3306", "127.0.0.1:3307"},
		{"127.0.0.1:3306", "[127.0.0.1]:3307"},
		{"[::ffff:127.0.0.1]:3306", "[::1]:3307"},
		{"127.0.0.1:3306", "0.0.0.0:3307"},
		{"[::1]:3306", "[::]:3307"},
	} {
		t.Run(tc.recorded+" to "+tc.live, func(t *testing.T) {
			p := testProxy(t)
			p.errChannel = make(chan error, 4)
			recordedMySQL(p, tc.recorded, "")
			client := newSilentClient()
			done := replayProbe(context.Background(), p, client, tc.live, zap.NewNop())
			if probe := readOrVerdict(t, done, client); probe != nil {
				t.Fatalf("IsMySQL=%v reason %q: another port of this machine was judged without the client", probe.IsMySQL, probe.Reason)
			}
			_ = client.Close() // the application gave up waiting for a greeting
			if probe := awaitEvent(t, done, "the probe's verdict once the client left"); probe.IsMySQL {
				t.Fatalf("reason %q: another port of this machine was served as MySQL", probe.Reason)
			}
			if n := client.clocksUsed(); n != 0 {
				t.Fatalf("the probe waited on %d clock(s)", n)
			}
			var miss models.ParserError
			select {
			case err := <-p.errChannel:
				pe, ok := err.(models.ParserError)
				if !ok || pe.MismatchReport == nil {
					t.Fatalf("reported %v, want a miss with a report", err)
				}
				miss = pe
			default:
				t.Fatal("no miss was reported for the connection that waited to be greeted")
			}
			if phase := miss.MismatchReport.MatchPhase; phase != models.MatchPhaseNoRequest {
				t.Fatalf("the miss's phase is %q; want %q: no call was made, so it fails no test by itself", phase, models.MatchPhaseNoRequest)
			}
			if next := miss.MismatchReport.NextSteps; !strings.Contains(next, "add 3307 to mysqlPorts") || !strings.Contains(next, "localhost") {
				t.Fatalf("the miss's next step is %q; want it to say to add 3307 to mysqlPorts, for localhost", next)
			}

			listed, err := p.probeMysql(context.Background(), newSilentClient(), tc.live, nil, 3307,
				models.MODE_TEST, models.OutgoingOptions{MysqlPorts: []uint32{3307}}, zap.NewNop())
			if err != nil || !listed.IsMySQL || listed.Reason != "configured-port" {
				t.Fatalf("with 3307 in mysqlPorts: IsMySQL=%v reason %q err %v, want true/configured-port", listed.IsMySQL, listed.Reason, err)
			}
		})
	}
}

// The Docker host is shared like this machine: an application in a container
// reaches every service on the host at its network's gateway (172.17.0.1 on
// Docker's default bridge, where host.docker.internal and host-gateway point
// too), so another port of it is not taken for the recorded MySQL there; nor
// is another port of an address of this machine's own. The client is read
// instead, and when it gives up, the miss names the port to list and the host
// it is on. A listed port is MySQL.
func TestReplayNeverTakesAnotherPortOfTheDockerHostForTheRecordedMySQL(t *testing.T) {
	gateway := netip.MustParseAddr("172.17.0.1")
	gateway6 := netip.MustParseAddr("fde2:5ad0:1727::1")
	lan := netip.MustParseAddr("192.168.1.10")
	for _, tc := range []struct {
		name, recorded, live string
		docker               bool
		own, gateways        []netip.Addr
		host                 string // the next step names it
	}{
		{"the container's gateway", "172.17.0.1:3306", "172.17.0.1:3307", true, nil, []netip.Addr{gateway}, "the Docker host (172.17.0.1, the container's gateway)"},
		{"the gateway written IPv4-mapped", "[::ffff:172.17.0.1]:3306", "[172.17.0.1]:3307", true, nil, []netip.Addr{gateway}, "the Docker host (172.17.0.1, the container's gateway)"},
		{"an IPv6 gateway", "[fde2:5ad0:1727::1]:3306", "[fde2:5ad0:1727::1]:3307", true, nil, []netip.Addr{gateway6}, "the Docker host (fde2:5ad0:1727::1, the container's gateway)"},
		{"natively, the Docker bridge's address is this machine's", "172.17.0.1:3306", "172.17.0.1:3307", false, []netip.Addr{lan, gateway}, nil, "this machine (172.17.0.1, an address of its own)"},
		{"an address of this machine's own", "192.168.1.10:3306", "192.168.1.10:3307", false, []netip.Addr{lan}, nil, "this machine (192.168.1.10, an address of its own)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy(t)
			p.IsDocker = tc.docker
			p.readNetns = func() (own, gateways []netip.Addr) { return tc.own, tc.gateways }
			p.errChannel = make(chan error, 4)
			recordedMySQL(p, tc.recorded, "")
			client := newSilentClient()
			done := replayProbe(context.Background(), p, client, tc.live, zap.NewNop())
			if probe := readOrVerdict(t, done, client); probe != nil {
				t.Fatalf("IsMySQL=%v reason %q: another port of %s was judged without the client", probe.IsMySQL, probe.Reason, tc.live)
			}
			_ = client.Close() // the application gave up waiting for a greeting
			if probe := awaitEvent(t, done, "the probe's verdict once the client left"); probe.IsMySQL {
				t.Fatalf("reason %q: another port of a shared host was served as MySQL", probe.Reason)
			}
			if n := client.clocksUsed(); n != 0 {
				t.Fatalf("the probe waited on %d clock(s)", n)
			}
			var miss models.ParserError
			select {
			case err := <-p.errChannel:
				pe, ok := err.(models.ParserError)
				if !ok || pe.MismatchReport == nil {
					t.Fatalf("reported %v, want a miss with a report", err)
				}
				miss = pe
			default:
				t.Fatal("no miss was reported for the connection that waited to be greeted")
			}
			if next := miss.MismatchReport.NextSteps; !strings.Contains(next, "add 3307 to mysqlPorts") || !strings.Contains(next, "another port of "+tc.host+" is never taken") {
				t.Fatalf("the miss's next step is %q; want it to say to add 3307 to mysqlPorts, naming %s", next, tc.host)
			}

			listed, err := p.probeMysql(context.Background(), newSilentClient(), tc.live, nil, 3307,
				models.MODE_TEST, models.OutgoingOptions{MysqlPorts: []uint32{3307}}, zap.NewNop())
			if err != nil || !listed.IsMySQL || listed.Reason != "configured-port" {
				t.Fatalf("with 3307 in mysqlPorts: IsMySQL=%v reason %q err %v, want true/configured-port", listed.IsMySQL, listed.Reason, err)
			}
		})
	}
}

// A gateway is the Docker host only for an application in a container: a
// native application's default gateway is a router, another machine, and a
// recorded MySQL host there, or any other recorded host that is no gateway of
// the container's, still matches with its port moved.
func TestReplayStillTakesAMovedPortOnARecordedHostThatIsNoSharedOne(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, live string
		docker               bool
		gateways             []netip.Addr
	}{
		{"natively, the default gateway", "192.168.1.1:3306", "192.168.1.1:3307", false, []netip.Addr{netip.MustParseAddr("192.168.1.1")}},
		{"in a container, a host other than its gateway", "10.1.2.3:3306", "10.1.2.3:3307", true, []netip.Addr{netip.MustParseAddr("172.17.0.1")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy(t)
			p.IsDocker = tc.docker
			p.readNetns = func() (own, gateways []netip.Addr) { return nil, tc.gateways }
			recordedMySQL(p, tc.recorded, "")
			probe := verdict(t, p, newSilentClient(), tc.live)
			if !probe.IsMySQL || probe.Reason != "recorded-endpoint-moved" {
				t.Fatalf("IsMySQL=%v reason %q, want true/recorded-endpoint-moved", probe.IsMySQL, probe.Reason)
			}
		})
	}
}

// Under container keys, the container the recording reached MySQL at is MySQL
// at whatever address it is reached now (a random published port), and a
// container that is not it is not MySQL, even on the recorded host: the
// container's key decides over the address.
func TestReplayJudgesAContainerDestinationByItsKey(t *testing.T) {
	const recordedKey = "container:mysql:3306@0123456789ab"
	index := &sessionIndex{at: map[string]containerkey.Container{
		"127.0.0.1:49153": {ID: "m1", Image: "mysql:8.4", Port: 3306, Fingerprint: "0123456789ab"},
		"172.17.0.5:6379": {ID: "r1", Image: "redis:7", Port: 6379, Fingerprint: "fedcba987654"},
		"172.17.0.5:3307": {ID: "u1", Image: "mysql:8.4", Port: 3306}, // configuration unread
	}}
	keyer := containerkey.NewKeyer(index)

	for _, tc := range []struct {
		name, dst string
		mysql     bool
	}{
		{"the recorded container on another port", "127.0.0.1:49153", true},
		{"another container on the recorded host", "172.17.0.5:6379", false},
		{"a container whose configuration cannot be read, on the recorded host", "172.17.0.5:3307", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy(t)
			recordedMySQL(p, "172.17.0.5:3306", recordedKey)
			connID := "c-" + tc.dst
			keyer.Name(nil, connID, tc.dst)
			defer keyer.Forget(connID)
			ctx, cancel := context.WithCancel(containerkey.WithConn(context.Background(), keyer, connID))
			defer cancel()
			client := newSilentClient()

			done := replayProbe(ctx, p, client, tc.dst, zap.NewNop())
			probe := readOrVerdict(t, done, client)
			if probe == nil {
				cancel()
				probe = awaitEvent(t, done, "the probe returning once the run ended")
			}
			if probe.IsMySQL != tc.mysql {
				t.Fatalf("IsMySQL=%v reason %q, want %v", probe.IsMySQL, probe.Reason, tc.mysql)
			}
			if n := client.clocksUsed(); n != 0 {
				t.Fatalf("the probe waited on %d clock(s)", n)
			}
		})
	}
}

// A moved endpoint keploy cannot tie to the recording is not served; when its
// client ends without having said anything — it was waiting to be greeted —
// keploy says which port to add to mysqlPorts.
func TestReplayTellsWhichPortToListForAnUnlistedMovedEndpoint(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	p := testProxy(t)
	p.errChannel = make(chan error, 4)
	recordedMySQL(p, "127.0.0.1:3306", "")
	client := newSilentClient()

	done := replayProbe(context.Background(), p, client, "10.9.9.9:13306", zap.New(core))
	if probe := readOrVerdict(t, done, client); probe != nil {
		t.Fatalf("the probe decided (IsMySQL=%v, reason %q) before the client did anything", probe.IsMySQL, probe.Reason)
	}
	_ = client.Close() // the application gave up waiting for a greeting
	probe := awaitEvent(t, done, "the probe's verdict once the client left")
	if probe.IsMySQL {
		t.Fatalf("reason %q: an endpoint the recording cannot be tied to was served as MySQL", probe.Reason)
	}
	var said bool
	for _, e := range logs.All() {
		for _, f := range e.Context {
			if f.Key == "next_step" && strings.Contains(f.String, "mysqlPorts") && strings.Contains(f.String, "13306") {
				said = true
			}
		}
	}
	if !said {
		t.Fatalf("no warning named port 13306 for mysqlPorts; logged: %v", logs.All())
	}
	select {
	case err := <-p.errChannel:
		pe, ok := err.(models.ParserError)
		if !ok || pe.MismatchReport == nil || !strings.Contains(pe.MismatchReport.NextSteps, "add 13306 to mysqlPorts") || pe.MismatchReport.MatchPhase != models.MatchPhaseNoRequest {
			t.Fatalf("reported %v; want a no_request miss whose next step says to add 13306 to mysqlPorts", err)
		}
	default:
		t.Fatal("no miss was reported for the connection that waited to be greeted")
	}
}

// disableMysqlEndpointDrift turns the identity inference off: a moved port on
// the recorded host is then not served as MySQL.
func TestReplayEndpointDriftOffServesNoMovedEndpoint(t *testing.T) {
	p := testProxy(t)
	recordedMySQL(p, "10.1.2.3:3306", "")
	client := newSilentClient()
	port, _ := portFromAddr("10.1.2.3:3307")
	probe, err := p.probeMysql(context.Background(), client, "10.1.2.3:3307", nil, port,
		models.MODE_TEST, models.OutgoingOptions{DisableMysqlEndpointDrift: true}, zap.NewNop())
	if err != nil || probe.IsMySQL || probe.Reason != "no-mysql-mock-for-port" {
		t.Fatalf("IsMySQL=%v reason %q err %v, want false/no-mysql-mock-for-port", probe.IsMySQL, probe.Reason, err)
	}
}

// A client of a destination the recording cannot be tied to that speaks is
// handed on with its bytes, and the port is not marked as not MySQL: each
// connection to it is judged by itself, so a later silent one still gets its
// next step.
func TestReplayClientOfAnUntiedDestinationIsHandedOnWhenItSpeaks(t *testing.T) {
	p := testProxy(t)
	recordedMySQL(p, "127.0.0.1:3306", "")
	app, srcConn := clientPair(t)
	done := replayProbe(context.Background(), p, srcConn, "172.20.0.2:283", zap.NewNop())
	hello := []byte{0x16, 0x03, 0x01, 0x00, 0x2f}
	if _, err := app.Write(hello); err != nil {
		t.Fatal(err)
	}
	probe := awaitEvent(t, done, "the probe's verdict once the client spoke")
	if probe.IsMySQL || probe.Reason != "client-spoke-first" {
		t.Fatalf("IsMySQL=%v reason %q, want false/client-spoke-first", probe.IsMySQL, probe.Reason)
	}
	got := make([]byte, len(hello))
	if _, err := io.ReadFull(probe.SrcConn, got); err != nil || string(got) != string(hello) {
		t.Fatalf("the client's bytes read back as %x, %v", got, err)
	}
	if p.mysqlPorts.IsKnownNotMysql(283) {
		t.Fatal("the port was marked not MySQL: the next connection to it would skip its judgement and its next step")
	}
}

// The next step for an untied endpoint is said once per endpoint, also when
// the run ends with its client still silent.
func TestReplayUnlistedEndpointNextStepIsSaidOncePerEndpoint(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	p := testProxy(t)
	p.errChannel = make(chan error, 4)
	recordedMySQL(p, "127.0.0.1:3306", "")
	for i := 0; i < 2; i++ {
		client := newSilentClient()
		ctx, cancel := context.WithCancel(context.Background())
		done := replayProbe(ctx, p, client, "10.9.9.9:13306", zap.New(core))
		if probe := readOrVerdict(t, done, client); probe != nil {
			t.Fatalf("IsMySQL=%v reason %q before the client did anything", probe.IsMySQL, probe.Reason)
		}
		cancel() // the run ends with the client silent
		if probe := awaitEvent(t, done, "the probe returning"); probe.IsMySQL || probe.Reason != "cancelled" {
			t.Fatalf("IsMySQL=%v reason %q, want false/cancelled", probe.IsMySQL, probe.Reason)
		}
	}
	if n := logs.FilterField(zap.String("dstAddr", "10.9.9.9:13306")).Len(); n != 1 {
		t.Fatalf("the next step was said %d times for one endpoint; want once", n)
	}
	select {
	case err := <-p.errChannel:
		t.Fatalf("a miss was reported for a client still silent when the run ended: %v", err)
	default:
	}
}

// A new recording is judged by its own MySQL destinations: the swap drops the
// previous recording's hosts and keys with its ports.
func TestSessionSwapReplacesTheRecordedMySQLDestinations(t *testing.T) {
	p := testProxy(t)
	recordedMySQL(p, "10.2.2.2:3306", "")
	p.mysqlPorts.MarkSessionStale()
	recordedMySQL(p, "10.1.1.1:3306", "")

	client := newSilentClient()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := replayProbe(ctx, p, client, "10.2.2.2:3307", zap.NewNop())
	if probe := readOrVerdict(t, done, client); probe != nil {
		t.Fatalf("IsMySQL=%v reason %q: the previous recording's host still matched", probe.IsMySQL, probe.Reason)
	}
	cancel()
	awaitEvent(t, done, "the probe returning")

	probe := verdict(t, p, newSilentClient(), "10.1.1.1:3307")
	if !probe.IsMySQL || probe.Reason != "recorded-endpoint-moved" {
		t.Fatalf("IsMySQL=%v reason %q: the new recording's host must match", probe.IsMySQL, probe.Reason)
	}
}

// A host compares as the proxy writes it: a dual-stack socket's connection to
// an IPv4 address as [a.b.c.d]:p (Java's default), or IPv4-mapped.
func TestReplayMovedPortMatchesTheRecordedHostHoweverItIsWritten(t *testing.T) {
	for _, tc := range []struct{ recorded, live string }{
		{"[10.1.2.3]:3306", "[10.1.2.3]:3307"},
		{"10.1.2.3:3306", "[10.1.2.3]:3307"},
		{"[10.1.2.3]:3306", "10.1.2.3:3307"},
		{"[::ffff:10.1.2.3]:3306", "10.1.2.3:3307"},
		{"[::ffff:10.1.2.3]:3306", "[10.1.2.3]:13306"},
	} {
		t.Run(tc.recorded+" to "+tc.live, func(t *testing.T) {
			p := testProxy(t)
			recordedMySQL(p, tc.recorded, "")
			probe := verdict(t, p, newSilentClient(), tc.live)
			if !probe.IsMySQL || probe.Reason != "recorded-endpoint-moved" {
				t.Fatalf("IsMySQL=%v reason %q, want true/recorded-endpoint-moved", probe.IsMySQL, probe.Reason)
			}
		})
	}
}

// A host the recording reached a MySQL container on says nothing about a
// host-native service on it, where the replay names containers; where it does
// not (no keyer), the address is all there is, and it matches.
func TestReplayContainerRecordedHostMatchesOnlyWithoutContainerNames(t *testing.T) {
	const recordedKey = "container:mysql:3306@0123456789ab"
	t.Run("the replay names containers", func(t *testing.T) {
		keyer := containerkey.NewKeyer(&sessionIndex{at: map[string]containerkey.Container{}})
		p := testProxy(t)
		recordedMySQL(p, "10.0.0.5:49153", recordedKey)
		keyer.Name(nil, "native", "10.0.0.5:6379")
		defer keyer.Forget("native")
		ctx, cancel := context.WithCancel(containerkey.WithConn(context.Background(), keyer, "native"))
		defer cancel()
		client := newSilentClient()
		done := replayProbe(ctx, p, client, "10.0.0.5:6379", zap.NewNop())
		if probe := readOrVerdict(t, done, client); probe != nil {
			t.Fatalf("IsMySQL=%v reason %q: a host-native service on a host a MySQL container was recorded on is not that container", probe.IsMySQL, probe.Reason)
		}
		cancel()
		awaitEvent(t, done, "the probe returning")
	})
	t.Run("the replay does not name containers", func(t *testing.T) {
		p := testProxy(t)
		recordedMySQL(p, "10.0.0.5:49153", recordedKey)
		probe := verdict(t, p, newSilentClient(), "10.0.0.5:6379")
		if !probe.IsMySQL {
			t.Fatalf("reason %q: without container names the recorded host is all there is", probe.Reason)
		}
	})
}

// wedgedIndex is a container index whose read of a container's configuration
// never ends.
type wedgedIndex struct{ sessionIndex }

func (*wedgedIndex) Fingerprint(ctx context.Context, _ string) (string, bool) {
	<-ctx.Done()
	return "", false
}

// When no recorded MySQL mock carries a container key, no key can decide, so
// the probe does not wait for a container's configuration (which Docker can
// hold): whether the destination is a container is enough.
func TestReplayDoesNotWaitForAKeyNoRecordingCanMatch(t *testing.T) {
	index := &wedgedIndex{sessionIndex{at: map[string]containerkey.Container{
		"127.0.0.1:49153": {ID: "m1", Image: "mysql:8.4", Port: 3306}, // configuration unread
	}}}
	keyer := containerkey.NewKeyer(index)
	p := testProxy(t)
	recordedMySQL(p, "10.0.0.1:3306", "")
	keyer.Name(nil, "c1", "127.0.0.1:49153")
	defer keyer.Forget("c1")
	ctx, cancel := context.WithCancel(containerkey.WithConn(context.Background(), keyer, "c1"))
	defer cancel()
	client := newSilentClient()
	done := replayProbe(ctx, p, client, "127.0.0.1:49153", zap.NewNop())
	if probe := readOrVerdict(t, done, client); probe != nil {
		t.Fatalf("IsMySQL=%v reason %q before the client did anything", probe.IsMySQL, probe.Reason)
	}
	cancel()
	if probe := awaitEvent(t, done, "the probe returning"); probe.IsMySQL {
		t.Fatalf("reason %q: a container no recorded key names is not MySQL", probe.Reason)
	}
}

// A new recording's container keys replace the previous recording's.
func TestSessionSwapReplacesTheRecordedMySQLContainerKeys(t *testing.T) {
	index := &sessionIndex{at: map[string]containerkey.Container{
		"127.0.0.1:49153": {ID: "m1", Image: "mysql:8.4", Port: 3306, Fingerprint: "0123456789ab"},
	}}
	keyer := containerkey.NewKeyer(index)
	p := testProxy(t)
	recordedMySQL(p, "172.17.0.5:3306", "container:mysql:3306@0123456789ab")
	p.mysqlPorts.MarkSessionStale()
	recordedMySQL(p, "172.17.0.6:3306", "container:mysql:3306@fedcba987654")
	keyer.Name(nil, "c1", "127.0.0.1:49153")
	defer keyer.Forget("c1")
	ctx, cancel := context.WithCancel(containerkey.WithConn(context.Background(), keyer, "c1"))
	defer cancel()
	client := newSilentClient()
	done := replayProbe(ctx, p, client, "127.0.0.1:49153", zap.NewNop())
	if probe := readOrVerdict(t, done, client); probe != nil {
		t.Fatalf("IsMySQL=%v reason %q: the previous recording's container key still matched", probe.IsMySQL, probe.Reason)
	}
	cancel()
	awaitEvent(t, done, "the probe returning")
}

// A recording made without container names knows its MySQL only by its
// address, so under the switch a container on the recorded host with the port
// moved is served, as it is without the switch.
func TestReplayKeylessRecordingMatchesAContainerOnTheRecordedHost(t *testing.T) {
	keyer := containerkey.NewKeyer(&sessionIndex{at: map[string]containerkey.Container{
		"10.0.0.5:49200": {ID: "m2", Image: "mysql:8.4", Port: 3306, Fingerprint: "0123456789ab"},
	}})
	p := testProxy(t)
	recordedMySQL(p, "10.0.0.5:49153", "")
	keyer.Name(nil, "c1", "10.0.0.5:49200")
	defer keyer.Forget("c1")
	ctx, cancel := context.WithCancel(containerkey.WithConn(context.Background(), keyer, "c1"))
	defer cancel()
	client := newSilentClient()
	done := replayProbe(ctx, p, client, "10.0.0.5:49200", zap.NewNop())
	probe := readOrVerdict(t, done, client)
	if probe == nil {
		cancel()
		awaitEvent(t, done, "the probe returning")
		t.Fatal("a keyless recording's MySQL on a new port of the recorded host was not served under the switch; without it, it is")
	}
	if !probe.IsMySQL || probe.Reason != "recorded-endpoint-moved" {
		t.Fatalf("IsMySQL=%v reason %q, want true/recorded-endpoint-moved", probe.IsMySQL, probe.Reason)
	}
}

// Whether a recording was made naming containers is its whole test set's (the
// keyer's tally, SetRecorded), not what the mocks delivered so far say: with a
// stamp on another dependency's mock, a container on the host the recording
// reached a host-native MySQL on is judged by its key alone, from the first
// connection on; the next test set, made without container names, is judged
// by its address again.
func TestReplayRecordingNamedForContainersJudgesAContainerByItsKeyAlone(t *testing.T) {
	keyer := containerkey.NewKeyer(&sessionIndex{at: map[string]containerkey.Container{
		"10.0.0.5:49200": {ID: "r2", Image: "redis:7", Port: 6379, Fingerprint: "fedcba987654"},
	}})
	judge := func(p *Proxy, connID string) *mysqlProbe {
		keyer.Name(nil, connID, "10.0.0.5:49200")
		defer keyer.Forget(connID)
		ctx, cancel := context.WithCancel(containerkey.WithConn(context.Background(), keyer, connID))
		defer cancel()
		client := newSilentClient()
		done := replayProbe(ctx, p, client, "10.0.0.5:49200", zap.NewNop())
		if probe := readOrVerdict(t, done, client); probe != nil {
			return probe
		}
		cancel()
		return awaitEvent(t, done, "the probe returning")
	}

	var named containerkey.RecordedTally
	named.Add(&models.Mock{Kind: models.REDIS, Spec: models.MockSpec{Metadata: map[string]string{
		containerkey.MetadataKey: "container:redis:6379@fedcba987654",
		containerkey.InstanceKey: "instance-of-redis",
	}}})
	keyer.SetRecorded(&named)
	p := testProxy(t)
	recordedMySQL(p, "10.0.0.5:3306", "") // the only mock delivered: host-native MySQL
	if probe := judge(p, "c1"); probe.IsMySQL {
		t.Fatalf("reason %q: a recording made naming containers matched another container on its MySQL's host by its address", probe.Reason)
	}

	keyer.SetRecorded(&containerkey.RecordedTally{})
	p.mysqlPorts.MarkSessionStale()
	recordedMySQL(p, "10.0.0.5:3306", "")
	if probe := judge(p, "c2"); !probe.IsMySQL || probe.Reason != "recorded-endpoint-moved" {
		t.Fatalf("IsMySQL=%v reason %q: the previous test set's stamp outlived its replacement", probe.IsMySQL, probe.Reason)
	}
}

// Whether a recorded host is shared is read from the network namespace: it is
// asked outside the registry's lock, so no connection's look at the registry
// waits on that read.
func TestASharedHostIsAskedOutsideTheRegistrysLock(t *testing.T) {
	p := testProxy(t)
	recordedMySQL(p, "10.1.2.3:3306", "")
	asked := 0
	why, ok := p.mysqlPorts.recordedEndpointMoved(netip.MustParseAddr("10.1.2.3"), "", false, false, false, func(netip.Addr) bool {
		asked++
		if !p.mysqlPorts.mu.TryLock() {
			t.Error("the shared host was asked with the registry's lock held")
		} else {
			p.mysqlPorts.mu.Unlock()
		}
		return true
	})
	if ok || asked != 1 {
		t.Fatalf("matched %v (%q), the shared host asked %d time(s); want no match, asked once", ok, why, asked)
	}
}
