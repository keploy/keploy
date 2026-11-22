package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// A host name is the app's (a tenant's subdomain, a customer's own domain):
// every line the agent logs about a connection names it by its labels' shape
// beside its tag under the connection's session (models.HostLogName), and so
// does every error a line logs, the dial's and the TLS handshake's among
// them, whose text quotes the name whole. The rows below drive what the
// replay rows (dns_logleak_test.go) do not: a recording's CONNECT tunnel and
// its TLS, the leaf lines of two sessions, and mock.real's other names.

// leakHost is a host name that holds the canary: lower case, as a client's
// SNI and a DNS name are sent, which the detector finds in any case.
var leakHost = strings.ToLower(logleak.Canary) + ".tenant.example"

// corpProxyTLS is a corporate HTTP proxy that answers each CONNECT 200 and
// is then, on the tunnel, the target's TLS server, with httptest's
// certificate (example.com, 127.0.0.1): roots trusts it. It answers one HTTP
// call on the tunnel 200 "ok".
func corpProxyTLS(t *testing.T) (addr string, roots *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	roots = x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	cfg := &tls.Config{Certificates: srv.TLS.Certificates, MinVersion: tls.VersionTLS12}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
					return
				}
				tc := tls.Server(c, cfg)
				if _, err := http.ReadRequest(bufio.NewReader(tc)); err != nil {
					return
				}
				_, _ = io.WriteString(tc, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}()
		}
	}()
	return ln.Addr().String(), roots
}

