package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	mysqlparser "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connphase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// In proxy mode a capture hole is a chunk the relay forwarded but did not tee
// (here the per-connection cap refuses one COM_QUERY). MySQL re-aligns across
// it (MySQL.CanResyncAfterGap), so the relay keeps feeding it, and the
// connection's recording goes on at the client's next command: every exchange
// but the one the hole cut is recorded, each with its own answer, and the cut
// one has no mock.
//
// Before, the relay fed MySQL no more after the hole, and the connection
// recorded nothing more for the rest of its life. What the relay does with the
// test cases is unchanged: the hole still starts the connection's unrecorded
// span (syncMock.UnrecordedConn), so the test cases recorded while it carries
// traffic after the hole are still left out, as for Mongo, which re-aligns too.
func TestRecordViaSupervisorKeepsRecordingMySQLAcrossACaptureHole(t *testing.T) {
	t.Parallel()
	const (
		queries = 8
		lost    = 4    // the query the cap refuses
		capB    = 1024 // the connection's capture buffer, recordBufferCap
	)
	query := func(i int) string {
		if i == lost {
			// Longer than the whole buffer: refused whatever the parser has read.
			return fmt.Sprintf("DO %d, '%s'", i, strings.Repeat("x", 2*capB))
		}
		return fmt.Sprintf("DO %d", i)
	}

	clientApp, srcConn := net.Pipe()
	dstConn, destSvc := net.Pipe()
	logger := zap.NewNop()
	parser := mysqlparser.New(logger)
	p := &Proxy{
		logger:                     logger,
		Integrations:               map[integrations.IntegrationType]integrations.Integrations{integrations.MYSQL: parser},
		recordBufferCap:            capB,
		recordBufferQueueSize:      64,
		recordBufferStallGrace:     10 * time.Second,
		recordBufferHalfCloseGrace: 10 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managerMocks := make(chan *models.Mock, 128)
	rawMocks := make(chan *models.Mock, 128)
	mgr := syncMock.New(logger)
	mgr.SetOutputChannel(managerMocks)
	ctx = syncMock.NewContext(ctx, mgr)
	errGrp := &errgroup.Group{}
	ctx = context.WithValue(ctx, models.ErrGroupKey, errGrp)
	ctx = context.WithValue(ctx, models.ClientConnectionIDKey, "1")
	ctx = context.WithValue(ctx, models.DestConnectionIDKey, "2")

	returned := make(chan error, 1)
	go func() {
		returned <- p.recordViaSupervisor(ctx, srcConn, connseq.NewUpstream(dstConn), parser,
			integrations.MYSQL, rawMocks, errGrp, logger, 1, 2, models.OutgoingOptions{})
	}()
	t.Cleanup(func() {
		cancel()
		for _, c := range []net.Conn{clientApp, srcConn, dstConn, destSvc} {
			_ = c.Close()
		}
	})

	// The server: a greeting, OK to the handshake response, and to query i an
	// OK whose affected-rows count is i, so a mock that pairs a query with
	// another query's answer shows.
	serverCaps := uint32(mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION)
	served := make(chan error, 1)
	go func() {
		served <- func() error {
			greeting, err := connphase.EncodeHandshakeV10(context.Background(), logger, &mysql.HandshakeV10Packet{
				ProtocolVersion: 0x0a,
				ServerVersion:   "8.0.test-keploy",
				ConnectionID:    7,
				AuthPluginData:  []byte("0123456789abcdefghij"),
				CapabilityFlags: serverCaps,
				CharacterSet:    0x21,
				StatusFlags:     0x02,
				AuthPluginName:  string(mysql.Native),
			})
			if err != nil {
				return err
			}
			if _, err := destSvc.Write(mysqlPacket(greeting, 0)); err != nil {
				return err
			}
			if _, _, err := readMySQLPacket(destSvc); err != nil {
				return fmt.Errorf("handshake response: %w", err)
			}
			ok := func(rows uint64, seq byte) error {
				b, err := phase.EncodeOk(context.Background(), &mysql.OKPacket{Header: mysql.OK, AffectedRows: rows, StatusFlags: 2}, serverCaps)
				if err != nil {
					return err
				}
				_, err = destSvc.Write(mysqlPacket(b, seq))
				return err
			}
			if err := ok(0, 2); err != nil {
				return err
			}
			for i := 1; i <= queries; i++ {
				body, _, err := readMySQLPacket(destSvc)
				if err != nil {
					return fmt.Errorf("query %d: %w", i, err)
				}
				if want := string(append([]byte{mysql.COM_QUERY}, query(i)...)); string(body) != want {
					return fmt.Errorf("query %d reached the server as %.40q, want %.40q", i, body, want)
				}
				if err := ok(uint64(i), 1); err != nil {
					return err
				}
			}
			return nil
		}()
	}()

	// The application: the handshake, then each query in turn, waiting for
	// its answer, as a client on a pooled connection does.
	if _, _, err := readMySQLPacket(clientApp); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	resp, err := connphase.EncodeHandshakeResponse41(context.Background(), logger, &mysql.HandshakeResponse41Packet{
		CapabilityFlags: serverCaps,
		MaxPacketSize:   1 << 24,
		CharacterSet:    0x21,
		Username:        "root",
		AuthResponse:    []byte("abcdefghij0123456789"),
		AuthPluginName:  string(mysql.Native),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientApp.Write(mysqlPacket(resp, 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readMySQLPacket(clientApp); err != nil {
		t.Fatalf("handshake OK: %v", err)
	}
	for i := 1; i <= queries; i++ {
		if _, err := clientApp.Write(mysqlPacket(append([]byte{mysql.COM_QUERY}, query(i)...), 0)); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		if _, _, err := readMySQLPacket(clientApp); err != nil {
			t.Fatalf("answer to query %d: %v", i, err)
		}
	}
	if err := <-served; err != nil {
		t.Fatalf("server: %v", err)
	}
	_ = clientApp.Close()
	_ = destSvc.Close()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		t.Fatal("recordViaSupervisor did not return after both ends closed")
	}

	got := map[int]bool{}
	var wrong []string
	for len(managerMocks) > 0 {
		m := <-managerMocks
		if m.Spec.Metadata["type"] == "config" || len(m.Spec.MySQLRequests) == 0 {
			continue
		}
		qp, ok := m.Spec.MySQLRequests[0].Message.(*mysql.QueryPacket)
		if !ok {
			wrong = append(wrong, fmt.Sprintf("a %T request", m.Spec.MySQLRequests[0].Message))
			continue
		}
		var i int
		if _, err := fmt.Sscanf(qp.Query, "DO %d", &i); err != nil || query(i) != qp.Query {
			wrong = append(wrong, fmt.Sprintf("a query the app never sent: %.40q", qp.Query))
			continue
		}
		var answer any = nil
		if len(m.Spec.MySQLResponses) == 1 {
			answer = m.Spec.MySQLResponses[0].Message
		}
		if okp, isOK := answer.(*mysql.OKPacket); !isOK || okp.AffectedRows != uint64(i) {
			wrong = append(wrong, fmt.Sprintf("query %d recorded with the answer %+v, want the OK of %d rows", i, answer, i))
			continue
		}
		got[i] = true
	}
	if len(wrong) > 0 {
		t.Fatalf("%d wrong mock(s); the first: %s", len(wrong), wrong[0])
	}
	if got[lost] {
		t.Fatalf("query %d, which the cap refused, was recorded", lost)
	}
	for i := 1; i <= queries; i++ {
		if i != lost && !got[i] {
			t.Errorf("query %d has no mock (recorded: %v): one capture hole must cost only the exchange it cut, not the rest of the connection's recording", i, got)
		}
	}
	if n := len(rawMocks); n != 0 {
		t.Errorf("%d mock(s) went down the raw Mocks channel; V2 parser mocks go through the SyncMockManager", n)
	}
}

// mysqlPacket frames payload as one MySQL packet with sequence id seq.
func mysqlPacket(payload []byte, seq byte) []byte {
	out := make([]byte, 4, 4+len(payload))
	out[0], out[1], out[2] = byte(len(payload)), byte(len(payload)>>8), byte(len(payload)>>16)
	out[3] = seq
	return append(out, payload...)
}

// readMySQLPacket reads one MySQL packet from c: its payload and sequence id.
func readMySQLPacket(c net.Conn) ([]byte, byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return nil, 0, err
	}
	body := make([]byte, binary.LittleEndian.Uint32(append(h[:3:3], 0)))
	if _, err := io.ReadFull(c, body); err != nil {
		return nil, 0, err
	}
	return body, h[3], nil
}

// askReadsHoles answers ReadsCaptureHoles with what it was built with, so the
// row is pinned to the method's answer, not to its presence.
type askReadsHoles struct {
	stubParser
	reads bool
}

func (a askReadsHoles) ReadsCaptureHoles() bool { return a.reads }

// A chunk a tee drops marks the session's mock incomplete for every parser
// but one that reads each capture hole in place: MySQL's streams report the
// hole, and its recorder leaves out what the hole cut.
func TestTeeDropMark(t *testing.T) {
	t.Parallel()
	var marked []string
	mark := func(reason string) { marked = append(marked, reason) }
	for _, c := range []struct {
		name   string
		parser integrations.Integrations
		marks  bool
	}{
		{"a parser that does not say", stubParser{}, true},
		{"a parser that says it does not read holes", askReadsHoles{reads: false}, true},
		{"a parser that reads holes in place", askReadsHoles{reads: true}, false},
		{"MySQL", mysqlparser.New(zap.NewNop()), false},
	} {
		marked = nil
		if got := teeDropMark(mark, c.parser); got != nil {
			got("per_conn_cap")
		}
		if (len(marked) == 1) != c.marks {
			t.Errorf("%s: a dropped chunk marked the session %d time(s), want marked=%v", c.name, len(marked), c.marks)
		}
	}
}
