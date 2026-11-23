//go:build linux || darwin || windows

package http

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A hold past the app's FIN (a fault whose recording found the app had
// ended its side first) waits for a reset or the run's end, because a FIN
// cannot say whether the app closed or only half-closed and reads on. Once
// the proxy finds that no process holds the app's socket any more (it closed
// it, or exited), nothing can read on: the proxy says so on the connection
// (integrations.AppClosed), and the hold ends then, so the proxy's close of
// the connection is counted while the session's last read waits for it
// (PairedBytesDrain). Held on until the run's end, the close came after that
// read had given up on it, and the run was left with no verdict.

// closedMarks is idleMarks that also carries the proxy's word that the app's
// socket has no process holding it (integrations.AppClosed): closed is
// closed when the test says so.
type closedMarks struct {
	*idleMarks
	closed chan struct{}
}

func (m *closedMarks) AppClosed() <-chan struct{} { return m.closed }

// replayClosable is replayTo with a request holder that can say the app's
// socket is closed, and an observed logger so a caller can wait for the
// answer to be served before sending the app's FIN.
func replayClosable(t *testing.T, m *models.Mock, request string, finFirst bool) (heldReplay, *closedMarks, *observer.ObservedLogs) {
	t.Helper()
	app, proxy := tcpPair(t)
	t.Cleanup(func() { _ = app.Close() })
	if _, err := app.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(request))
	for n := 0; n < len(request); {
		k, err := proxy.Read(buf[n:])
		if err != nil {
			t.Fatal(err)
		}
		n += k
	}
	if finFirst {
		if err := app.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	core, logs := observer.New(zapcore.DebugLevel)
	marks := &closedMarks{idleMarks: newIdleMarks(), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(integrations.WithRequestHolder(context.Background(), marks))
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		err := (&HTTP{Logger: zap.New(core)}).decodeHTTP(ctx, []byte(request), proxy, nil,
			&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
		_ = proxy.Close()
		done <- err
	}()
	return heldReplay{app: app, proxy: proxy, marks: marks.idleMarks, done: done, stop: cancel}, marks, logs
}

func TestReplay_AHoldPastTheAppsFINEndsOnceItsSocketIsClosed(t *testing.T) {
	timeout := func(*testing.T) *models.Mock {
		return faultMock(&models.Fault{Kind: models.FaultTimeout, AppEndedFirst: true,
			Observed: "the application closed the connection with no answer"}, nil)
	}
	stopped := func(t *testing.T) *models.Mock { return halfClosedAtRecord(t, partAnswers[0].read, false) }
	for _, row := range []struct {
		name, request, body string
		mock                func(*testing.T) *models.Mock
	}{
		{"timeout before the head (a long poll the app gave up)", faultReq, "", timeout},
		{"stopped_reading", jdkRequest(7), partAnswers[0].body, stopped},
	} {
		for _, finFirst := range []bool{true, false} {
			order := map[bool]string{true: "its FIN before the answer", false: "its FIN after the answer"}[finFirst]
			for _, proven := range []string{"", "1"} {
				t.Run(row.name+"/"+order+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
					t.Setenv("KEPLOY_PROVEN", proven)
					m := throughDisk(t, row.mock(t))
					if m.Spec.Fault == nil || !m.Spec.Fault.AppEndedFirst {
						t.Fatalf("the recording %+v is not marked as an app that ended its side first", m.Spec.Fault)
					}
					r, marks, logs := replayClosable(t, m, row.request, finFirst)
					if !finFirst {
						if row.body != "" {
							readsUntilItsTimeout(t, r.app, row.body)
						} else {
							awaitServed(t, logs)
						}
						if err := r.app.(*net.TCPConn).CloseWrite(); err != nil {
							t.Fatal(err)
						}
					}
					r.held(t)
					r.stillHeld(t, "past the app's FIN, before the proxy found its socket closed")
					close(marks.closed)
					r.ended(t, "the proxy found no process holds the app's socket")
				})
			}
		}
	}
}
