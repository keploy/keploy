package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	"go.keploy.io/server/v3/pkg/agent/proxy/internal/testnet"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// smtpServer is a server-first protocol keploy has no parser for: it greets
// at once, then answers each line; to QUIT it says goodbye and closes.
// served says it answered a line.
func smtpServer(served chan<- string) func(c net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		if _, err := io.WriteString(c, "220 mail.example ESMTP ready\r\n"); err != nil {
			return
		}
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if strings.TrimSpace(line) == "QUIT" {
				_, _ = io.WriteString(c, "221 bye\r\n")
				return
			}
			select {
			case served <- strings.TrimSpace(line):
			default:
			}
			if _, err := io.WriteString(c, "250 ok "+line); err != nil {
				return
			}
		}
	}
}

// recordingProxy is a proxy recording, as an agent's, with the MySQL parser
// the probe reads a greeting with.
func recordingProxy(t *testing.T, addr string, logger *zap.Logger) *Proxy {
	t.Helper()
	cfg := config.New()
	cfg.Agent.Mode = models.MODE_RECORD
	p := New(logger, fakeDestInfo{dest: ipv4Dest(t, addr)}, cfg)
	p.Integrations = map[integrations.IntegrationType]integrations.Integrations{
		integrations.MYSQL: mysql.New(zap.NewNop()),
	}
	if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	return p
}

// to points p's next connections at addr.
func (p *Proxy) to(t *testing.T, addr string) {
	t.Helper()
	p.DestInfo = fakeDestInfo{dest: ipv4Dest(t, addr)}
}

const notRecorded = ": a server-first protocol; passed through, not recorded"

// serverFirstLoss is what p counted of server_first losses.
func serverFirstLoss(p *Proxy) models.CaptureLoss {
	for _, c := range p.CaptureLosses() {
		if c.Kind == models.CaptureLossServerFirst {
			return c
		}
	}
	return models.CaptureLoss{}
}

// P250: under record, a server-first protocol keploy has no parser for
// (SMTP, FTP, POP3: the server greets, the client waits for it) is relayed
// both ways from the greeting, so the application's call is made. It hung:
// the probe had read the greeting, and dispatch then waited for the client's
// first bytes, which the client sends only once greeted. With the switch off
// and on alike, the application gets the greeting and its answers, over the
// one connection to the destination the probe opened, and keploy says once
// per destination in a recording session that it cannot record it, with the
// next step the switch gives. Under the switch each piece of the
// application's data is a capture loss of a call keploy cannot record
// (CaptureLossServerFirst), which leaves its test not judged; the greeting
// charges nothing. Without the switch nothing is counted.
func TestAServerFirstProtocolIsRelayedFromItsGreeting(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			served := make(chan string, 8)
			addr, conns, _ := acceptingServer(t, smtpServer(served))
			core, logs := observer.New(zap.WarnLevel)
			p := recordingProxy(t, addr, zap.New(core))
			for i := 0; i < 2; i++ {
				callServerFirst(t, p, served, false)
			}
			if n := conns.Load(); n != 2 {
				t.Fatalf("the destination got %d connections for 2 calls; want the probe's own, one each", n)
			}
			said := logs.FilterMessage("keploy cannot record " + addr + notRecorded).All()
			if len(said) != 1 {
				t.Fatalf("said %d times that keploy cannot record the destination, want once", len(said))
			}
			next := fmt.Sprint(said[0].ContextMap()["next_step"])
			if want := map[string]string{"": "bypassRules", "1": "mock.real"}[proven]; !strings.Contains(next, want) {
				t.Fatalf("next_step %q; want it to name %s, what reaches it by design with the switch %q", next, want, proven)
			}
			counted := serverFirstLoss(p)
			if proven == "" {
				if counted.Count != 0 {
					t.Fatalf("without the switch a loss was counted: %+v", counted)
				}
			} else if counted.Count != 2 || counted.What != "to "+addr {
				t.Fatalf("under the switch the loss is %+v; want the application's 2 sends counted, to %s, and not the greetings", counted, addr)
			}

			// A new recording session says it again.
			if err := p.Record(context.Background(), make(chan *models.Mock, 16), models.OutgoingOptions{}); err != nil {
				t.Fatal(err)
			}
			callServerFirst(t, p, served, false)
			if n := len(logs.FilterMessage("keploy cannot record " + addr + notRecorded).All()); n != 2 {
				t.Fatalf("said %d times over two sessions, want once in each", n)
			}
		})
	}
}

