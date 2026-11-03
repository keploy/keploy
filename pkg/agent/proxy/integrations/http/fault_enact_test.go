//go:build linux

package http

import (
	"context"
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// EnactFault is what a parser that serves an HTTP mock its own way (keploy
// enterprise's SQS replay) calls before it writes the answer: a mock that
// ended in a fault gets the fault, never its head as a whole answer, and a
// whole one is left to the caller with nothing written.
func TestEnactFault_ServesTheFaultOrLeavesTheWholeAnswer(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	cut := faultMock(&models.Fault{Kind: models.FaultTruncate, BodyBytes: 5},
		okHead("01234567890123456789", map[string]string{"Content-Length": "20"}))
	whole := faultMock(nil, okHead(`{"ok":true}`, map[string]string{"Content-Length": "11"}))
	for _, tc := range []struct {
		name     string
		mock     *models.Mock
		hook     AnswerFault
		wantOver bool
		wantErr  error
		want     seen // what the application reads once EnactFault returned and the connection closed
	}{
		{"a whole answer: nothing written, the caller serves it", whole, nil, false, nil, seen{"", "eof"}},
		{"a recorded truncate: the head, 5 body bytes, the call over", cut, nil, true, nil, seen{head + "01234", "eof"}},
		{"the hook's fault on a whole answer", whole, func(context.Context, *models.Mock) *models.Fault {
			return &models.Fault{Kind: models.FaultEOF}
		}, true, nil, seen{"", "eof"}},
		{"a fault this keploy cannot enact: refused, nothing written", faultMock(&models.Fault{Kind: "explode"}, nil), nil,
			true, models.ErrFaultUnsupported, seen{"", "eof"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &HTTP{Logger: zap.NewNop()}
			h.SetAnswerFault(tc.hook)
			app, served := loopback(t)
			over, err := h.EnactFault(context.Background(), served, tc.mock)
			if over != tc.wantOver || !errors.Is(err, tc.wantErr) {
				t.Fatalf("EnactFault = %v, %v; want %v, %v", over, err, tc.wantOver, tc.wantErr)
			}
			_ = served.Close()
			if s := seenAt(t, app); s != tc.want {
				t.Errorf("the application saw %+v, want %+v", s, tc.want)
			}
		})
	}
}
