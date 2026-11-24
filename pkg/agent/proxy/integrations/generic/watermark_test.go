package generic

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/connseq"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// TestEncodeGenericV2_HoldsTestCaseUntilMockIsHandedOn verifies that while the
// generic V2 recorder is busy handing on exchange 1's mock, exchange 2 (whose
// flush is triggered by request 3) is NOT read ahead off the FakeConn streams
// in a way that lets the relay's watermark settle a test case over exchange 2
// before exchange 2's mock is handed on.
func TestEncodeGenericV2_HoldsTestCaseUntilMockIsHandedOn(t *testing.T) {
	clientApp, srcProxy := net.Pipe()
	dstProxy, destSvc := net.Pipe()
	up := connseq.NewUpstream(dstProxy)
	r := relay.New(relay.Config{Logger: zap.NewNop()}, srcProxy, up)

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); _ = r.Run(ctx) }()

	w := relay.NewWatermark()
	untrack := w.Track(r, "generic")

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	mocks := make(chan *models.Mock, 16)
	sess := &supervisor.Session{
		ClientStream: r.ClientStream(),
		DestStream:   r.DestStream(),
		Order:        r.StreamedBefore,
		Mocks:        mocks,
		Logger:       zap.NewNop(),
		Ctx:          context.Background(),
		ClientConnID: "c",
		DestConnID:   "d",
	}
	calls := 0
	sess.AddPostRecordHook(func(*models.Mock) {
		calls++
		if calls == 1 {
			entered <- struct{}{}
			<-release
		}
	})

	capDone := make(chan error, 1)
	go func() { capDone <- encodeGenericV2(sess, zap.NewNop()) }()

	released := false
	capWaited := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		untrack()
		cancel()
		_ = clientApp.Close()
		_ = destSvc.Close()
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
		}
		_ = r.ClientStream().Close()
		_ = r.DestStream().Close()
		_ = srcProxy.Close()
		_ = dstProxy.Close()
		if !capWaited {
			select {
			case <-capDone:
			case <-time.After(5 * time.Second):
			}
		}
	})

	ask := func(req string) {
		t.Helper()
		b := []byte(req)
		go func() { _, _ = clientApp.Write(b) }()
		if _, err := io.ReadFull(destSvc, make([]byte, len(b))); err != nil {
			t.Fatalf("server read: %v", err)
		}
	}
	answer := func(ans string) {
		t.Helper()
		b := []byte(ans)
		go func() { _, _ = destSvc.Write(b) }()
		if _, err := io.ReadFull(clientApp, make([]byte, len(b))); err != nil {
			t.Fatalf("app read: %v", err)
		}
	}

	// Exchange 1: req1 + ans1. Generic holds ans1 until req2 arrives.
	ask("REQ1")
	answer("ANS1")

	// Start exchange 2: req2 flushes exchange 1 -> pauses inside emit hook.
	before := time.Now()
	ask("REQ2")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the capture never emitted exchange 1's mock")
	}

	// While exchange 1's emit is held: exchange 2 finishes (ans2) and req3
	// arrives after the window (which will flush exchange 2 once exchange 1's
	// emit returns).
	answer("ANS2")
	after := time.Now()
	ask("REQ3")
	answer("ANS3")

	settled := false
	for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline) && !settled; time.Sleep(5 * time.Millisecond) {
		settled = w.Settled("", "", before, after)
	}
	if got := len(mocks); got != 0 {
		t.Fatalf("the capture handed on %d mocks while paused inside exchange 1's hook, want 0", got)
	}
	if settled {
		t.Errorf("the watermark settled the test case over exchange 2 while its mock was not handed on (consumed client=%d dest=%d)", r.ClientStream().Consumed(), r.DestStream().Consumed())
	}

	close(release)
	released = true

	// Trigger flush of exchange 3 by closing the pipes, then expect all 3 mocks
	// with one request payload each (so a duplicated primed chunk is caught).
	_ = clientApp.Close()
	_ = destSvc.Close()
	select {
	case err := <-capDone:
		capWaited = true
		if err != nil {
			t.Fatalf("encodeGenericV2: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("encodeGenericV2 did not return after closing pipes")
	}
	close(mocks)
	wantReqs := []string{"REQ1", "REQ2", "REQ3"}
	var gotReqs []string
	for m := range mocks {
		if len(m.Spec.GenericRequests) != 1 {
			t.Fatalf("mock had %d request payloads, want 1", len(m.Spec.GenericRequests))
		}
		gotReqs = append(gotReqs, m.Spec.GenericRequests[0].Message[0].Data)
	}
	if len(gotReqs) != len(wantReqs) {
		t.Fatalf("recorded %d mocks (%v), want %d (%v)", len(gotReqs), gotReqs, len(wantReqs), wantReqs)
	}
	for i := range wantReqs {
		if gotReqs[i] != wantReqs[i] {
			t.Fatalf("mock %d request = %q, want %q", i, gotReqs[i], wantReqs[i])
		}
	}
}