// callServerFirst is the application's call over the server-first protocol
// through p: greeted, it says EHLO and gets the answer, then closes, or, with
// quit, says QUIT and reads the destination's goodbye and its close.
func callServerFirst(t *testing.T, p *Proxy, served <-chan string, quit bool) {
	t.Helper()
	app, src := acceptedConn(t)
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	// The application's own deadline, only so that the test
	// reports a hang instead of being one.
	_ = app.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(app)
	greeting, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(greeting, "220 ") {
		t.Fatalf("the application got %q, %v; want the server's greeting (the hang: keploy held it)", greeting, err)
	}
	if _, err := io.WriteString(app, "EHLO app\r\n"); err != nil {
		t.Fatal(err)
	}
	answer, err := r.ReadString('\n')
	if err != nil || answer != "250 ok EHLO app\r\n" {
		t.Fatalf("the application got %q, %v; want the server's answer", answer, err)
	}
	if served != nil {
		if got := <-served; got != "EHLO app" {
			t.Fatalf("the server got %q", got)
		}
	}
	if quit {
		if _, err := io.WriteString(app, "QUIT\r\n"); err != nil {
			t.Fatal(err)
		}
		if bye, err := r.ReadString('\n'); err != nil || bye != "221 bye\r\n" {
			t.Fatalf("the application got %q, %v; want the goodbye", bye, err)
		}
		if rest, err := r.ReadString('\n'); err != io.EOF || rest != "" {
			t.Fatalf("after the goodbye the application got %q, %v; want the destination's close", rest, err)
		}
	}
	// The application closes its end, as a client does once done (or once
	// its server closed).
	_ = app.Close()
	awaitEvent(t, done, "the connection's handling ending once an end closed")
}

// The relay also serves: the destination closing (its goodbye, then EOF to
// the application); the connection the application's handshake was held on
// (the probe looks at it and hands it back, and the relay takes it, the one
// connection the destination sees); two connections at once (the second,
// its port learned, relayed as the destination was marked); the run's
// context ending; and a client-first destination on the same port is
// dispatched as before.
func TestAServerFirstRelayServesEveryWayAConnectionGoes(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	served := make(chan string, 8)
	addr, conns, _ := acceptingServer(t, smtpServer(served))
	p := recordingProxy(t, addr, zap.NewNop())

	t.Run("the destination closes", func(t *testing.T) {
		callServerFirst(t, p, served, true)
	})

	t.Run("the held connection", func(t *testing.T) {
		before := conns.Load()
		app, src := acceptedConn(t)
		held := predialedTo(t, p, src, addr)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		_ = app.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(app)
		if greeting, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(greeting, "220 ") {
			t.Fatalf("the application got %q, %v; want the greeting over the held connection", greeting, err)
		}
		if _, err := io.WriteString(app, "EHLO held\r\n"); err != nil {
			t.Fatal(err)
		}
		if answer, err := r.ReadString('\n'); err != nil || answer != "250 ok EHLO held\r\n" {
			t.Fatalf("the application got %q, %v", answer, err)
		}
		if got := <-served; got != "EHLO held" {
			t.Fatalf("the server got %q", got)
		}
		_ = app.Close()
		awaitEvent(t, done, "the handling ending")
		if n := conns.Load() - before; n != 1 {
			t.Fatalf("the destination got %d connections; want the held one alone", n)
		}
		if held.closes.Load() == 0 {
			t.Fatal("the held connection was not the one relayed (it is still open)")
		}
	})

	t.Run("two connections at once", func(t *testing.T) {
		// Both are greeted before either says a word: each was looked
		// at, and relayed, on its own.
		var greeted sync.WaitGroup
		apps := make([]*bufio.Reader, 2)
		ends := make([]net.Conn, 2)
		done := make(chan error, 2)
		for i := range apps {
			app, src := acceptedConn(t)
			_ = app.SetDeadline(time.Now().Add(10 * time.Second))
			go func() { done <- p.handleConnection(context.Background(), src) }()
			apps[i], ends[i] = bufio.NewReader(app), app
			greeted.Add(1)
		}
		errs := make(chan error, 2)
		for _, r := range apps {
			go func() {
				defer greeted.Done()
				if g, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(g, "220 ") {
					errs <- fmt.Errorf("got %q, %v; want the greeting", g, err)
				}
			}()
		}
		greeted.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		for _, c := range ends {
			_ = c.Close()
		}
		awaitEvent(t, done, "the first handling ending")
		awaitEvent(t, done, "the second handling ending")
	})

	port := addr[strings.LastIndex(addr, ":")+1:]
	t.Run("the run's context ends", func(t *testing.T) {
		app, src := acceptedConn(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(ctx, src) }()
		_ = app.SetDeadline(time.Now().Add(10 * time.Second))
		if greeting, err := bufio.NewReader(app).ReadString('\n'); err != nil || !strings.HasPrefix(greeting, "220 ") {
			t.Fatalf("the application got %q, %v", greeting, err)
		}
		cancel()
		awaitEvent(t, done, "the relay ending with the run's context")
	})

	// The port is learned not to be MySQL, as it always was, so the probe
	// is skipped for another destination on it: a client-first one is
	// dispatched as before (and a server-first one would hang as before
	// P250, a known limit).
	t.Run("a client-first destination on the same port", func(t *testing.T) {
		clientFirst := listenAt(t, "127.0.0.3:"+port, silentUntilAsked)
		p.to(t, clientFirst)
		t.Cleanup(func() { p.to(t, addr) })
		app, src := acceptedConn(t)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		_ = app.SetDeadline(time.Now().Add(10 * time.Second))
		if _, err := io.WriteString(app, "hello\n"); err != nil {
			t.Fatal(err)
		}
		if got, err := bufio.NewReader(app).ReadString('\n'); err != nil || got != "ok hello\n" {
			t.Fatalf("the client-first destination's answer %q, %v; want it dispatched as before", got, err)
		}
		_ = app.Close()
		awaitEvent(t, done, "the handling ending")
	})
}

