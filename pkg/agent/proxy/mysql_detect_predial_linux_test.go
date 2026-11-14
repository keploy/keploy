package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestProbeClosesTheSilentApplicationsEndTheDestinationClosed: when the
// probe takes the connection dialled while the application's handshake was
// held, and the destination closed it while the application was still
// silent, the application's end closes too, as its own connection would have
// without keploy. Handshakes are held, and so connections pre-dialled and
// watched (util.Predialed.WatchUntilTaken), on Linux only: elsewhere the
// proxy dials when the application first speaks, so this row is Linux's.
func TestProbeClosesTheSilentApplicationsEndTheDestinationClosed(t *testing.T) {
	addr, _ := predialProbeServer(t, func(c net.Conn) { _ = c.Close() })
	p := testProxy(t)
	upstream, err := util.DialUpstream(context.Background(), nil, "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	pre := util.NewPredialed(addr, upstream)
	defer pre.CloseIfUnused()
	app, srcConn := clientPair(t)
	defer func() { _ = app.Close() }()
	// As handleConnection arms it.
	pre.WatchUntilTaken(func() {
		if util.ReceivedNothing(srcConn) {
			_ = srcConn.Close()
		}
	})
	port := uint32(upstream.RemoteAddr().(*net.TCPAddr).Port)

	probe, err := p.probeMysql(context.Background(), srcConn, addr, pre, port,
		models.MODE_RECORD, models.OutgoingOptions{}, zap.NewNop())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if probe.IsMySQL {
		t.Fatal("classified as MySQL")
	}
	_ = app.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := app.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read %v; the destination closed the application's connection while it was silent, and its end must close too", err)
	}
}
