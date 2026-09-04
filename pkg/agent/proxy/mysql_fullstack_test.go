package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	mysqlparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// TestMySQLV2_FullStackAgainstARealServer drives the COMPLETE production
// record path against a real TLS-enabled mysqld: the real MySQL V2
// parser, the real supervisor, the real relay, the real proxy TLS upgrade
// (keploy's own MITM cert chain), and the real client driver.
//
// It closes the gap the relay-package tests cannot. There the parser is a
// scripted stand-in and TLSUpgradeFn is a passthrough; in the
// mysql/recorder tests there is no relay underneath at all. Both halves
// were only ever checked against each other's contract.
//
// Asserts, in order of what has actually regressed before:
//  0. It goes through recordMySQLOutgoing — the real dispatcher — so the
//     gate is actually evaluated rather than bypassed.
//  1. The query succeeds — mysqld's connection state was never corrupted.
//  2. Mocks reach the SyncMockManager, and one carries the query — the
//     parser was not left blocked on a bogus MySQL header of 16 03 01 00
//     (payloadLength=66326), and the command phase was not lost while the
//     handshake alone was captured.
//  3. Nothing was delivered down the raw Mocks channel, which production
//     does not use for V2 parsers (RouteMocksViaSyncMock is true).
func TestMySQLV2_FullStackAgainstARealServer(t *testing.T) {
	for _, tc := range []struct {
		name string
		// wrapDest mirrors how handleConnection hands the destination to
		// the dispatcher.
		wrapDest func(net.Conn, *zap.Logger) (net.Conn, error)
		// relayOff sets the REMOVED KEPLOY_NEW_RELAY variable, to prove a
		// stale value left in an operator's environment is inert.
		relayOff string
	}{
		{
			name:     "bare socket",
			wrapDest: func(c net.Conn, _ *zap.Logger) (net.Conn, error) { return c, nil },
		},
		{
			// The PROBE path — the primary MySQL path, since probeMysql runs
			// before parser matching — dials the server itself to read the
			// greeting, then hands the relay a replayConn that serves those
			// consumed bytes back before falling through to the socket. That
			// wrapper had never been fed to the relay: every other test uses
			// a bare net.Dial, so the shape production actually uses was the
			// one shape nothing exercised.
			name: "prefix-replay dstConn (the probe's real shape)",
			wrapDest: func(c net.Conn, lg *zap.Logger) (net.Conn, error) {
				greeting, err := readOneMySQLPacket(c)
				if err != nil {
					return nil, fmt.Errorf("read greeting for replay wrap: %w", err)
				}
				return replayConn(c, greeting, lg), nil
			},
		},
		{
			// KEPLOY_NEW_RELAY was the rollback knob, and it is gone. Operators
			// who set it once will still have it in their environment, so prove
			// end-to-end against a real server that it no longer diverts
			// anything: this case must record through the SAME V2 path as
			// "bare socket", including the strict no-raw-mocks assertion that
			// the legacy surface used to be exempt from.
			name:     "a stale KEPLOY_NEW_RELAY=off is inert",
			wrapDest: func(c net.Conn, _ *zap.Logger) (net.Conn, error) { return c, nil },
			relayOff: "off",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.relayOff != "" {
				t.Setenv("KEPLOY_NEW_RELAY", tc.relayOff)
				// Assert the DISPATCH DECISION, not just that mocks appear.
				// MySQL's legacy path also routes its mocks through the
				// SyncMockManager, so every downstream assertion in
				// runFullStackMySQL is satisfied on either surface — this case
				// stayed green with the kill switch restored until this check
				// was added.
				if !shouldRecordViaSupervisor(mysqlparser.New(zap.NewNop())) {
					t.Fatal("a stale KEPLOY_NEW_RELAY in the environment diverted MySQL off " +
						"the supervisor path; the rollback knob was removed and must be inert")
				}
			}
			runFullStackMySQL(t, tc.wrapDest)
		})
	}
}