// listenAt serves serve at addr, a loopback address of another port's
// choosing; the test is skipped where the address cannot be bound.
func listenAt(t *testing.T, addr string, serve func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Skipf("cannot listen at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serve(c)
		}
	}()
	return ln.Addr().String()
}

// h2Settings is an HTTP/2 server's connection preface, a SETTINGS frame of
// one setting (MAX_CONCURRENT_STREAMS 100), as grpc-go and net/http's h2c
// write it at accept, before they read their client's.
var h2Settings = []byte{0, 0, 6, 0x04, 0, 0, 0, 0, 0, 0, 0x03, 0, 0, 0, 100}

// What a client-first protocol's server may send at accept is no
// server-first greeting: its client speaks without waiting, so it is
// dispatched on the client's bytes, as before (an HTTP/2 server's SETTINGS,
// an HTTP/1 answer, SSH's version line). A server-first protocol's greeting
// is one (SMTP, POP3, IMAP, NATS, FTP). A refusal at accept is neither.
func TestAClientFirstProtocolsServerPreambleIsNoServerFirstGreeting(t *testing.T) {
	for _, g := range [][]byte{
		h2Settings, h2Settings[:4], h2Settings[:9], {0, 0, 0, 0x04, 0, 0, 0, 0, 0},
		[]byte("HTTP/1.1 408 Request Timeout\r\n"), []byte("HTTP"),
		[]byte("SSH-2.0-OpenSSH_9.6\r\n"),
	} {
		if !clientFirstPreamble(g) {
			t.Errorf("%q: want a client-first protocol's preamble", g)
		}
	}
	for _, g := range [][]byte{
		[]byte("220 mail.example ESMTP\r\n"), []byte("+OK POP3 ready\r\n"), []byte("* OK IMAP4rev1\r\n"),
		[]byte("INFO {\"server_id\":\"x\"}\r\n"), []byte("220 FTP ready\r\n"),
		{0, 0, 6, 0x04, 0x01, 0, 0, 0, 0}, // a SETTINGS acknowledgement is no preface
		{0, 0, 5, 0x04, 0, 0, 0, 0, 0},    // nor a payload of no whole setting
		{0, 0, 6, 0x04, 0, 0, 0, 0, 1},    // nor one on a stream
	} {
		if clientFirstPreamble(g) {
			t.Errorf("%q: want a server-first greeting", g)
		}
	}
	for _, g := range [][]byte{
		[]byte("-ERR max number of clients reached\r\n"), []byte("-DENIED Redis is running in protected mode\r\n"),
		[]byte("-ERR"), []byte("-ERR [SYS/TEMP] busy\r\n"),
		{0x17, 0, 0, 0, 0xff, 0x10, 0x04, '#', '0', '8', '0', '0', '4'}, // MySQL's Too many connections
	} {
		if !refusalAtAccept(g) {
			t.Errorf("%q: want a refusal at accept", g)
		}
	}
	for _, g := range [][]byte{[]byte("+OK POP3 ready\r\n"), []byte("-1\r\n"), []byte("- x"), []byte("220 ready\r\n")} {
		if refusalAtAccept(g) {
			t.Errorf("%q: want no refusal", g)
		}
	}
}

