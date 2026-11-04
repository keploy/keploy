package http

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// At replay, the hold of an answer whose recording found that the app had
// ended its side first (models.Fault.AppEndedFirst, which a recording writes
// on each stopped_reading and timeout it judges by the app's end) holds past
// the app's FIN, sent before the answer or after it: a FIN cannot say whether
// the app closed or only half-closed and reads on. Only a reset or the run's
// end ends it. No look at the app's socket decides it, so it holds the same
// where the socket cannot be asked (Windows). A fault without the mark ends
// the hold at a FIN after the answer, as before.

// idleMarks is a request holder (integrations.WithRequestHolder) that keeps
// the idle marks a serve takes (integrations.MarkIdle), as the proxy's kept
// connection does: marked closes once one is taken, released once it is
// let go.
type idleMarks struct {
	marked, released chan struct{}
	mark, release    sync.Once
}

func newIdleMarks() *idleMarks {
	return &idleMarks{marked: make(chan struct{}), released: make(chan struct{})}
}

func (m *idleMarks) Hold() func() { return func() {} }

func (m *idleMarks) MarkIdle() func() {
	m.mark.Do(func() { close(m.marked) })
	return func() { m.release.Do(func() { close(m.released) }) }
}

func markedAt(m *idleMarks) bool {
	select {
	case <-m.marked:
		return true
	default:
		return false
	}
}

// readsUntilDeadline is what the app reads on c until its own timeout, in
// d, or an end of c, and how the read ended.
func readsUntilDeadline(c net.Conn, d time.Duration) (string, error) {
	_ = c.SetReadDeadline(time.Now().Add(d))
	var got []byte
	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			return string(got), err
		}
	}
}

func TestAHoldOfAnAppThatEndedItsSideFirstOutlastsItsFIN(t *testing.T) {
	const request = "GET /charge HTTP/1.1\r\nHost: pay.example\r\n\r\n"
	const head = "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\n"
	for _, kind := range []struct {
		name  string
		fault models.Fault
		want  string // what the app reads
	}{
		{"stopped_reading", models.Fault{Kind: models.FaultStoppedReading, AfterHead: true, BodyBytes: 5, ContentLength: 10}, head + "01234"},
		{"a timeout after the head", models.Fault{Kind: models.FaultTimeout, AfterHead: true, BodyBytes: 5}, head + "01234"},
		{"a timeout before the head", models.Fault{Kind: models.FaultTimeout}, ""},
	} {
		for _, marked := range []bool{true, false} {
			for _, end := range []string{"its FIN before the answer", "its FIN after the answer", "its reset"} {
				if !marked && end == "its FIN before the answer" {
					continue // the look at the socket decides it (replay_half_close_test.go)
				}
				for _, proven := range []string{"", "1"} {
					name := kind.name + "/app_ended_first=" +
						map[bool]string{true: "yes", false: "no"}[marked] + "/" + end + "/KEPLOY_PROVEN=" + proven
					t.Run(name, func(t *testing.T) {
						t.Setenv("KEPLOY_PROVEN", proven)
						f := kind.fault
						f.AppEndedFirst = marked
						m := &models.Mock{Name: "mock-held", Kind: models.HTTP, Spec: models.MockSpec{
							HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://pay.example/charge", ProtoMajor: 1, ProtoMinor: 1,
								Header: map[string]string{"Host": "pay.example"}},
							HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Length": "10"}, Body: "0123456789"},
							Fault:    &f,
						}}
						app, proxy := tcpPair(t)
						t.Cleanup(func() { _ = app.Close() })
						if _, err := app.Write([]byte(request)); err != nil {
							t.Fatal(err)
						}
						if _, err := readFullConn(proxy, len(request)); err != nil {
							t.Fatal(err)
						}
						if end == "its FIN before the answer" {
							if err := app.(*net.TCPConn).CloseWrite(); err != nil {
								t.Fatal(err)
							}
						}
						marks := newIdleMarks()
						ctx, cancel := context.WithCancel(integrations.WithRequestHolder(context.Background(), marks))
						t.Cleanup(cancel)
						done := make(chan error, 1)
						go func() {
							err := (&HTTP{Logger: zap.NewNop()}).decodeHTTP(ctx, []byte(request), proxy, nil,
								&mockMemDb{mocks: []*models.Mock{m}, updateUnFilteredReturn: true}, models.OutgoingOptions{})
							_ = proxy.Close()
							done <- err
						}()
						got, err := readsUntilDeadline(app, 200*time.Millisecond)
						if got != kind.want || !errors.Is(err, os.ErrDeadlineExceeded) {
							t.Fatalf("the app read %q, then %v; want %q, then nothing until its own timeout", got, err, kind.want)
						}
						switch end {
						case "its reset":
							if err := app.(*net.TCPConn).SetLinger(0); err != nil {
								t.Fatal(err)
							}
							_ = app.Close()
						case "its FIN after the answer":
							if err := app.(*net.TCPConn).CloseWrite(); err != nil {
								t.Fatal(err)
							}
						}
						if end == "its reset" || !marked {
							select {
							case err := <-done:
								if err != nil {
									t.Fatalf("decodeHTTP = %v once the app ended (%s), want nil", err, end)
								}
							case <-time.After(10 * time.Second): // turns a hang into a failure
								t.Fatalf("decodeHTTP did not return at %s", end)
							}
							if markedAt(marks) {
								t.Fatalf("the serve marked the connection idle at %s; want it let go", end)
							}
							return
						}
						select {
						case <-marks.marked:
						case err := <-done:
							t.Fatalf("decodeHTTP returned %v at %s: the app is let go with an end it may never have had", err, end)
						case <-time.After(10 * time.Second): // turns a hang into a failure
							t.Fatalf("the serve neither held the connection past %s nor returned", end)
						}
						select {
						case err := <-done:
							t.Fatalf("decodeHTTP returned %v past %s, before the run's end; want the connection held", err, end)
						case <-marks.released:
							t.Fatalf("the idle mark was released past %s, before the run's end", end)
						default:
						}
						cancel()
						select {
						case err := <-done:
							if err != nil && !errors.Is(err, context.Canceled) {
								t.Fatalf("decodeHTTP = %v at the run's end", err)
							}
						case <-time.After(10 * time.Second): // turns a hang into a failure
							t.Fatal("decodeHTTP did not return at the run's end")
						}
						select {
						case <-marks.released:
						default:
							t.Fatal("decodeHTTP returned at the run's end with its idle mark still taken")
						}
					})
				}
			}
		}
	}
}

// readFullConn reads n bytes from c, bounded, so a hang fails the row.
func readFullConn(c net.Conn, n int) (string, error) {
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	var b strings.Builder
	buf := make([]byte, n)
	for b.Len() < n {
		k, err := c.Read(buf[:n-b.Len()])
		b.Write(buf[:k])
		if err != nil {
			return b.String(), err
		}
	}
	return b.String(), nil
}
