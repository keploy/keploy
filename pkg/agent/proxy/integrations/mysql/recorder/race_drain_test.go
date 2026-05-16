package recorder

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap/zaptest"
)

// pipeConn is a net.Conn-shaped wrapper around a unidirectional byte
// pipe. Reads block until bytes are pushed or the conn is closed.
// Writes are captured into a buffer (used to test the forward path
// without bringing up real sockets). It is just enough surface to
// drive handleClientQueries from outside the package boundary.
type pipeConn struct {
	mu     sync.Mutex
	rdBuf  bytes.Buffer
	wrBuf  bytes.Buffer
	closed bool
	ready  chan struct{}
}

func newPipeConn() *pipeConn {
	return &pipeConn{ready: make(chan struct{}, 256)}
}

func (p *pipeConn) push(b []byte) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.rdBuf.Write(b)
	p.mu.Unlock()
	select {
	case p.ready <- struct{}{}:
	default:
	}
}

func (p *pipeConn) Read(out []byte) (int, error) {
	for {
		p.mu.Lock()
		if p.rdBuf.Len() > 0 {
			n, err := p.rdBuf.Read(out)
			p.mu.Unlock()
			return n, err
		}
		if p.closed {
			p.mu.Unlock()
			return 0, net.ErrClosed
		}
		p.mu.Unlock()
		<-p.ready
	}
}

func (p *pipeConn) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, net.ErrClosed
	}
	return p.wrBuf.Write(b)
}

func (p *pipeConn) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	close(p.ready)
	return nil
}

func (*pipeConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}
}
func (*pipeConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3306}
}
func (*pipeConn) SetDeadline(time.Time) error      { return nil }
func (*pipeConn) SetReadDeadline(time.Time) error  { return nil }
func (*pipeConn) SetWriteDeadline(time.Time) error { return nil }

// buildPostHandshakeDecodeCtx returns a DecodeContext ready to consume
// command-phase packets on the given client conn. The capability bits
// chosen here mirror what handleInitialHandshake leaves in place after a
// typical CLIENT_PROTOCOL_41 + mysql_native_password + CLIENT_DEPRECATE_EOF
// negotiation, so the decoder takes the modern result-set path.
func buildPostHandshakeDecodeCtx(clientConn net.Conn) *wire.DecodeContext {
	caps := uint32(mysql.CLIENT_PROTOCOL_41 |
		mysql.CLIENT_PLUGIN_AUTH |
		mysql.CLIENT_SECURE_CONNECTION |
		wire.CLIENT_DEPRECATE_EOF |
		mysql.CLIENT_CONNECT_WITH_DB)
	dctx := &wire.DecodeContext{
		Mode:               models.MODE_RECORD,
		LastOp:             wire.NewLastOpMap(),
		ServerGreetings:    wire.NewGreetings(),
		PreparedStatements: make(map[uint32]*mysql.StmtPrepareOkPacket),
		ClientCaps:         caps,
		ClientCapabilities: caps,
		ServerCaps:         caps,
		PluginName:         string(mysql.Native),
	}
	dctx.ServerGreetings.Store(clientConn, &mysql.HandshakeV10Packet{
		ProtocolVersion: 0x0a,
		ServerVersion:   "8.0.test-keploy",
		ConnectionID:    99,
		AuthPluginData:  bytes.Repeat([]byte{0x11}, 20),
		CapabilityFlags: caps,
		CharacterSet:    0x21,
		StatusFlags:     0x02,
		AuthPluginName:  string(mysql.Native),
	})
	return dctx
}

// comQueryPacket builds a COM_QUERY wire packet.
func comQueryPacket(seq byte, query string) []byte {
	payload := make([]byte, 1+len(query))
	payload[0] = mysql.COM_QUERY
	copy(payload[1:], query)
	hdr := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	return append(hdr, payload...)
}

// okResponsePacket builds an OK response to a COM_QUERY (DEPRECATE_EOF
// path: 7 byte body — header + affected_rows + last_insert_id +
// status_flags + warnings).
func okResponsePacket(seq byte) []byte {
	payload := []byte{
		mysql.OK,
		0x00, 0x00, // affected_rows + last_insert_id (lenenc 0)
		0x02, 0x00, // status_flags = AUTOCOMMIT
		0x00, 0x00, // warnings
	}
	hdr := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	return append(hdr, payload...)
}

// prepareCmd builds a COM_STMT_PREPARE command packet.
func prepareCmd(seq byte, q string) []byte {
	payload := make([]byte, 1+len(q))
	payload[0] = mysql.COM_STMT_PREPARE
	copy(payload[1:], q)
	hdr := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	return append(hdr, payload...)
}

// prepareOkShort builds a minimal COM_STMT_PREPARE_OK response with
// zero columns and zero params, which the decoder treats as a single
// "head packet" and emits the mock immediately on.
func prepareOkShort(seq byte, stmtID uint32) []byte {
	payload := []byte{
		0x00,                                                                    // status (OK marker)
		byte(stmtID), byte(stmtID >> 8), byte(stmtID >> 16), byte(stmtID >> 24), // statement_id
		0x00, 0x00, // num_columns
		0x00, 0x00, // num_params
		0x00,       // reserved
		0x00, 0x00, // warnings
	}
	hdr := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), seq}
	return append(hdr, payload...)
}