// Which greeting is relayed as a server-first protocol's, by the switch (the
// coordinator's ruling): without it, only the known server-first protocols'
// (SMTP and FTP 220, POP3 +OK, IMAP * OK, NATS INFO, VNC RFB), every other
// greeting dispatched as on main; under it, every greeting but a client-first
// protocol's preamble, an IRC server's notice and a refusal's not among them.
func TestWhichGreetingIsServerFirstFollowsTheSwitch(t *testing.T) {
	for _, c := range []struct {
		greeting string
		off, on  bool
	}{
		{"220 mail.example ESMTP\r\n", true, true},
		{"220-multi-line banner\r\n", true, true},
		{"+OK POP3 ready\r\n", true, true},
		{"* OK IMAP4rev1 ready\r\n", true, true},
		{"INFO {\"server_id\":\"x\"}\r\n", true, true},
		{"RFB 003.008\n", true, true},
		{":irc.example NOTICE * :Looking up your hostname\r\n", false, true},
		{"\xff\xfd\x18", false, true}, // a Telnet server's option negotiation
		{string(h2Settings), false, false},
		{"HTTP/1.1 408 Request Timeout\r\n", false, false},
		{"SSH-2.0-OpenSSH_9.6\r\n", false, false},
	} {
		t.Setenv(provenswitch.Env, "")
		if got := serverFirstGreeting([]byte(c.greeting)); got != c.off {
			t.Errorf("switch off: %q server-first %v, want %v", c.greeting, got, c.off)
		}
		t.Setenv(provenswitch.Env, "1")
		if got := serverFirstGreeting([]byte(c.greeting)); got != c.on {
			t.Errorf("switch on: %q server-first %v, want %v", c.greeting, got, c.on)
		}
	}
}

