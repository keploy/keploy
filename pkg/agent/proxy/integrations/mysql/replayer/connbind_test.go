package replayer

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A replayed session begins with the autocommit of the greeting it was
// answered with, as the replayed login reads it: a server whose sessions
// begin with autocommit off (status 0x0000) puts the session in a
// transaction from its first statement; one that greets with autocommit on
// (0x0002) does not.
func TestAReplayedLoginBeginsTheSessionWithTheGreetingsAutocommit(t *testing.T) {
	for _, c := range []struct {
		status uint16
		off    bool
	}{
		{0x0000, true},
		{0x0002, false},
	} {
		hs := nativeHandshake()
		hs.Spec.MySQLResponses[0].Message.(*mysql.HandshakeV10Packet).StatusFlags = c.status
		client, agentEnd := net.Pipe()
		t.Cleanup(func() { _ = client.Close(); _ = agentEnd.Close() })
		d := &wire.DecodeContext{
			Mode:               models.MODE_TEST,
			LastOp:             wire.NewLastOpMap(),
			ServerGreetings:    wire.NewGreetings(),
			PreparedStatements: map[uint32]*mysql.StmtPrepareOkPacket{},
			PluginName:         string(mysql.CachingSha2),
			PreferRecordedCaps: true,
			StmtIDToQuery:      map[uint32]string{},
			NextStmtID:         1,
		}
		d.LastOp.Store(agentEnd, wire.RESET)
		done := make(chan error, 1)
		go func() {
			_, err := simulateInitialHandshake(context.Background(), zap.NewNop(), agentEnd, []*models.Mock{hs},
				&fakeMockDb{session: []*models.Mock{hs}}, d, models.OutgoingOptions{}, &models.ConditionalDstCfg{Addr: testDBAddr})
			done <- err
		}()
		app := mysqlClient{t: t, c: client}
		app.greetAndAnswer("app", mysql.Native)
		if ok := app.read(); len(ok) == 0 || ok[0] != mysql.OK {
			t.Fatalf("status %#04x: the client read %x after its handshake; want the recorded OK", c.status, ok)
		}
		if err := <-done; err != nil {
			t.Fatalf("status %#04x: the handshake returned %v", c.status, err)
		}
		if d.AutocommitOffAtConnect != c.off || d.AutocommitOff != c.off || d.InTx != c.off {
			t.Errorf("greeted with status %#04x, the session began with autocommit off at connect %v, off %v, in a transaction %v; want %v for each",
				c.status, d.AutocommitOffAtConnect, d.AutocommitOff, d.InTx, c.off)
		}
	}
}