// TestHandleClientQueries_DrainOnCtxCancel pins the invariant that a
// COM_QUERY whose response chunks have been delivered into the per-
// direction relay channels is NOT dropped when the parent ctx cancels
// shortly afterwards. This is the regression class behind the
// "record-side packet drop during fast back-to-back operations" bug:
// a /me handler issues a query + cache miss + DB call sequence
// 2-3 ms before the test harness cancels the parser ctx, and the
// previous cleanup path took `case <-ctx.Done()` immediately without
// draining the buff chans — so the bytes (already read off the real
// socket and queued for the parser) ended up on the floor instead of
// in the captured mock pool. The fix:
//
//   - detaches the async decoder's ctx via context.WithoutCancel so a
//     parent cancel can't race recordMock's select-against-ctx,
//   - drains clientBuffChan/destBuffChan into the decoder during
//     cleanup before closing the decoder's input channel,
//   - keeps the readRelay's `ch <- data` send unconditional on the
//     fast path so a successful Read can never lose its bytes to a
//     concurrent ctx-cancel.
func TestHandleClientQueries_DrainOnCtxCancel(t *testing.T) {
	t.Parallel()

	clientConn := newPipeConn()
	destConn := newPipeConn()

	decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
	decodeCtx.LastOp.Store(clientConn, wire.RESET)

	mocks := make(chan *models.Mock, 8)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), models.ClientConnectionIDKey, "test-conn-0"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = handleClientQueries(ctx, zaptest.NewLogger(t), clientConn, destConn, mocks,
			decodeCtx, models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"}})
	}()

	// Push the command first, then the response. The small gap mirrors
	// wire-ordering on a real keep-alive connection: the request leaves
	// the app before the server's response can arrive on the dest socket,
	// so the readRelay goroutines see them in that order.
	clientConn.push(comQueryPacket(0, "SELECT 1"))
	time.Sleep(2 * time.Millisecond)
	destConn.push(okResponsePacket(1))
	time.Sleep(20 * time.Millisecond)
	cancel()
	// In production the agent's deferred conn.Close fires shortly after
	// ctx cancel and wakes any readRelay goroutine stuck in Read. The
	// synthetic pipeConn doesn't share that behaviour, so we close it
	// here so the readRelay goroutines exit cleanly inside the bounded
	// drain wait.
	_ = clientConn.Close()
	_ = destConn.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleClientQueries did not return after ctx cancel")
	}

	var got []*models.Mock
collect:
	for {
		select {
		case m, ok := <-mocks:
			if !ok {
				break collect
			}
			got = append(got, m)
		default:
			break collect
		}
	}

	if len(got) == 0 {
		t.Fatalf("expected at least 1 mock emitted before/after ctx cancel; got 0 — chunks were dropped at the buff-chan/decode-chan boundary")
	}
	var found *models.Mock
	for _, m := range got {
		if m == nil || m.Spec.Metadata == nil {
			continue
		}
		if m.Spec.Metadata["requestOperation"] == "COM_QUERY" {
			found = m
			break
		}
	}
	if found == nil {
		t.Fatalf("no COM_QUERY mock found in emitted set (%d total); the bug is back — fast cmd/resp burst lost across ctx cancel boundary", len(got))
	}
	if got, want := found.Spec.Metadata["responseOperation"], mysql.StatusToString(mysql.OK); got != want {
		t.Errorf("response operation = %q, want %q", got, want)
	}
}

// TestHandleClientQueries_PreparePlusExecute pins the specific shape
// from the production repro: a back-to-back COM_STMT_PREPARE pair that
// straddles the parser ctx-cancel window. Both PREPARE mocks must
// survive end-to-end.
//
// We use minimal PREPARE_OK packets (zero columns, zero params) so the
// decoder emits the mock on the head packet without needing follow-up
// column/param defs — that keeps the test focused on the cancel-race
// boundary rather than the result-set decoder's state machine.
func TestHandleClientQueries_PreparePlusExecute(t *testing.T) {
	t.Parallel()

	clientConn := newPipeConn()
	destConn := newPipeConn()

	decodeCtx := buildPostHandshakeDecodeCtx(clientConn)
	decodeCtx.LastOp.Store(clientConn, wire.RESET)

	mocks := make(chan *models.Mock, 8)
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), models.ClientConnectionIDKey, "test-conn-1"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = handleClientQueries(ctx, zaptest.NewLogger(t), clientConn, destConn, mocks,
			decodeCtx, models.OutgoingOptions{DstCfg: &models.ConditionalDstCfg{Addr: "127.0.0.1:3306"}})
	}()

	// First exchange — drains cleanly through the steady-state path.
	clientConn.push(prepareCmd(0, "SELECT 1"))
	time.Sleep(2 * time.Millisecond)
	destConn.push(prepareOkShort(1, 1))
	time.Sleep(20 * time.Millisecond)

	// Second exchange staged just before cancel. The fix must keep this
	// mock alive through the ctx-cancel cleanup path.
	clientConn.push(prepareCmd(0, "SELECT 2"))
	time.Sleep(2 * time.Millisecond)
	destConn.push(prepareOkShort(1, 2))
	time.Sleep(20 * time.Millisecond)
	cancel()
	_ = clientConn.Close()
	_ = destConn.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleClientQueries did not return after ctx cancel")
	}

	var got []*models.Mock
collect:
	for {
		select {
		case m, ok := <-mocks:
			if !ok {
				break collect
			}
			got = append(got, m)
		default:
			break collect
		}
	}

	var prepares int
	for _, m := range got {
		if m == nil || m.Spec.Metadata == nil {
			continue
		}
		if m.Spec.Metadata["requestOperation"] == "COM_STMT_PREPARE" {
			prepares++
		}
	}
	if prepares < 2 {
		t.Fatalf("expected both COM_STMT_PREPARE mocks captured before ctx-cancel teardown; got %d (total mocks emitted: %d) — fast back-to-back prepares are still dropping at the buff-chan boundary", prepares, len(got))
	}
}
