//go:build linux

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"
)

// A test's client that hangs up on a live call while the app's answer still
// streams (a job's event stream the client stops following) is told to the
// app, as it would be without keploy, and the call's connection settles: the
// test's end does not wait on it. Before, the HTTP/1 ingress read only the
// app while it forwarded the answer: an app that sent nothing more (it waits
// for events that never come) held the handler, so the app never learned its
// client had gone and the connection held work for good. Its socket in
// CLOSE_WAIT, it held a request not answered; once the app wrote again and
// the client's kernel reset the socket (TCP_CLOSE), its counts took the
// client's FIN for one byte no read returned, and the settle's watchdog gave
// up on it ("held 1 byte(s) the application had sent that its handler had not
// read"), leaving the test not judged, its window open for the app's
// background calls to fill.
func TestALiveClientsHangUpMidAnswerReachesTheAppAndSettles(t *testing.T) {
	for _, tc := range []struct {
		name string
		// again: the app writes once more after the client hung up.
		again bool
	}{
		{"the client's FIN, the app quiet", false},
		{"the client's FIN, then a line its kernel resets", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gone, hungUp := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(sync.OnceFunc(func() { close(release) }))
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				if tc.again {
					select {
					case <-hungUp:
					case <-release:
						return
					}
					fmt.Fprintln(w, `{"status":"STILL RUNNING"}`)
					w.(http.Flusher).Flush()
				}
				select {
				case <-r.Context().Done():
					close(gone)
				case <-release:
				}
			})}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			cfg := &config.Config{}
			cfg.Agent.MockMode = true
			cfg.Agent.RecordRequests = true
			pm := incoming.New(zap.NewNop(), nil, cfg)

			tr := &inflight{}
			app, proxy := acceptedPair(t, tr)
			served := make(chan struct{})
			go func() {
				defer close(served)
				pm.ServeLive(context.Background(), proxy, ln.Addr().String(), 8083)
			}()
			if _, err := app.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(app)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("the answer's first event: %v", err)
				}
				if strings.Contains(line, "RUNNING") {
					break
				}
			}
			_ = app.Close()
			close(hungUp)

			// A hang that never ends fails at the test binary's deadline,
			// not at a budget of its own (the cases run in parallel, so
			// each has all of it).
			never := make(<-chan time.Time)
			if d, ok := t.Deadline(); ok {
				never = time.After(time.Until(d) - 5*time.Second)
			}
			state := func() string {
				_, st, ok := socketReceived(proxy.raw)
				v := proxy.view()
				return fmt.Sprintf("socket state %d (asked: %v), delivered %d, untaken %d, eof %v, reading %d, pending in a replay %d",
					st, ok, v.delivered, proxy.untaken(v), v.eof, v.reading, tr.Pending(true))
			}
			select {
			case <-gone:
			case <-never:
				t.Fatalf("the app never saw its client gone: %s", state())
			}
			select {
			case <-served:
			case <-never:
				t.Fatalf("the live call's handler never ended: %s", state())
			}
			if n := tr.Pending(true); n != 0 {
				t.Fatalf("the hung-up live call still holds work in a replay: %s", state())
			}
			if n := tr.Pending(false); n != 0 {
				t.Fatalf("the hung-up live call still holds work in a record: %s", state())
			}
		})
	}
}