// recordThroughCONNECT records one connection through the corporate proxy at
// corp: the application asks it, by CONNECT, for leakHost:443, makes its TLS
// handshake with keploy's MITM naming leakHost (SNI), and sends one call.
// keploy's lines go to logger. What handleConnection returned.
func recordThroughCONNECT(t *testing.T, logger *zap.Logger, keys *models.LogKeySession, corp string, opts models.OutgoingOptions) error {
	t.Helper()
	app, src := acceptedConn(t)
	p := &Proxy{logger: logger, DestInfo: fakeDestInfo{dest: ipv4Dest(t, corp)}, mysqlPorts: newMysqlPortRegistry()}
	opts.DisableMysqlAutoDetect = true
	p.setSession(&agent.Session{Mode: models.MODE_RECORD, MC: make(chan *models.Mock, 4), OutgoingOptions: opts, LogKeys: keys})
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	_, err := fmt.Fprintf(app, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\n\r\n", leakHost, leakHost)
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(app), &http.Request{Method: http.MethodConnect})
	require.NoError(t, err, "reading the tunnel's 200")
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	client := tls.Client(app, &tls.Config{ServerName: leakHost, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) //nolint:gosec // keploy's leaf is not under test
	require.NoError(t, client.Handshake(), "the client's handshake with keploy's MITM")
	_, err = fmt.Fprintf(client, "GET /v1/items HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", leakHost)
	require.NoError(t, err)
	_, _ = io.ReadAll(client)
	_ = client.Close()
	return awaitEvent(t, done, "handleConnection returning after the client left")
}

// fieldOf is whether an entry of logs with message msg has field key equal
// to want.
func fieldOf(logs *observer.ObservedLogs, msg, key, want string) bool {
	for _, e := range logs.FilterMessage(msg).All() {
		if e.ContextMap()[key] == want {
			return true
		}
	}
	return false
}

// A recording's CONNECT tunnel names its target's host in every line about
// it: the tunnel's, the TLS over it, the upstream dial and the connection's
// own (its "Destination Address"). They logged connectResult.TargetAddr, and
// the SNI's host:port, whole, at Debug on every such connection.
func TestARecordedCONNECTTunnelsHostIsLoggedByItsShapeAndTag(t *testing.T) {
	corp, _ := corpProxyTLS(t)
	logger, logs := logleak.Observe()
	keys := models.NewLogKeySession()
	_ = recordThroughCONNECT(t, logger, keys, corp, models.OutgoingOptions{})

	target := keys.HostLogName(leakHost + ":443")
	require.Equal(t, "{}.{}.{}:443 "+keys.HostTag(leakHost), target)
	require.True(t, fieldOf(logs, "CONNECT tunnel established, proceeding with inner connection", "target", target),
		"no line names the tunnel's target by its shape and tag under the recording's key: %+v", logs.All())
	require.True(t, fieldOf(logs, "TLS over CONNECT tunnel established", "target", target),
		"no line names the TLS server over the tunnel by its shape and tag: %+v", logs.All())
	logleak.Check(t, logs, logleak.Canary, "a recorded CONNECT tunnel")
}

// A TLS handshake over the tunnel that fails its certificate check (the
// recording verifies its upstream, record.upstreamTls.verify) logs its error,
// and returns it to the accept loop, which logs it again: crypto/x509's text
// names the server name it checked, and the certificate's own names. Both
// are written by their shape and tag; errors.As still finds the x509 error.
func TestAFailedTLSHandshakeOverCONNECTNamesNoHost(t *testing.T) {
	corp, roots := corpProxyTLS(t)
	logger, logs := logleak.Observe()
	keys := models.NewLogKeySession()
	err := recordThroughCONNECT(t, logger, keys, corp, models.OutgoingOptions{UpstreamTLSVerify: true, UpstreamTLSRootCAs: roots})

	require.Error(t, err, "the upstream's certificate is not for the target: its handshake must fail")
	var hn x509.HostnameError
	require.True(t, errors.As(err, &hn), "the error %v (%T) lost its x509 cause", err, err)
	require.False(t, logleak.Holds(err.Error(), logleak.Canary), "the error, which the accept loop logs, names the host: %q", err)
	require.Contains(t, err.Error(), keys.HostLogName(leakHost), "the error names the server it checked by its shape and tag")
	require.NotContains(t, err.Error(), "example.com", "the error names the certificate's own name whole")
	require.NotEmpty(t, logs.FilterMessageSnippet("failed TLS handshake over CONNECT tunnel").All(), "precondition: the failed handshake was not logged: %+v", logs.All())
	logleak.Check(t, logs, logleak.Canary, "a failed TLS handshake over a CONNECT tunnel")
}

// The leaf keploy's MITM mints or reuses for a server name is said under the
// connection's session, as its other lines are: two sessions an agent serves
// tag one host two ways, each its own. The leaf's lines took the handshake's
// context (Handshake with none: the process's key), so one host had one tag
// across every recording and replay of a long-lived agent.
func TestTheLeafLinesOfTwoSessionsTagOneHostTwoWays(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	handshake := func(keys *models.LogKeySession) {
		t.Helper()
		app, src := acceptedConn(t)
		p := &Proxy{logger: zap.New(core), DestInfo: fakeDestInfo{dest: ipv4Dest(t, "10.96.0.9:443")}, mysqlPorts: newMysqlPortRegistry()}
		p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true, DisableMysqlAutoDetect: true}, LogKeys: keys})
		mm := NewMockManager(nil, nil, zap.NewNop())
		t.Cleanup(mm.Close)
		p.mockManager = mm
		done := make(chan error, 1)
		go func() { done <- p.handleConnection(context.Background(), src) }()
		client := tls.Client(app, &tls.Config{ServerName: leakHost, InsecureSkipVerify: true}) //nolint:gosec // keploy's leaf is not under test
		require.NoError(t, client.Handshake())
		_ = client.Close()
		_ = awaitEvent(t, done, "handleConnection returning after the client left")
	}
	a, b := models.NewLogKeySession(), models.NewLogKeySession()
	// The first handshake may mint the leaf; every one after it reuses it.
	handshake(a)
	handshake(a)
	handshake(b)

	tags := map[string]int{}
	for _, e := range logs.FilterMessage("reusing cached certificate").All() {
		tags[fmt.Sprint(e.ContextMap()["hostname"])]++
	}
	require.NotZero(t, tags[a.HostLogName(leakHost)], "no reused leaf of session a's is named under a's key: %v", tags)
	require.NotZero(t, tags[b.HostLogName(leakHost)], "no reused leaf of session b's is named under b's key: %v", tags)
	require.NotEqual(t, a.HostTag(leakHost), b.HostTag(leakHost), "two sessions tagged one host alike")
	require.Len(t, tags, 2, "a leaf line named the host under neither session's key: %v", tags)
	logleak.Check(t, logs, logleak.Canary, "the MITM's leaf lines")
}