// Under the switch a destination that refuses the connection at accept (a
// POP3 -ERR, Redis past its maxclients) is relayed, so the application gets
// the refusal and the close, and its port learns nothing: a later connection
// to it, accepted, is probed afresh and served by what it speaks then.
// Without the switch a refusal is dispatched, and its port learned, as on
// main.
func TestARefusalAtAcceptIsRelayedAndTeachesThePortNothing(t *testing.T) {
	refuse := make(chan bool, 4)
	addr, _, _ := acceptingServer(t, func(c net.Conn) {
		defer c.Close()
		if <-refuse {
			_, _ = io.WriteString(c, "-ERR [SYS/TEMP] too busy\r\n")
			// What the client says anyway, read before the close.
			_, _ = bufio.NewReader(c).ReadString('\n')
			return
		}
		smtpServer(make(chan string, 4))(c)
	})
	port := uint32(mustPort(t, addr))

	t.Run("switch on", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		core, logs := observer.New(zap.WarnLevel)
		p := recordingProxy(t, addr, zap.New(core))
		refuse <- true
		app, src := acceptedConn(t)
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		_ = app.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(app)
		if got, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(got, "-ERR ") {
			t.Fatalf("the application got %q, %v; want the refusal", got, err)
		}
		if _, err := io.WriteString(app, "QUIT\r\n"); err != nil {
			t.Fatal(err)
		}
		if rest, err := r.ReadString('\n'); err != io.EOF || rest != "" {
			t.Fatalf("after the refusal the application got %q, %v; want the close", rest, err)
		}
		_ = app.Close()
		awaitEvent(t, done, "the handling ending")
		if p.mysqlPorts.IsKnownNotMysql(port) {
			t.Fatal("a refusal taught the port it is not MySQL")
		}
		if n := logs.FilterMessage("keploy passed through " + addr + "'s refusal at accept, not recorded").Len(); n != 1 {
			t.Fatalf("said the refusal %d times, want once", n)
		}
		// What the application sent on it is a refusal's loss, not a
		// server-first protocol's.
		var refused models.CaptureLoss
		for _, c := range p.CaptureLosses() {
			if c.Kind == models.CaptureLossRefusedAtAccept {
				refused = c
			}
		}
		if refused.Count != 1 || serverFirstLoss(p).Count != 0 {
			t.Fatalf("losses %+v; want the one send counted as a refusal's", p.CaptureLosses())
		}
		refuse <- false
		callServerFirst(t, p, nil, false)
	})

	t.Run("switch off", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "")
		p := testProxy(t)
		refuse <- true
		_, srcConn := clientPair(t)
		probe, err := p.probeMysql(context.Background(), srcConn, addr, nil, port, models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Predialed.CloseIfUnused()
		if probe.Reason != "greeting-not-mysql" || !p.mysqlPorts.IsKnownNotMysql(port) {
			t.Fatalf("reason %q, learned %v; want greeting-not-mysql and the port learned, as on main", probe.Reason, p.mysqlPorts.IsKnownNotMysql(port))
		}
	})
}

// An h2c (or gRPC) destination writes its SETTINGS at accept, before its
// client's preface is read: the probe may see it first. It is dispatched on
// the client's bytes, as it always was, with the switch off and on: never
// relayed as a server-first protocol, never said, never counted, and the
// port is learned not to be MySQL.
func TestAnH2cServersSettingsAtAcceptIsDispatchedAsBefore(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			wrote := make(chan struct{}, 4)
			addr, _, _ := acceptingServer(t, func(c net.Conn) {
				defer c.Close()
				if _, err := c.Write(h2Settings); err != nil {
					return
				}
				wrote <- struct{}{}
				_, _ = io.Copy(io.Discard, c)
			})
			p := testProxy(t)
			_, srcConn := clientPair(t)
			port := uint32(mustPort(t, addr))
			probe, err := p.probeMysql(context.Background(), srcConn, addr, nil, port, models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Predialed.CloseIfUnused()
			if probe.Reason != "greeting-not-mysql" || !p.mysqlPorts.IsKnownNotMysql(port) {
				t.Fatalf("reason %q, known not MySQL %v; want greeting-not-mysql and the port learned, as before", probe.Reason, p.mysqlPorts.IsKnownNotMysql(port))
			}
			<-wrote

			// Through handleConnection, the SETTINGS first: the held
			// connection has them before the probe looks, and the client
			// speaks only once the probe has decided (the port learned).
			core, logs := observer.New(zap.WarnLevel)
			rp := recordingProxy(t, addr, zap.New(core))
			app, src := acceptedConn(t)
			predialedTo(t, rp, src, addr)
			awaitEvent(t, wrote, "the destination's SETTINGS on the held connection")
			done := make(chan error, 1)
			go func() { done <- rp.handleConnection(context.Background(), src) }()
			decided := make(chan struct{})
			go func() {
				defer close(decided)
				for !rp.mysqlPorts.IsKnownNotMysql(port) {
					time.Sleep(time.Millisecond)
				}
			}()
			awaitEvent(t, decided, "the probe deciding on the SETTINGS")
			_ = app.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(app, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(h2Settings))
			if _, err := io.ReadFull(app, got); err != nil || string(got) != string(h2Settings) {
				t.Fatalf("the application got %x, %v; want the server's SETTINGS", got, err)
			}
			_ = app.Close()
			awaitEvent(t, done, "the handling ending")
			if n := len(logs.FilterMessageSnippet(notRecorded).All()); n != 0 {
				t.Fatalf("an h2c destination was said to be a server-first protocol %d time(s)", n)
			}
			if c := serverFirstLoss(rp); c.Count != 0 {
				t.Fatalf("an h2c destination counted as a server-first loss: %+v", c)
			}
		})
	}
}

