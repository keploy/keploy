package proxy

import (
	"context"
	"net"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A gRPC call opens no window, so the windows no longer cover the requests once
// one has come, and the calls it makes must not read as made outside them. That
// holds from before its first byte reaches the app: a call it makes can never
// be ahead of it.
func TestRecordIncomingStopsCallsBeingMarkedOutsideRequests(t *testing.T) {
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
	if _, err := cliApp.Write([]byte("P")); err != nil {
		t.Fatal(err)
	}
	if _, err := appApp.Read(make([]byte, 1)); err != nil {
		t.Fatalf("the preface never reached the app: %v", err)
	}
	if call().OutsideRequests {
		t.Fatal("a call made once a gRPC request had reached the app is marked outside the requests")
	}
}
