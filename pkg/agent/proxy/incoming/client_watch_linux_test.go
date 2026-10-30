//go:build linux

package proxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/config"
)

// An exchange whose client hung up before the answer's end is not recorded,
// in either loop: the client did not take the answer, and the app's end of it
// is its being told the client went (watchClient). Its capture, were one
// started, is over once captures is waited on, so the check is not a race
// with it.
func TestAnExchangeItsClientLeftIsNotRecorded(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous %v", synchronous), func(t *testing.T) {
			t.Parallel()
			gone := make(chan struct{})
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				fmt.Fprintln(w, `{"status":"RUNNING"}`)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(gone)
			})}
			appLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go func() { _ = srv.Serve(appLn) }()
			t.Cleanup(func() { _ = srv.Close() })

			cfg := &config.Config{}
			cfg.Agent.MockMode = true
			cfg.Agent.RecordRequests = true
			cfg.Agent.Synchronous = synchronous
			pm := New(zap.NewNop(), nil, cfg)

			client, proxied := tcpPairForWatch(t)
			served := make(chan struct{})
			go func() {
				defer close(served)
				defer proxied.Close() // as the caller does once the handler returns
				pm.ServeLive(context.Background(), proxied, appLn.Addr().String(), 8083)
			}()
			if _, err := client.Write([]byte("GET /client/v1/jobs/j/events HTTP/1.1\r\nHost: api\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			br := bufio.NewReader(client)
			for {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("the answer's first event: %v", err)
				}
				if strings.Contains(line, "RUNNING") {
					break
				}
			}
			_ = client.Close()

			// A hang fails at the test binary's deadline, not at a budget
			// of its own.
			never := make(<-chan time.Time)
			if d, ok := t.Deadline(); ok {
				never = time.After(time.Until(d) - 5*time.Second)
			}
			for _, w := range []struct {
				what string
				c    <-chan struct{}
			}{{"the app saw its client gone", gone}, {"the handler ended", served}} {
				select {
				case <-w.c:
				case <-never:
					t.Fatalf("never: %s", w.what)
				}
			}
			pm.captures.Wait()
			select {
			case tc := <-pm.TCChan():
				t.Fatalf("the exchange its client left was recorded: %s %s", tc.HTTPReq.Method, tc.HTTPReq.URL)
			default:
			}
		})
	}
}

// tcpPairForWatch is a connected TCP pair: the client's end, and the proxy's.
func tcpPairForWatch(t *testing.T) (client, proxied net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	proxied = <-accepted
	if proxied == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = client.Close(); _ = proxied.Close() })
	return client, proxied
}