// A destination marked as speaking a server-first protocol, its port
// learned not to be MySQL, is probed again on each record connection, not
// relayed blindly: the address may be another service's by now (a
// container's port reused, testcontainers' random ports). A client-first one
// there is dispatched as main's probe dispatches a port's first connection,
// with the switch off and on; one that still greets is relayed. A client
// speaking first is no evidence another service is there (an eager client
// of the same server-first destination does too), so the mark stays, and
// each connection is decided by its own probe.
func TestAMarkedDestinationThatIsAnotherServiceNowIsDispatchedAsOnMain(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			// On Linux the destination's port is held for the test's life
			// (testnet.SharedPort), so the test can listen there again.
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
			ln := testnet.ListenShared(t, "tcp4", addr)
			served := make(chan string, 8)
			serve := func(ln net.Listener, handle func(net.Conn)) <-chan struct{} {
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						c, err := ln.Accept()
						if err != nil {
							return
						}
						go handle(c)
					}
				}()
				return done
			}
			stopped := serve(ln, smtpServer(served))
			core, logs := observer.New(zap.WarnLevel)
			p := recordingProxy(t, addr, zap.New(core))
			callServerFirst(t, p, served, false)
			port := uint32(mustPort(t, addr))
			if !p.serverFirstDests.has(addr) || !p.mysqlPorts.IsKnownNotMysql(port) {
				t.Fatal("the SMTP destination was not marked, or its port not learned")
			}
			callServerFirst(t, p, served, false) // still greets: relayed, still marked
			if !p.serverFirstDests.has(addr) {
				t.Fatal("a destination that still greets lost its mark")
			}

			// The address is a client-first service's now.
			_ = ln.Close()
			<-stopped
			serve(testnet.ListenShared(t, "tcp4", addr), silentUntilAsked)
			app, src := acceptedConn(t)
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			_ = app.SetDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.WriteString(app, "hello\n"); err != nil {
				t.Fatal(err)
			}
			if got, err := bufio.NewReader(app).ReadString('\n'); err != nil || got != "ok hello\n" {
				t.Fatalf("the client-first service's answer %q, %v", got, err)
			}
			_ = app.Close()
			awaitEvent(t, done, "the handling ending")
			if n := logs.FilterMessage("keploy cannot record " + addr + notRecorded).Len(); n != 1 {
				t.Fatalf("said %d times it cannot record %s; the client-first service must not be said so", n, addr)
			}
			if c := serverFirstLoss(p); proven != "" && c.Count != 2 {
				t.Fatalf("server-first loss %+v; want the 2 SMTP sends only", c)
			}
		})
	}
}

// The destinations a saidOnce holds are bounded, the oldest forgotten one at
// a time, never all at once; drop forgets one.
func TestASaidOnceForgetsItsOldestDestinationOnly(t *testing.T) {
	var s saidOnce
	for i := 0; i < saidOnceBound+1; i++ {
		if !s.first(fmt.Sprintf("10.0.0.1:%d", i)) {
			t.Fatalf("destination %d said before", i)
		}
	}
	if s.has("10.0.0.1:0") {
		t.Fatal("the oldest destination was kept past the bound")
	}
	if !s.has("10.0.0.1:1") || !s.has(fmt.Sprintf("10.0.0.1:%d", saidOnceBound)) {
		t.Fatal("more than the oldest destination was forgotten")
	}
	if s.first("10.0.0.1:1") {
		t.Fatal("a destination held was said again")
	}
	s.drop("10.0.0.1:1")
	if s.has("10.0.0.1:1") || !s.first("10.0.0.1:1") {
		t.Fatal("drop did not forget the destination")
	}
}

