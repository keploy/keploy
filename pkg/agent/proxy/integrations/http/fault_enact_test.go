//go:build linux

package http

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// EnactFault is what a parser that serves an HTTP mock its own way (keploy
// enterprise's SQS replay) calls before it writes the answer: a mock that
// ended in a fault gets the fault, never its head as a whole answer, and a
// whole one is left to the caller with nothing written. A delay is held
// first: once it ran its course the caller serves the answer; the run's end
// ends it, and the call, with nothing written.
func TestEnactFault_ServesTheFaultOrLeavesTheWholeAnswer(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\n"
	const delay = 50 * time.Millisecond
	cut := faultMock(&models.Fault{Kind: models.FaultTruncate, BodyBytes: 5},
		okHead("01234567890123456789", map[string]string{"Content-Length": "20"}))
	whole := faultMock(nil, okHead(`{"ok":true}`, map[string]string{"Content-Length": "11"}))
	for _, tc := range []struct {
		name     string
		mock     *models.Mock
		hook     AnswerFault
		wantOver bool
		wantErr  error
		want     seen          // what the application reads once EnactFault returned and the connection closed
		runEnded bool          // the run is over before EnactFault is called
		held     time.Duration // EnactFault returns no sooner than this
	}{
		{"a whole answer: nothing written, the caller serves it", whole, nil, false, nil, seen{"", "eof"}, false, 0},
		{"a recorded truncate: the head, 5 body bytes, the call over", cut, nil, true, nil, seen{head + "01234", "eof"}, false, 0},
		{"the hook's fault on a whole answer", whole, func(context.Context, *models.Mock) *models.Fault {
			return &models.Fault{Kind: models.FaultEOF}
		}, true, nil, seen{"", "eof"}, false, 0},
		{"a fault this keploy cannot enact: refused, nothing written", faultMock(&models.Fault{Kind: "explode"}, nil), nil,
			true, models.ErrFaultUnsupported, seen{"", "eof"}, false, 0},
		{"a delay that ran its course: nothing written, the caller serves it",
			faultMock(&models.Fault{Kind: models.FaultDelay, Delay: delay}, okHead(`{"ok":true}`, map[string]string{"Content-Length": "11"})),
			nil, false, nil, seen{"", "eof"}, false, delay},
		{"a delay once the run has ended: the call over, nothing written",
			faultMock(&models.Fault{Kind: models.FaultDelay, Delay: time.Hour}, okHead(`{"ok":true}`, map[string]string{"Content-Length": "11"})),
			nil, true, nil, seen{"", "eof"}, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &HTTP{Logger: zap.NewNop()}
			h.SetAnswerFault(tc.hook)
			app, served := loopback(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if tc.runEnded {
				cancel()
			}
			type enacted struct {
				over bool
				err  error
			}
			returned := make(chan enacted, 1)
			called := time.Now()
			go func() {
				over, err := h.EnactFault(ctx, served, tc.mock)
				returned <- enacted{over, err}
			}()
			var got enacted
			select {
			case got = <-returned:
			case <-time.After(10 * time.Second): // turns a hold past the run's end into a failure
				t.Fatal("EnactFault did not return within 10s")
			}
			if held := time.Since(called); held < tc.held {
				t.Errorf("EnactFault returned after %s, before its delay of %s", held, tc.held)
			}
			if got.over != tc.wantOver || !errors.Is(got.err, tc.wantErr) {
				t.Fatalf("EnactFault = %v, %v; want %v, %v", got.over, got.err, tc.wantOver, tc.wantErr)
			}
			_ = served.Close()
			if s := seenAt(t, app); s != tc.want {
				t.Errorf("the application saw %+v, want %+v", s, tc.want)
			}
		})
	}
}
