package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// takenRequest is one request the ingress gave the learn step.
type takenRequest struct {
	req    *models.HTTPReq
	unread string
	at     time.Time
}

// requestLog is a RequestTaker that wants requests while wants is set and
// keeps each one given.
type requestLog struct {
	wants atomic.Bool
	mu    sync.Mutex
	got   []takenRequest
	taken atomic.Int32
}

func (l *requestLog) WantsIncoming() bool { return l.wants.Load() }

func (l *requestLog) TakeIncoming(req *models.HTTPReq, unread string, at time.Time) {
	l.mu.Lock()
	l.got = append(l.got, takenRequest{req: req, unread: unread, at: at})
	l.mu.Unlock()
	l.taken.Add(1)
}

func (l *requestLog) list() []takenRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]takenRequest(nil), l.got...)
}

// readRequest is raw read as the ingress reads a request.
func readRequest(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// A client test's request is given to the learn step before the application
// is given any byte of it, in both of the ingress's HTTP/1 loops: no call the
// request leads the application to make can read the test's places without
// it. The application still gets the request whole, its body read ahead and
// put back.
func TestTheLearnStepTakesAClientTestsRequestBeforeTheAppGetsAByteOfIt(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	const body = `{"name":"app-1762700000123456789"}`
	for _, mode := range []string{"sync", "zero-copy"} {
		t.Run(mode, func(t *testing.T) {
			log := &requestLog{}
			log.wants.Store(true)
			takenFirst := make(chan bool, 1)
			gotBody := make(chan string, 1)
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				r := bufio.NewReader(c)
				if _, err := r.Peek(1); err != nil {
					return
				}
				takenFirst <- log.taken.Load() == 1
				req, err := http.ReadRequest(r)
				if err != nil {
					return
				}
				b, _ := io.ReadAll(req.Body)
				gotBody <- string(b)
				_, _ = io.WriteString(c, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}()
			pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: mode == "sync"}
			pm.SetRequestTaker(log)
			ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())), 10*time.Second)
			defer cancel()
			client, done := ingressPipe(t, ctx, pm, ln.Addr().String())
			go func() {
				for range pm.tcChan {
				}
			}()
			raw := fmt.Sprintf("POST /apps?x=1 HTTP/1.1\r\nHost: api\r\nCookie: jwt=a.b.c\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			if _, err := io.WriteString(client, raw); err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !<-takenFirst {
				t.Fatal("the application got the request's first byte before the learn step took it")
			}
			if got := <-gotBody; got != body {
				t.Fatalf("the application got the body %q, want %q", got, body)
			}
			got := log.list()
			if len(got) != 1 || got[0].unread != "" || got[0].at.IsZero() {
				t.Fatalf("taken %+v", got)
			}
			r := got[0].req
			if r.Method != "POST" || r.URL != "http://api/apps?x=1" || r.Body != body || r.Header["Cookie"] != "jwt=a.b.c" || !r.Timestamp.Equal(got[0].at) {
				t.Fatalf("taken request %+v", r)
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("the ingress did not end the exchange")
			}
		})
	}
}

// A request whose body keploy does not read before the application is given
// it is taken as unread, saying why, and forwarded as it came: the learn step
// reads nothing of it rather than part of it.
func TestTakeRequestSaysWhyItCannotReadARequest(t *testing.T) {
	for _, c := range []struct{ name, raw, why string }{
		{"continue", "POST /u HTTP/1.1\r\nHost: api\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\nab", "100 (Continue)"},
		{"chunked", "POST /u HTTP/1.1\r\nHost: api\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nab\r\n0\r\n\r\n", "declares no length"},
		{"large", fmt.Sprintf("POST /u HTTP/1.1\r\nHost: api\r\nContent-Length: %d\r\n\r\nab", maxTakenBody+1), "larger than"},
		{"encoded", "POST /u HTTP/1.1\r\nHost: api\r\nContent-Encoding: gzip\r\nContent-Length: 2\r\n\r\nab", "encoded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &requestLog{}
			log.wants.Store(true)
			pm := &IngressProxyManager{logger: zap.NewNop()}
			pm.SetRequestTaker(log)
			req := readRequest(t, c.raw)
			body := req.Body
			pm.takeRequest(req, time.Now())
			got := log.list()
			if len(got) != 1 || !strings.Contains(got[0].unread, c.why) || got[0].req.Body != "" {
				t.Fatalf("taken %+v, want one unread for %q", got, c.why)
			}
			if req.Body != body {
				t.Fatal("a body keploy does not read was read ahead")
			}
		})
	}
}

