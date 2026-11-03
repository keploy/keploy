package mysql

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// MySQL records only from keploy's V2 chunk streams. A caller that attaches no
// V2 session is declined (integrations.ErrParserDeclined) before the parser
// reads a byte, so it can relay the connection raw: the bytes the app and the
// server sent are still on their sockets.
func TestRecordOutgoingDeclinesASessionWithoutV2(t *testing.T) {
	m := New(zap.NewNop())
	app, clientSide := net.Pipe()
	server, destSide := net.Pipe()
	t.Cleanup(func() {
		for _, c := range []net.Conn{app, clientSide, server, destSide} {
			_ = c.Close()
		}
	})
	sess := &integrations.RecordSession{Ingress: clientSide, Egress: destSide, Logger: zap.NewNop()}
	// net.Pipe is unbuffered: a write completes only when the other end reads.
	wrote := make(chan error, 2)
	go func() { _, err := app.Write([]byte("client")); wrote <- err }()
	go func() { _, err := server.Write([]byte("server")); wrote <- err }()

	if err := m.RecordOutgoing(context.Background(), sess); !errors.Is(err, integrations.ErrParserDeclined) {
		t.Fatalf("RecordOutgoing without V2 = %v, want a decline", err)
	}
	for _, c := range []net.Conn{clientSide, destSide} {
		if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 6)
		if n, err := c.Read(buf); err != nil || n != 6 {
			t.Fatalf("after the decline the socket gave %q, %v: the parser read bytes it declined", buf[:n], err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := <-wrote; err != nil {
			t.Fatal(err)
		}
	}
	if err := m.RecordOutgoing(context.Background(), nil); !errors.Is(err, integrations.ErrParserDeclined) {
		t.Fatalf("RecordOutgoing(nil) = %v, want a decline", err)
	}
}

// A session with V2 chunk streams is recorded from them, not declined: here
// the streams end before the server's greeting, so nothing is recorded.
func TestRecordOutgoingRecordsASessionWithV2(t *testing.T) {
	clientCh, destCh := make(chan fakeconn.Chunk), make(chan fakeconn.Chunk)
	close(clientCh)
	close(destCh)
	mocks := make(chan *models.Mock, 1)
	v2 := &supervisor.Session{
		ClientStream: fakeconn.New(clientCh, nil, nil),
		DestStream:   fakeconn.New(destCh, nil, nil),
		Mocks:        mocks,
		Logger:       zap.NewNop(),
		Ctx:          context.Background(),
	}
	sess := &integrations.RecordSession{Logger: zap.NewNop(), V2: v2}
	if err := New(zap.NewNop()).RecordOutgoing(context.Background(), sess); errors.Is(err, integrations.ErrParserDeclined) {
		t.Fatalf("RecordOutgoing with V2 = %v: a session with V2 streams is recorded, not declined", err)
	}
	if n := len(mocks); n != 0 {
		t.Fatalf("%d mocks from streams that ended before the greeting", n)
	}
}