func runFullStackMySQL(t *testing.T, wrapDest func(net.Conn, *zap.Logger) (net.Conn, error)) {
	addr, user, pass, dbName := mysqlTestTarget(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	logger := zap.NewNop()
	parser := mysqlparser.New(logger)
	p := &Proxy{
		logger: logger,
		// Registered so the dispatcher resolves the parser itself, rather
		// than the test handing it in — the dispatch decision is the thing
		// under test.
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: parser},
		recordBufferCap:            8 << 20,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     10 * time.Second,
		recordBufferHalfCloseGrace: 10 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Production routes EmitMock through the SyncMockManager
	// (RouteMocksViaSyncMock: true), NOT down the raw Mocks channel. Bind
	// a SEPARATE channel to each so the two are distinguishable: an
	// earlier version passed one channel to both and stayed green with
	// RouteMocksViaSyncMock flipped to false.
	managerMocks := make(chan *models.Mock, 128)
	rawMocks := make(chan *models.Mock, 128)

	mgr := syncMock.New(logger)
	mgr.SetOutputChannel(managerMocks)
	ctx = syncMock.NewContext(ctx, mgr)

	svErr := make(chan error, 1)
	go func() {
		clientConn, err := ln.Accept()
		// One connection is all this test serves. Closing the listener now
		// makes the driver's ErrBadConn retry fail immediately instead of
		// dialling a listener nobody drains — which cost ~40s per failure.
		_ = ln.Close()
		if err != nil {
			svErr <- fmt.Errorf("accept: %w", err)
			return
		}
		defer func() { _ = clientConn.Close() }()

		rawDest, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			svErr <- fmt.Errorf("dial mysql: %w", err)
			return
		}
		defer func() { _ = rawDest.Close() }()
		destConn, err := wrapDest(rawDest, logger)
		if err != nil {
			svErr <- err
			return
		}

		errGrp, gctx := errgroup.WithContext(ctx)
		// handleConnection seeds these three before dispatching (proxy.go
		// ~2010). Without them the LEGACY half of this dispatcher is
		// unrunnable — recordLegacy fails with "failed to get the error group
		// from the context", and recorder/record.go does an unchecked
		// ctx.Value(ClientConnectionIDKey).(string) — so a gate mutation
		// failed here after 80s blaming cleartext leakage when the real cause
		// was a missing context value.
		gctx = context.WithValue(gctx, models.ErrGroupKey, errGrp)
		gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "1")
		gctx = context.WithValue(gctx, models.DestConnectionIDKey, "2")
		// Through the DISPATCHER, not straight to recordViaSupervisor. Calling
		// the V2 entry point directly leaves the gate unevaluated, so the whole
		// production change could be reverted with this test still green.
		src, dst := clientConn, destConn
		upgrader := util.NewConnTLSUpgrader(&src, &dst, logger, pTls.HandleTLSConnection)
		svErr <- p.recordMySQLOutgoing(gctx, clientConn, destConn, rawMocks, errGrp, logger,
			1, 2, models.OutgoingOptions{
				DstCfg: &models.ConditionalDstCfg{Addr: addr},
			}, upgrader)
	}()

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?tls=skip-verify&timeout=20s&readTimeout=20s",
		user, pass, ln.Addr().String(), dbName)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)

	var got int
	if err := db.QueryRow("SELECT 42").Scan(&got); err != nil {
		t.Fatalf("query through the full production path failed: %v.\n"+
			"The direct connection in the preflight succeeded, so this is keploy: the most "+
			"likely cause is client bytes past the 36-byte SSLRequest reaching mysqld in "+
			"cleartext and corrupting its view of the connection.", err)
	}
	if got != 42 {
		t.Fatalf("SELECT 42 returned %d", got)
	}
	_ = db.Close()

	// Wait for the production path itself rather than guessing a drain
	// window; the connection lives only a few milliseconds. AddMock
	// forwards synchronously on the bound && !firstReqSeen branch, so once
	// recordViaSupervisor has returned every mock is already in the
	// channel and no sleep is needed.
	var svFinal error
	select {
	case svFinal = <-svErr:
	case <-time.After(30 * time.Second):
		t.Fatal("recordViaSupervisor never returned")
	}

	var captured []*models.Mock