// A body its client did not send whole gives the learn step nothing, and
// the forward reads the part that came and then the same end it would have
// met without the read ahead. A taker that wants no request now is given
// none, and the body is left as it came.
func TestTakeRequestGivesNothingOfARequestItsClientDidNotSendWhole(t *testing.T) {
	log := &requestLog{}
	log.wants.Store(true)
	pm := &IngressProxyManager{logger: zap.NewNop()}
	pm.SetRequestTaker(log)
	req := readRequest(t, "POST /u HTTP/1.1\r\nHost: api\r\nContent-Length: 10\r\n\r\nabcd")
	pm.takeRequest(req, time.Now())
	if got := log.list(); len(got) != 0 {
		t.Fatalf("a body its client left gave %+v", got)
	}
	b, err := io.ReadAll(req.Body)
	if string(b) != "abcd" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("the forward read %q, %v; want the part that came and an unexpected EOF", b, err)
	}
	if err := req.Body.Close(); err != nil {
		t.Fatal(err)
	}
	// A body that ends short of its declared length with no error of its own
	// is not sent whole either.
	req = readRequest(t, "POST /u HTTP/1.1\r\nHost: api\r\nContent-Length: 2\r\n\r\nab")
	req.Body, req.ContentLength = io.NopCloser(strings.NewReader("ab")), 5
	pm.takeRequest(req, time.Now())
	if got := log.list(); len(got) != 0 {
		t.Fatalf("a body that ended short gave %+v", got)
	}
	if b, err := io.ReadAll(req.Body); string(b) != "ab" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("the forward read %q, %v; want the part that came and an unexpected EOF", b, err)
	}

	log.wants.Store(false)
	req = readRequest(t, "POST /u HTTP/1.1\r\nHost: api\r\nContent-Length: 2\r\n\r\nab")
	body := req.Body
	pm.takeRequest(req, time.Now())
	if got := log.list(); len(got) != 0 || req.Body != body {
		t.Fatalf("a taker that wants none was given %+v", got)
	}
	pm.SetRequestTaker(nil)
	pm.takeRequest(req, time.Now())
	if req.Body != body {
		t.Fatal("an ingress with no taker read a body ahead")
	}
}

// A request's body is taken as a recording reads it: a form sent URL-encoded
// is unescaped, a body of no length read as empty, and a request with none
// still taken.
func TestTakeRequestReadsABodyAsARecordingDoes(t *testing.T) {
	log := &requestLog{}
	log.wants.Store(true)
	pm := &IngressProxyManager{logger: zap.NewNop()}
	pm.SetRequestTaker(log)
	pm.takeRequest(readRequest(t, "POST /f HTTP/1.1\r\nHost: api\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 13\r\n\r\nname=app%2D12"), time.Now())
	get := readRequest(t, "GET /s/12 HTTP/1.1\r\nHost: api\r\n\r\n")
	pm.takeRequest(get, time.Now())
	if get.Body != http.NoBody {
		t.Fatal("a request with no body was given one to forward")
	}
	pm.takeRequest(readRequest(t, "POST /f HTTP/1.1\r\nHost: api\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 3\r\n\r\n%zz"), time.Now())
	got := log.list()
	if len(got) != 3 || got[0].req.Body != "name=app-12" || got[1].req.Body != "" || got[1].unread != "" || got[1].req.URL != "http://api/s/12" ||
		got[2].req.Body != "%zz" {
		t.Fatalf("taken %+v", got)
	}
}
