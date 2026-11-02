package http

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// authoredFault is faultMock as keploy mock generate writes one: an authored
// block, and an answer the fault cuts short (an empty one for a fault before
// any answer).
func authoredFault(fault *models.Fault, resp *models.HTTPResp) *models.Mock {
	m := faultMock(fault, resp)
	m.Spec.Authored = &models.Authored{How: models.AuthoredGenerated, Reason: "payments hangs", Request: "sha256:0"}
	return m
}

// A fault an author wrote is written as a fault mock, read back with its
// fault and its authored block, and served as a recorded one is: a timeout
// before any answer gives the application nothing until its own timeout; a
// reset after the head gives it the head and the body bytes, then the reset.
func TestReplayFault_AnAuthoredFaultIsServed(t *testing.T) {
	m := throughDisk(t, authoredFault(&models.Fault{Kind: models.FaultTimeout}, nil))
	if m.Spec.Authored == nil || m.FaultOf() == nil || m.FaultServed() != "mock-12: no answer until the application closed, as authored" {
		t.Fatalf("read back: authored %+v, fault %+v, served %q", m.Spec.Authored, m.FaultOf(), m.FaultServed())
	}
	app, proxy := loopback(t)
	t.Cleanup(func() { _ = app.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&HTTP{Logger: zap.NewNop()}).decodeHTTP(ctx, []byte(faultReq), proxy, nil,
			&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
		_ = proxy.Close()
	}()
	if s := appGivesUp(t, app); s != (seen{"", "its own timeout"}) {
		t.Fatalf("the application saw %+v; want nothing until its own timeout", s)
	}
	cancel()
	_ = awaitReturn(t, done)

	m = throughDisk(t, authoredFault(&models.Fault{Kind: models.FaultReset, AfterHead: true, BodyBytes: 3},
		&models.HTTPResp{StatusCode: 503, Header: map[string]string{"Content-Length": "8"}, Body: "unusable"}))
	if m.Spec.Authored == nil || m.FaultOf() == nil {
		t.Fatalf("read back: authored %+v, fault %+v", m.Spec.Authored, m.FaultOf())
	}
	app, returned := replaying(t, &HTTP{Logger: zap.NewNop()}, m)
	got := seenAt(t, app)
	if !strings.HasPrefix(got.got, "HTTP/1.1 503 ") || !strings.HasSuffix(got.got, "\r\n\r\nunu") || got.end != "reset" {
		t.Errorf("the application saw %+v; want the 503's head, 3 body bytes, then a reset", got)
	}
	_ = awaitReturn(t, returned)
}
