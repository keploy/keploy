package proxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A gRPC call's window opens before the app can read its request, so a call the
// app makes for it never reads as made outside the requests, however late its
// mock comes; one made after the latest window ended, with none open, does.
func TestRecordIncomingKeepsACallsMocksFromBeingMarkedOutsideRequests(t *testing.T) {
	mgr := syncMock.New(zap.NewNop())
	mgr.OpenWindow(time.Now(), nil).Close()
	call := func() *models.Mock {
		m := &models.Mock{Kind: models.Mongo}
		now := time.Now()
		m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock = now.Add(time.Second), now.Add(2*time.Second)
		mgr.AddMock(m)
		return m
	}
	if !call().OutsideRequests {
		t.Fatal("a call after the only request ended is not marked, so this row proves nothing")
	}

	ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
	cliApp, cliProxy := net.Pipe()
	appProxy, appApp := net.Pipe()
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		_ = cliApp.Close()
		_ = appApp.Close()
		<-done
	})
	tcCh := make(chan *models.TestCase, 8)
	go func() {
		defer close(done)
		_ = RecordIncoming(ctx, zap.NewNop(), cliProxy, appProxy, tcCh, 50051, "", false, false)
	}()
	req, _ := pingCall(t)
	go func() { _, _ = cliApp.Write(req) }()
	if _, err := io.ReadFull(appApp, make([]byte, len(req))); err != nil {
		t.Fatalf("the request never reached the app: %v", err)
	}
	if call().OutsideRequests {
		t.Fatal("a call made once a gRPC request had reached the app is marked outside the requests")
	}
}