// A connection to a mock.real name's address that carries another name (its
// Host header, its SNI) is the app's: that name is logged by its shape and
// tag, never whole, both where keploy hands the connection on as a
// dependency's and where a name check ends a mock.real connection mid-relay
// (capture_incomplete, at WARN). The report's text keeps the name whole, and
// the evidence lists it with the tag the WARN names it by
// (models.CaptureIncomplete.Hosts), which keploy's CLI logs it by.
func TestAMockRealConnectionsOtherNameIsLoggedByItsShapeAndTag(t *testing.T) {
	dst := realServer(t)
	_, portText, _ := net.SplitHostPort(dst)
	p := realProxy(t, []string{"api.test:" + portText}, map[string]string{"api.test": "127.0.0.1", leakHost: "127.0.0.1"})
	logger, logs := logleak.Observe()
	p.logger = logger
	keys := models.NewLogKeySession()
	ctx := models.WithLogKeySession(context.Background(), keys)
	serve := func(accepted net.Conn) chan served {
		out := make(chan served, 1)
		go func() {
			h, c, err := p.serveReal(ctx, accepted, dst, "", 1, realdeps.Origin{}, nil)
			out <- served{h, c, err}
		}()
		return out
	}

	// Another name on its first request: a dependency's connection.
	app, accepted := realConnPair(t)
	done := serve(accepted)
	_, err := fmt.Fprintf(app, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", leakHost)
	require.NoError(t, err)
	require.False(t, (<-done).handled, "another name is a dependency")
	require.True(t, fieldOf(logs, "a connection to a mock.real name's address is for another name: a dependency", "name", keys.HostLogName(leakHost)),
		"no line names the other name by its shape and tag: %+v", logs.All())

	// Its own name first, then another: the name check ends it.
	app, accepted = realConnPair(t)
	done = serve(accepted)
	sendAndRead(t, app, fmt.Sprintf("GET / HTTP/1.1\r\nHost: api.test\r\n\r\nGET / HTTP/1.1\r\nHost: %s\r\n\r\n", leakHost))
	require.True(t, (<-done).handled)
	warns := logs.FilterMessage("keploy ended a mock.real connection: capture_incomplete").All()
	require.Len(t, warns, 1, "the name check's end was not said once: %+v", logs.All())
	require.Contains(t, fmt.Sprint(warns[0].ContextMap()["why"]), keys.HostLogName(leakHost), "the WARN does not name the other name by its shape and tag")
	run := p.RealRun()
	require.Len(t, run.Incomplete, 1)
	require.Contains(t, run.Incomplete[0].Why, leakHost, "the report's text lost the other name")
	require.True(t, run.Incomplete[0].HostsListed, "the evidence does not say it lists the hosts its why names")
	at := len("a mock.real connection to api.test:" + portText + " carried another name: a request with Host ")
	require.Equal(t, models.LossHosts{{Name: leakHost, Tag: keys.HostTag(leakHost), At: at}}, run.Incomplete[0].Hosts, "the evidence does not list the other name with the tag its WARN names it by, where its why writes it")
	require.Contains(t, run.Incomplete[0].LogWhy(), keys.HostLogName(leakHost), "keploy's CLI would not log the other name by the WARN's shape and tag")
	require.False(t, logleak.Holds(run.Incomplete[0].LogWhy(), logleak.Canary), "keploy's CLI would log the other name whole")
	logleak.Check(t, logs, logleak.Canary, "a mock.real connection's other name")
}

// What handleConnection returns, which the accept loop logs, names each host
// name the connection named (its CONNECT target, its TLS server name) by its
// shape and tag in the connection's session, whatever returned it: a
// parser's error or a dial's, which name it whole. errors.Is still sees the
// cause; an error that names none of them is returned as it is.
func TestAConnectionsErrorNamesItsHostsByTheirShapeAndTag(t *testing.T) {
	keys := models.NewLogKeySession()
	var hosts connHosts
	hosts.keys = keys
	hosts.add(leakHost+":443", "", leakHost)
	cause := errors.New("connection reset by peer")
	err := hosts.inError(fmt.Errorf("pass the call through to %s:443: %w", leakHost, cause))
	require.False(t, logleak.Holds(err.Error(), logleak.Canary), "the error names the host: %q", err)
	require.Equal(t, "pass the call through to "+keys.HostLogName(leakHost+":443")+": connection reset by peer", err.Error())
	require.ErrorIs(t, err, cause)
	plain := errors.New("EOF")
	require.Same(t, plain, hosts.inError(plain), "an error naming no host was rewritten")
	var none connHosts
	require.Equal(t, cause, none.inError(cause), "a connection that named no host rewrote its error")
}

// A request line keploy does not read (an extension method with '-', a
// version other than 1.0 and 1.1, a trailing space) holds the request's
// path and query, and an absolute target its host. On a connection to a
// mock.real address keploy's DNS did not name, closed unnamed for it, and
// on one keploy's DNS named once, made REAL by that name and refused by its
// name check, the agent's WARN and the capture_incomplete evidence keploy's
// CLI logs describe the line, never quote it.
func TestAMockRealLineKeployCannotReadIsNeverQuoted(t *testing.T) {
	lines := []string{
		"VERSION-CONTROL /repo/" + logleak.Canary + ".txt?token=" + logleak.Canary + " HTTP/1.1\r\nHost: api.test\r\n\r\n",
		"GET http://" + leakHost + "/pay?token=" + logleak.Canary + " HTTP/1.2\r\nHost: api.test\r\n\r\n",
		"GET /pay?token=" + logleak.Canary + " HTTP/1.1 \r\nHost: api.test\r\n\r\n",
	}
	for _, named := range []bool{false, true} {
		dst := realServer(t)
		_, portText, _ := net.SplitHostPort(dst)
		warn := "keploy closed a connection it could not name: capture_incomplete"
		if named {
			warn = "keploy ended a mock.real connection: capture_incomplete"
		}
		for _, line := range lines {
			p := realProxy(t, []string{"api.test:" + portText}, map[string]string{"api.test": "127.0.0.1"})
			if named {
				// keploy's DNS named the address once: the connection is
				// api.test's, relayed with its client's names checked.
				p.real.Load().book.Answered("api.test", []netip.Addr{netip.MustParseAddr("127.0.0.1")})
			}
			logger, logs := logleak.Observe()
			p.logger = logger
			ctx := models.WithLogKeySession(context.Background(), models.NewLogKeySession())
			app, accepted := realConnPair(t)
			done := make(chan served, 1)
			go func() {
				h, c, err := p.serveReal(ctx, accepted, dst, "", 1, realdeps.Origin{}, nil)
				done <- served{h, c, err}
			}()
			_, err := app.Write([]byte(line))
			require.NoError(t, err)
			require.True(t, (<-done).handled, "named %v %q", named, line)
			// Closed unnamed, the connection is the caller's to close
			// (handleConnection); refused, the relay closed it.
			_ = accepted.Close()
			warns := logs.FilterMessage(warn).All()
			require.Len(t, warns, 1, "named %v %q: the WARN was not said once: %+v", named, line, logs.All())
			require.Contains(t, fmt.Sprint(warns[0].ContextMap()["why"]), "its request line is HTTP keploy does not read (")
			logleak.Check(t, logs, logleak.Canary, fmt.Sprintf("named %v: a mock.real line keploy cannot read", named))
			run := p.RealRun()
			require.Len(t, run.Incomplete, 1, "%q", line)
			lost := models.RealUncapturedLosses(run.Incomplete)
			for form, text := range map[string]string{
				"Why": run.Incomplete[0].Why, "LogWhy": run.Incomplete[0].LogWhy(), "RealUncapturedLosses LogText": lost.LogText(), "RealUncapturedLosses Text": lost.Text(),
			} {
				require.False(t, logleak.Holds(text, logleak.Canary), "named %v %q: %s holds the request line: %q", named, line, form, text)
			}
		}
	}
}

// A mock.real connection's other name that is one of keploy's words (a
// client's Host header "client", beside keploy's "let the client open") is
// named by its shape and tag where the WARN's why writes it, there alone.
func TestAMockRealOtherNameOfKeploysWordsIsShapedThereAlone(t *testing.T) {
	dst := realServer(t)
	_, portText, _ := net.SplitHostPort(dst)
	p := realProxy(t, []string{"api.test:" + portText}, map[string]string{"api.test": "127.0.0.1"})
	logger, logs := logleak.Observe()
	p.logger = logger
	keys := models.NewLogKeySession()
	ctx := models.WithLogKeySession(context.Background(), keys)
	app, accepted := realConnPair(t)
	done := make(chan served, 1)
	go func() {
		h, c, err := p.serveReal(ctx, accepted, dst, "", 1, realdeps.Origin{}, nil)
		done <- served{h, c, err}
	}()
	// Its own name first, then "client": the name check ends it.
	sendAndRead(t, app, "GET / HTTP/1.1\r\nHost: api.test\r\n\r\nGET / HTTP/1.1\r\nHost: client\r\n\r\n")
	require.True(t, (<-done).handled)
	warns := logs.FilterMessage("keploy ended a mock.real connection: capture_incomplete").All()
	require.Len(t, warns, 1, "%+v", logs.All())
	why := fmt.Sprint(warns[0].ContextMap()["why"])
	tag := keys.HostTag("client")
	require.Equal(t, 1, strings.Count(why, tag), "the WARN names more than the one place by the tag: %q", why)
	require.Contains(t, why, "a request with Host {} "+tag+", which")
	require.Contains(t, why, "let the client open", "keploy's own word was shaped as the host: %q", why)
}

// A stopped worker's miss whose destination is an HTTP Host header naming
// an IP address with a port that is no number (match.go: dest :=
// request.Host; Go accepts `Host: 10.0.0.1:s3cr3t`) lists it with no tag,
// as an IP address has none, so keploy's CLI logs it by its shape
// (10.0.0.1:{}), as the miss's own line names it (HostLogName), in every log
// form of the loss; the report's text keeps it whole. Unlisted, every log
// form kept the port's text whole.
func TestAStoppedWorkersMissToAnIPWithATextPortIsLoggedByItsShape(t *testing.T) {
	ctx := models.WithLogKeySession(context.Background(), models.NewLogKeySession())
	for dest, shape := range map[string]string{"10.0.0.1:" + logleak.Canary: "10.0.0.1:{}", "[::1]:" + logleak.Canary: "[::1]:{}"} {
		r := &models.MockMismatchReport{Destination: dest, HostTag: models.HostTag(ctx, dest)}
		what, hosts := stoppedWorkerMissWhat(ctx, r, nil, 9, 1)
		require.Equal(t, models.LossHosts{{Name: dest, At: strings.Index(what, dest)}}, hosts, "%s: the loss does not list its destination, with no tag, where its What writes it", dest)
		loss := models.CaptureLoss{Kind: models.CaptureLossUnattributed, Count: 1, What: what, Hosts: hosts, HostsListed: true}
		for form, text := range map[string]string{
			"LogWhat": loss.LogWhat(), "LogText": models.CaptureLosses{loss}.LogText(), "NotJudgedLogText": models.CaptureLosses{loss}.NotJudgedLogText(),
		} {
			require.False(t, strings.Contains(text, logleak.Canary), "%s: %s holds the port's text whole: %q", dest, form, text)
			require.Contains(t, text, "a call to "+shape+", from process 9", "%s: %s does not name the destination by its shape: %q", dest, form, text)
		}
		require.Contains(t, models.CaptureLosses{loss}.Text(), "a call to "+dest+", from process 9", "the report's text lost the destination")
	}
}

// A stopped worker's miss to an IPv6 Host header with a zone (any text a
// client sends after %) names it by its shape, the zone {}, in the miss's
// line and in every log form of its loss, which lists the host with no tag;
// the report's text keeps it whole. The zone was whole in each.
func TestAStoppedWorkersMissToAZonedIPv6IsLoggedByItsShape(t *testing.T) {
	ctx := models.WithLogKeySession(context.Background(), models.NewLogKeySession())
	dest, shape := "[fe80::1%"+logleak.Canary+"]:443", "[fe80::1%{}]:443"
	r := &models.MockMismatchReport{Destination: dest, HostTag: models.HostTag(ctx, dest)}
	what, hosts := stoppedWorkerMissWhat(ctx, r, nil, 9, 1)
	require.Equal(t, models.LossHosts{{Name: dest, At: strings.Index(what, dest)}}, hosts, "the loss does not list its destination, with no tag, where its What writes it")
	loss := models.CaptureLoss{Kind: models.CaptureLossUnattributed, Count: 1, What: what, Hosts: hosts, HostsListed: true}
	for form, text := range map[string]string{
		"HostLogName (miss line)": models.HostLogName(ctx, dest),
		"LogWhat":                 loss.LogWhat(),
		"LogText":                 models.CaptureLosses{loss}.LogText(),
		"NotJudgedLogText":        models.CaptureLosses{loss}.NotJudgedLogText(),
	} {
		require.False(t, logleak.Holds(text, logleak.Canary), "%s holds the zone whole: %q", form, text)
		require.Contains(t, text, shape, "%s does not name the destination by its shape: %q", form, text)
	}
	require.Contains(t, models.CaptureLosses{loss}.Text(), "a call to "+dest+", from process 9", "the report's text lost the destination")
}

// A client's TLS server name may be a zoned IPv6 address whose zone is any
// text: a Go client sends "fe80::1%<text>%x" whole, its last '%' leaving no
// address, and netip reads all after the first '%' as the zone. keploy
// record dials that name when nothing was dialled while the handshake was
// held, and the dial fails (a link-local address names no interface). Its
// error, logged at ERROR and returned to the accept loop, which logs it
// again, held the zone whole; it is written {} there, as in every other line
// about the connection.
func TestARecordedDialToAZonedServerNameNamesNoZone(t *testing.T) {
	logger, logs := logleak.Observe()
	app, src := acceptedConn(t)
	p := &Proxy{logger: logger, DestInfo: fakeDestInfo{dest: ipv4Dest(t, "10.96.0.9:443")}, mysqlPorts: newMysqlPortRegistry()}
	p.setSession(&agent.Session{Mode: models.MODE_RECORD, MC: make(chan *models.Mock, 4), OutgoingOptions: models.OutgoingOptions{DisableMysqlAutoDetect: true}, LogKeys: models.NewLogKeySession()})
	done := make(chan error, 1)
	go func() { done <- p.handleConnection(context.Background(), src) }()

	client := tls.Client(app, &tls.Config{ServerName: "fe80::1%" + logleak.Canary + "%x", InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}) //nolint:gosec // keploy's leaf is not under test
	require.NoError(t, client.Handshake(), "the client's handshake with keploy's MITM")
	_, err := io.WriteString(client, "GET /v1/items HTTP/1.1\r\nHost: api.test\r\nConnection: close\r\n\r\n")
	require.NoError(t, err)
	_, _ = io.ReadAll(client)
	_ = client.Close()
	err = awaitEvent(t, done, "handleConnection returning after its dial failed")

	require.Error(t, err, "the dial of a link-local address that names no interface succeeded")
	require.False(t, logleak.Holds(err.Error(), logleak.Canary), "the error, which the accept loop logs, holds the zone: %q", err)
	require.Contains(t, err.Error(), "[fe80::1%{}]:443", "the error does not name the address it dialled, its zone {}")
	dials := logs.FilterMessage("failed to dial the conn to destination server").All()
	require.Len(t, dials, 1, "precondition: the failed dial was not logged once: %+v", logs.All())
	require.Equal(t, "[fe80::1%{}]:443", dials[0].ContextMap()["server address"])
	require.Contains(t, fmt.Sprint(dials[0].ContextMap()["error"]), "[fe80::1%{}]:443", "the ERROR's error does not name the address it dialled, its zone {}")
	logleak.Check(t, logs, logleak.Canary, "a recorded dial to a zoned server name")
}
