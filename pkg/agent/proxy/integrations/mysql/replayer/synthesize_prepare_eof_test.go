package replayer

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// prepareReq builds a COM_STMT_PREPARE request for the given query.
func prepareReq(query string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{SequenceID: 0},
			Type:   mysql.CommandStatusToString(mysql.COM_STMT_PREPARE),
		},
		Message: &mysql.StmtPreparePacket{Command: mysql.COM_STMT_PREPARE, Query: query},
	}}
}

// TestSynthesizedPrepareOK_ParamDefEOF is the regression guard for the MySQL
// fuzzer hang: when a COM_STMT_PREPARE has no recorded mock, matchCommand
// synthesizes a COM_STMT_PREPARE_OK. For a client that did NOT negotiate
// CLIENT_DEPRECATE_EOF (e.g. go-sql-driver), the parameter-definition block
// MUST be terminated by an EOF packet, or the driver's readUntilEOF() blocks
// forever ("driver: bad connection", whole-pool cascade, 1000s deadlock).
// DEPRECATE_EOF clients must NOT get the terminator, and a zero-param prepare
// has no param-def block so needs none either.
func TestSynthesizedPrepareOK_ParamDefEOF(t *testing.T) {
	logger := zap.NewNop()
	// A non-empty pool with NO recorded COM_STMT_PREPARE for our query: the
	// pool must be non-empty (an entirely empty db bails with "no mysql mocks
	// found" before the synthesize path), but nothing matches the prepare, so
	// matchCommand falls through to synthesizing a PREPARE_OK.
	emptyDb := &fakeMockDb{session: []*models.Mock{execMock("unrelated-filler", "x")}}

	synth := func(t *testing.T, dctx *wire.DecodeContext, query string) *mysql.StmtPrepareOkPacket {
		t.Helper()
		resp, ok, _, err := matchCommand(context.Background(), logger, prepareReq(query), emptyDb, dctx, nil, nil)
		if err != nil || !ok || resp == nil {
			t.Fatalf("expected a synthesized PREPARE_OK (ok=%v err=%v resp=%v)", ok, err, resp)
		}
		pkt, isPrep := resp.PacketBundle.Message.(*mysql.StmtPrepareOkPacket)
		if !isPrep {
			t.Fatalf("expected *StmtPrepareOkPacket, got %T", resp.PacketBundle.Message)
		}
		return pkt
	}

	t.Run("non-DEPRECATE_EOF client gets a well-formed param-def EOF", func(t *testing.T) {
		dctx := newDecodeCtx()                                            // ServerCaps=0, ClientCaps=0 → DeprecateEOF()==false
		pkt := synth(t, dctx, "SELECT id FROM t WHERE a BETWEEN ? AND ?") // 2 params

		if pkt.NumParams != 2 {
			t.Fatalf("NumParams = %d, want 2", pkt.NumParams)
		}
		eof := pkt.EOFAfterParamDefs
		// Legacy EOF: 4-byte header + 5-byte payload = 9 bytes.
		if len(eof) != 9 {
			t.Fatalf("EOFAfterParamDefs len = %d (%v), want 9-byte legacy EOF packet", len(eof), eof)
		}
		if eof[4] != 0xFE {
			t.Fatalf("EOF marker = 0x%02x, want 0xFE", eof[4])
		}
		// payload length field (3-byte LE) must be 5.
		if eof[0] != 0x05 || eof[1] != 0x00 || eof[2] != 0x00 {
			t.Fatalf("EOF length header = %v, want [5 0 0]", eof[:3])
		}
		// sequence id must follow the param defs (seq 2..1+numParams), i.e. 2+numParams.
		if want := byte(2 + pkt.NumParams); eof[3] != want {
			t.Fatalf("EOF sequence id = %d, want %d (2+numParams)", eof[3], want)
		}
	})

	t.Run("DEPRECATE_EOF client gets no terminator", func(t *testing.T) {
		dctx := newDecodeCtx()
		dctx.ServerCaps = wire.CLIENT_DEPRECATE_EOF
		dctx.ClientCaps = wire.CLIENT_DEPRECATE_EOF
		pkt := synth(t, dctx, "SELECT id FROM t WHERE a = ?") // 1 param
		if len(pkt.EOFAfterParamDefs) != 0 {
			t.Fatalf("DEPRECATE_EOF client must get no param-def EOF, got %v", pkt.EOFAfterParamDefs)
		}
	})

	t.Run("zero-param prepare needs no param-def EOF", func(t *testing.T) {
		pkt := synth(t, newDecodeCtx(), "SELECT 1")
		if pkt.NumParams != 0 {
			t.Fatalf("NumParams = %d, want 0", pkt.NumParams)
		}
		if len(pkt.EOFAfterParamDefs) != 0 {
			t.Fatalf("zero-param prepare must have no param-def EOF, got %v", pkt.EOFAfterParamDefs)
		}
	})
}
