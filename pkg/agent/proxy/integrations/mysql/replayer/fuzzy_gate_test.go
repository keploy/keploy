package replayer

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func pingMock(name string) *models.Mock {
	m := &models.Mock{
		Name: name,
		Kind: models.MySQL,
	}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.MySQLRequests = []mysql.Request{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_PING"},
			Message: &mysql.PingPacket{Command: 0x0e},
		},
	}}
	m.Spec.MySQLResponses = []mysql.Response{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 7, SequenceID: 1}, Type: "OK"},
			Message: &mysql.OKPacket{},
		},
	}}
	return m
}

func pingReq() mysql.Request {
	return mysql.Request{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 0}, Type: "COM_PING"},
			Message: &mysql.PingPacket{Command: 0x0e},
		},
	}
}

// Control commands (COM_PING etc.) only ever match via the score path, but
// their comparators are exact protocol-level matches — the fuzzy-policy gate
// must NOT classify them as similarity guesses. Under off, a matched ping
// must still be served; otherwise pooled drivers (keepalive pings) drop their
// connections the moment deterministic mode is enabled.
func TestMatchCommand_FuzzyOff_ServesExactControlCommands(t *testing.T) {
	logger := zap.NewNop()
	db := &fakeMockDb{session: []*models.Mock{pingMock("ping-1")}}
	dctx := newDecodeCtx()
	dctx.FuzzyMatchPolicy = models.FuzzyMatchOff

	resp, ok, _, _, err := matchCommand(context.Background(), logger, pingReq(), db, dctx)
	if err != nil || !ok || resp == nil {
		t.Fatalf("exact COM_PING match must be served under fuzzyMatch=off, got ok=%v err=%v", ok, err)
	}
}

// A COM_QUERY candidate that survives ONLY via partial-shape scoring (no
// exact text, no FIFO) is a similarity guess: refused under off, served
// under on.
func TestMatchCommand_FuzzyOff_RejectsScoreOnlyQueryPick(t *testing.T) {
	logger := zap.NewNop()
	recorded := readbackMock("row-1", "SELECT id, name FROM users WHERE tenant = 'acme'", "m1", time.Time{})
	liveSQL := "SELECT something_else FROM another_table"

	dctxOff := newDecodeCtx()
	dctxOff.FuzzyMatchPolicy = models.FuzzyMatchOff
	dbOff := &fakeMockDb{session: []*models.Mock{recorded}}
	_, ok, _, _, err := matchCommand(context.Background(), logger, comQueryReq(liveSQL), dbOff, dctxOff)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("score-only COM_QUERY pick must be refused under fuzzyMatch=off")
	}

	dctxOn := newDecodeCtx()
	dctxOn.FuzzyMatchPolicy = models.FuzzyMatchOn
	dbOn := &fakeMockDb{session: []*models.Mock{readbackMock("row-1", "SELECT id, name FROM users WHERE tenant = 'acme'", "m1", time.Time{})}}
	_, ok, _, _, err = matchCommand(context.Background(), logger, comQueryReq(liveSQL), dbOn, dctxOn)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("legacy fuzzyMatch=on must keep serving the score-based pick")
	}
}