// A marked destination's mark goes only on the destination's own evidence
// that another service is there (MySQL, a client-first protocol's preamble),
// not on a verdict that says nothing of what speaks there: an application
// that connects and closes, a greeting outside the known server-first ones
// (an SMTP 421), a destination that cannot be dialled for a while, a client
// that speaks before the greeting. After each, its next 220 connection is
// relayed, with the switch off and on.
func TestAMarkedDestinationKeepsItsMarkWithoutEvidence(t *testing.T) {
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			var busy atomic.Bool
			served := make(chan string, 8)
			// On Linux the destination's port is held for the test's life
			// (testnet.SharedPort), so it refuses the dial while nothing
			// listens there, and the test can listen there again.
			addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(testnet.SharedPort(t)))
			listen := func() (net.Listener, <-chan struct{}) {
				ln := testnet.ListenShared(t, "tcp4", addr)
				done := make(chan struct{})
				go func() {
					defer close(done)
					for {
						c, err := ln.Accept()
						if err != nil {
							return
						}
						if busy.Load() {
							go func() {
								_, _ = io.WriteString(c, "421 busy, try later\r\n")
								_ = c.Close()
							}()
							continue
						}
						go smtpServer(served)(c)
					}
				}()
				return ln, done
			}
			ln, stopped := listen()
			core, logs := observer.New(zap.DebugLevel)
			p := recordingProxy(t, addr, zap.New(core))
			// decided waits for the probe's next verdict: a client that
			// waits to be greeted speaks only after it.
			decided := func(before int) {
				t.Helper()
				ch := make(chan struct{})
				go func() {
					defer close(ch)
					for logs.FilterMessage("mysql probe result").Len() <= before {
						time.Sleep(time.Millisecond)
					}
				}()
				awaitEvent(t, ch, "the probe's verdict")
			}
			callServerFirst(t, p, served, false)
			mustBeMarked := func(after string) {
				t.Helper()
				if !p.serverFirstDests.has(addr) {
					t.Fatalf("the mark went after %s", after)
				}
			}
			mustBeMarked("the first call")

			// The application connects and closes.
			app, src := acceptedConn(t)
			done := make(chan error, 1)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			_ = app.Close()
			awaitEvent(t, done, "the handling of a connection closed at once")
			mustBeMarked("a connection the application closed at once")

			// A 421, and the destination closes: the client, waiting to be
			// greeted, speaks only once the probe has decided on the 421.
			busy.Store(true)
			before := logs.FilterMessage("mysql probe result").Len()
			app, src = acceptedConn(t)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			decided(before)
			_ = app.SetDeadline(time.Now().Add(10 * time.Second))
			_, _ = io.WriteString(app, "QUIT\r\n")
			_, _ = io.ReadAll(app)
			_ = app.Close()
			awaitEvent(t, done, "the handling of a 421")
			mustBeMarked("a 421")
			busy.Store(false)

			// An eager client, speaking before the greeting.
			app, src = acceptedConn(t)
			if _, err := io.WriteString(app, "EHLO eager\r\n"); err != nil {
				t.Fatal(err)
			}
			go func() { done <- p.handleConnection(context.Background(), src) }()
			_ = app.SetDeadline(time.Now().Add(10 * time.Second))
			r := bufio.NewReader(app)
			_, _ = r.ReadString('\n')
			_ = app.Close()
			awaitEvent(t, done, "the handling of an eager client")
			if got := awaitEvent(t, served, "the eager EHLO reaching the destination"); got != "EHLO eager" {
				t.Fatalf("the destination got %q; want the eager client's EHLO, dispatched to it", got)
			}
			mustBeMarked("a client that spoke first")

			// The destination cannot be dialled for a while.
			_ = ln.Close()
			<-stopped
			app, src = acceptedConn(t)
			go func() { done <- p.handleConnection(context.Background(), src) }()
			awaitEvent(t, done, "the handling of a connection that cannot be dialled")
			_ = app.Close()
			mustBeMarked("a failed dial")

			listen()
			callServerFirst(t, p, served, false)
			mustBeMarked("the destination greeting again")
		})
	}
}

// A destination seen again is the last to be forgotten.
func TestASaidOnceKeepsADestinationSeenAgain(t *testing.T) {
	var s saidOnce
	s.first("smtp")
	for i := 1; i < saidOnceBound; i++ {
		s.first(fmt.Sprintf("ftp-data:%d", i))
	}
	s.first("smtp") // seen again
	s.first("ftp-data:new")
	if !s.has("smtp") || s.has("ftp-data:1") {
		t.Fatal("the destination seen again was forgotten before an older one")
	}
}
