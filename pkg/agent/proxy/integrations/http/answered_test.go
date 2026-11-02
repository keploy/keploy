package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A replay that answers a call with its recording tells the answer-written
// listener the recording's name once the answer was written: the application
// can already read it whole then. It is the event a deliver: entry waits for.
func TestAMatchedAnswerTellsItsListenerOnceWritten(t *testing.T) {
	pay := recorded("mock-3", "/v1/charges", nil, "amount=5")
	pay.Spec.HTTPReq.ProtoMajor, pay.Spec.HTTPReq.ProtoMinor = 1, 1
	app, proxy := tcpPair(t)
	told := make(chan string, 1)
	integrations.SetAnswerWritten(func(m *models.Mock) {
		// The bound only turns a hang into a failure.
		told <- m.Name + " " + readUntil(app, `{"ok":true}`, time.Minute)
	})
	t.Cleanup(func() { integrations.SetAnswerWritten(nil) })
	h := &HTTP{Logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() {
		returned <- h.decodeHTTP(ctx, []byte("POST /v1/charges HTTP/1.1\r\nHost: api.example\r\nContent-Length: 8\r\n\r\namount=5"), proxy, nil,
			&mockMemDb{mocks: []*models.Mock{pay}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
	}()
	t.Cleanup(func() { cancel(); <-returned })
	select {
	case got := <-told:
		if !strings.HasPrefix(got, "mock-3 HTTP/1.1 200") || !strings.HasSuffix(got, `{"ok":true}`) {
			t.Fatalf("the listener was told %q, want mock-3 with its whole answer already written", got)
		}
	case <-time.After(time.Minute):
		t.Fatal("the listener was never told of the answer written")
	}
}