drain:
	for {
		select {
		case m := <-managerMocks:
			if m != nil {
				captured = append(captured, m)
			}
		default:
			break drain
		}
	}

	if len(captured) == 0 {
		t.Fatalf("the query succeeded but ZERO mocks reached the manager bound to this "+
			"session. Traffic flows, the app is happy, and keploy records nothing. Candidates: "+
			"the parser never got usable plaintext; EmitMock routed somewhere else; or the "+
			"session was marked incomplete and its mocks dropped. (supervisor: %v)", svFinal)
	}

	// A count alone is too weak: the handshake mock alone would satisfy it
	// while every command-phase mock was lost.
	var sawQuery bool
	for i, m := range captured {
		t.Logf("mock[%d] kind=%s name=%s", i, m.Kind, m.Name)
		if b, err := json.Marshal(m); err == nil && strings.Contains(string(b), "SELECT 42") {
			sawQuery = true
		}
	}
	if !sawQuery {
		t.Fatalf("recorded %d mock(s) but none contains the query 'SELECT 42' — the handshake "+
			"was captured while the command phase was lost", len(captured))
	}

	// Production does not deliver V2 parser mocks down the raw channel.
	// If this ever fills, RouteMocksViaSyncMock has been turned off and
	// the session-window buffering that replay depends on is bypassed.
	if n := len(rawMocks); n != 0 {
		t.Errorf("%d mock(s) went down the raw Mocks channel; production routes V2 parser "+
			"mocks through the SyncMockManager (RouteMocksViaSyncMock), and bypassing it loses "+
			"the firstReqSeen session window at replay", n)
	}
	t.Logf("recorded %d mock(s) through the full production path (supervisor err: %v)",
		len(captured), svFinal)
}

// mysqlTestTarget resolves the server and PROVES it usable with a
// direct, unproxied query first. Without that preflight an unrelated
// MySQL on 3306 (wrong password, missing database) fails through keploy
// and gets diagnosed as upstream corruption — a confident, wrong answer.
//
// A missing server is a hard failure only where the lane PROMISED one,
// signalled by KEPLOY_TEST_MYSQL_REQUIRED (set by go-test.yaml, which
// provides the service). Keying this on the generic CI var instead would
// redden every fork and every unrelated lane over a database they were
// never offered, while still not making the test run anywhere.
func mysqlTestTarget(t *testing.T) (addr, user, pass, dbName string) {
	t.Helper()
	addr = envOr("KEPLOY_TEST_MYSQL_ADDR", "127.0.0.1:3306")
	user = envOr("KEPLOY_TEST_MYSQL_USER", "root")
	pass = envOr("KEPLOY_TEST_MYSQL_PASS", "rootpass")
	dbName = envOr("KEPLOY_TEST_MYSQL_DB", "sampledb")

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?tls=skip-verify&timeout=5s&readTimeout=5s",
		user, pass, addr, dbName)
	db, err := sql.Open("mysql", dsn)
	if err == nil {
		defer func() { _ = db.Close() }()
		var probe int
		err = db.QueryRow("SELECT 1").Scan(&probe)
	}
	if err != nil {
		msg := fmt.Sprintf("no usable TLS-capable MySQL at %s (%v); set "+
			"KEPLOY_TEST_MYSQL_ADDR/USER/PASS/DB", addr, err)
		if os.Getenv("KEPLOY_TEST_MYSQL_REQUIRED") != "" {
			t.Fatalf("%s. KEPLOY_TEST_MYSQL_REQUIRED is set, so this lane promised a server. This is the only coverage of the real "+
				"MySQL TLS handshake through the full record path, and a skip would let it rot "+
				"unnoticed.", msg)
		}
		t.Skip(msg)
	}
	return addr, user, pass, dbName
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// readOneMySQLPacket reads a single length-prefixed MySQL packet
// (4-byte header, 3-byte LE payload length) so the test can consume the
// server greeting the way probeMysql does before wrapping the conn.
func readOneMySQLPacket(c net.Conn) ([]byte, error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return nil, err
	}
	n := int(uint32(hdr[0]) | uint32(hdr[1])<<8 | uint32(hdr[2])<<16)
	buf := make([]byte, 4+n)
	copy(buf, hdr)
	if n > 0 {
		if _, err := io.ReadFull(c, buf[4:]); err != nil {
			return nil, err
		}
	}
	return buf, nil
}
