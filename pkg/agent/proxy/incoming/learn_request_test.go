package proxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// takenRequest is one request the ingress gave the learn step.
type takenRequest struct {
	req    *models.HTTPReq
	unread string
	at     time.Time
	// coming: taken by TakeComing, its done not called yet; dones: how
	// many times its done was called.
	coming bool
	dones  int
}

// requestLog is a RequestTaker that wants requests while wants is set and
// keeps each one given.
type requestLog struct {
	// noComing: TakeComing takes nothing (a nil done).
	noComing bool
	wants    atomic.Bool
	mu       sync.Mutex
	got      []takenRequest
	taken    atomic.Int32
}

func (l *requestLog) WantsIncoming() bool { return l.wants.Load() }

func (l *requestLog) TakeIncoming(req *models.HTTPReq, unread string, at time.Time) {
	l.mu.Lock()
	l.got = append(l.got, takenRequest{req: req, unread: unread, at: at})
	l.mu.Unlock()
	l.taken.Add(1)
}

func (l *requestLog) TakeComing(req *models.HTTPReq, at time.Time) func(*models.HTTPReq, string) {
	if l.noComing {
		return nil
	}
	l.mu.Lock()
	k := len(l.got)
	l.got = append(l.got, takenRequest{req: req, at: at, coming: true})
	l.mu.Unlock()
	l.taken.Add(1)
	return func(r *models.HTTPReq, unread string) {
		l.mu.Lock()
		l.got[k] = takenRequest{req: r, unread: unread, at: at, dones: l.got[k].dones + 1}
		l.mu.Unlock()
	}
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
				defer func() { _ = c.Close() }()
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
		{"large", fmt.Sprintf("POST /u HTTP/1.1\r\nHost: api\r\nContent-Length: %d\r\n\r\nab", maxTakenBody+1), "larger than"},
		{"encoded so a recording keeps it as sent", postWith("Content-Encoding: deflate\r\n", "ab"), "Content-Encoding deflate), which a recording keeps"},
		{"gzip that does not decode", postWith("Content-Encoding: gzip\r\n", "ab"), "does not decode as its Content-Encoding gzip says"},
		{"a form part sending a file", postWith("Content-Type: multipart/form-data; boundary=XX\r\n",
			"--XX\r\nContent-Disposition: form-data; name=\"f\"; filename=\"a.txt\"\r\n\r\nabc\r\n--XX--\r\n"), "form part f sends a file"},
		{"a form of no boundary", postWith("Content-Type: multipart/form-data\r\n", "ab"), "names no boundary"},
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
			if c.name == "large" && req.Body != body {
				t.Fatal("a body larger than keploy reads was read ahead")
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

// postWith is a POST /u request with the header lines headers (each ending
// "\r\n") and body, its length declared.
func postWith(headers, body string) string {
	return fmt.Sprintf("POST /u HTTP/1.1\r\nHost: api\r\n%sContent-Length: %d\r\n\r\n%s", headers, len(body), body)
}

// A body sent gzip or br is taken decoded, as a recording decodes it, and
// forwarded as its client sent it. One that decodes larger than keploy reads
// is taken as unread.
func TestTakeRequestDecodesABodySentGzipOrBr(t *testing.T) {
	const text = `{"name":"app-1762758102000000000"}`
	var gz, br bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(text))
	_ = zw.Close()
	bw := brotli.NewWriter(&br)
	_, _ = bw.Write([]byte(text))
	_ = bw.Close()
	var big bytes.Buffer
	zw = gzip.NewWriter(&big)
	_, _ = zw.Write(bytes.Repeat([]byte("a"), maxTakenBody+1))
	_ = zw.Close()
	for _, c := range []struct {
		name, enc string
		sent      []byte
		body, why string
	}{
		{"gzip", "gzip", gz.Bytes(), text, ""},
		{"br", "BR", br.Bytes(), text, ""},
		{"gzip larger than keploy reads once decoded", "gzip", big.Bytes(), "", "decoded, its body is larger than"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &requestLog{}
			log.wants.Store(true)
			pm := &IngressProxyManager{logger: zap.NewNop()}
			pm.SetRequestTaker(log)
			req := readRequest(t, postWith("Content-Type: application/json\r\nContent-Encoding: "+c.enc+"\r\n", string(c.sent)))
			pm.takeRequest(req, time.Now())
			got := log.list()
			if len(got) != 1 || got[0].req.Body != c.body || !strings.HasPrefix(got[0].unread, c.why) || c.why == "" && got[0].unread != "" {
				t.Fatalf("taken %+v, want body %q, unread %q", got, c.body, c.why)
			}
			if fwd, err := io.ReadAll(req.Body); err != nil || !bytes.Equal(fwd, c.sent) {
				t.Fatalf("the forward read %d bytes (%v), want the %d its client sent", len(fwd), err, len(c.sent))
			}
		})
	}
}

// A multipart form is taken as a recording takes it apart: its text parts
// in the order sent, none of no name, its Content-Type without its
// boundary, which the forward still carries.
func TestTakeRequestReadsAMultipartFormsTextParts(t *testing.T) {
	log := &requestLog{}
	log.wants.Store(true)
	pm := &IngressProxyManager{logger: zap.NewNop()}
	pm.SetRequestTaker(log)
	form := "--XX\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\napp-1762758102\r\n" +
		"--XX\r\nContent-Disposition: form-data; name=\"suite\"\r\n\r\nsuite-1\r\n" +
		"--XX\r\nContent-Disposition: form-data\r\n\r\nno name\r\n" +
		"--XX\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\napp-2\r\n--XX--\r\n"
	req := readRequest(t, postWith("Content-Type: multipart/form-data; boundary=XX\r\n", form))
	pm.takeRequest(req, time.Now())
	got := log.list()
	want := []models.FormData{{Key: "name", Values: []string{"app-1762758102"}}, {Key: "suite", Values: []string{"suite-1"}}, {Key: "name", Values: []string{"app-2"}}}
	if len(got) != 1 || got[0].unread != "" || got[0].req.Body != "" || !reflect.DeepEqual(got[0].req.Form, want) || got[0].req.Header["Content-Type"] != "multipart/form-data" {
		t.Fatalf("taken %+v, want the form's text parts %+v", got, want)
	}
	if req.Header.Get("Content-Type") != "multipart/form-data; boundary=XX" {
		t.Fatalf("the forward carries Content-Type %q", req.Header.Get("Content-Type"))
	}
	if fwd, _ := io.ReadAll(req.Body); string(fwd) != form {
		t.Fatalf("the forward read %q", fwd)
	}
}

// oneByte reads its reader a byte at a time.
type oneByte struct{ io.Reader }

func (r oneByte) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

// A body of no declared length (chunked) is not read ahead: it is taken as
// still coming, and given whole as the forward's read meets its end, before
// the forward sends the last chunk the application waits for. One larger
// than keploy reads, and one closed before its end, are given as unread.
func TestTakeRequestReadsAChunkedBodyAsItPasses(t *testing.T) {
	log := &requestLog{}
	log.wants.Store(true)
	pm := &IngressProxyManager{logger: zap.NewNop()}
	pm.SetRequestTaker(log)
	req := readRequest(t, "POST /u HTTP/1.1\r\nHost: api\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n4\r\n{\"n\"\r\n7\r\n:\"ab-1\"\r\n1\r\n}\r\n0\r\n\r\n")
	pm.takeRequest(req, time.Now())
	if got := log.list(); len(got) != 1 || !got[0].coming {
		t.Fatalf("taken %+v, want one still coming", got)
	}
	r := oneByte{req.Body}
	var fwd []byte
	b := make([]byte, 8)
	for {
		n, err := r.Read(b)
		fwd = append(fwd, b[:n]...)
		if got := log.list(); err == nil && (len(got) != 1 || !got[0].coming) {
			t.Fatalf("after %q of the body, taken %+v: given before its end", fwd, got)
		}
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
	}
	_ = req.Body.Close()
	got := log.list()
	if string(fwd) != `{"n":"ab-1"}` || len(got) != 1 || got[0].coming || got[0].dones != 1 || got[0].unread != "" || got[0].req.Body != `{"n":"ab-1"}` {
		t.Fatalf("forwarded %q, taken %+v; want the whole body given once", fwd, got)
	}

	// One of exactly the bytes keploy reads is read whole.
	req = readRequest(t, "POST /u HTTP/1.1\r\nHost: api\r\nTransfer-Encoding: chunked\r\n\r\n"+fmt.Sprintf("%x\r\n%s\r\n", maxTakenBody, strings.Repeat("a", maxTakenBody))+"0\r\n\r\n")
	log = &requestLog{}
	log.wants.Store(true)
	pm.SetRequestTaker(log)
	pm.takeRequest(req, time.Now())
	_, _ = io.ReadAll(req.Body)
	if got := log.list(); len(got) != 1 || got[0].unread != "" || len(got[0].req.Body) != maxTakenBody {
		t.Fatalf("a body of exactly %d bytes: taken %d entries, unread %q", maxTakenBody, len(got), got[0].unread)
	}

	// A taker that takes nothing of it leaves the body as it came.
	log = &requestLog{noComing: true}
	log.wants.Store(true)
	pm.SetRequestTaker(log)
	req = readRequest(t, "POST /u HTTP/1.1\r\nHost: api\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nab\r\n0\r\n\r\n")
	body := req.Body
	pm.takeRequest(req, time.Now())
	if req.Body != body {
		t.Fatal("a body no taker takes was wrapped")
	}

	for _, c := range []struct {
		name, raw, why string
		readAll        bool
	}{
		{"larger than keploy reads", "POST /u HTTP/1.1\r\nHost: api\r\nTransfer-Encoding: chunked\r\n\r\n" + fmt.Sprintf("%x\r\n%s\r\n", maxTakenBody+1, strings.Repeat("a", maxTakenBody+1)) + "0\r\n\r\n", "its body is larger than", true},
		{"closed before its end", "POST /u HTTP/1.1\r\nHost: api\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nab\r\n0\r\n\r\n", "closed before its end", false},
		{"cut short", "POST /u HTTP/1.1\r\nHost: api\r\nTransfer-Encoding: chunked\r\n\r\n4\r\nab", "did not come whole: unexpected EOF", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &requestLog{}
			log.wants.Store(true)
			pm := &IngressProxyManager{logger: zap.NewNop()}
			pm.SetRequestTaker(log)
			req := readRequest(t, c.raw)
			pm.takeRequest(req, time.Now())
			if c.readAll {
				_, _ = io.ReadAll(req.Body)
			}
			cb, ok := req.Body.(*comingBody)
			if !ok {
				t.Fatal("the body was not read as it passed")
			}
			if len(cb.got) > maxTakenBody {
				t.Fatalf("the body read as it passed holds %d bytes, more than the %d keploy reads", len(cb.got), maxTakenBody)
			}
			_ = req.Body.Close()
			got := log.list()
			if len(got) != 1 || got[0].coming || got[0].dones != 1 || !strings.Contains(got[0].unread, c.why) {
				t.Fatalf("taken %+v, want one unread for %q, given once", got, c.why)
			}
		})
	}
}

// A body its client sends only once the application answered 100
// (Continue) is not read ahead: the application, not keploy, answers the
// client. It is taken as still coming, and given whole before the read that
// brings its last declared byte returns, so before the forward gives it to
// the application. One its client sent less of than it declared is given as
// unread.
func TestTakeRequestReadsABodyAfterContinueBeforeItsLastByteReachesTheApp(t *testing.T) {
	log := &requestLog{}
	log.wants.Store(true)
	pm := &IngressProxyManager{logger: zap.NewNop()}
	pm.SetRequestTaker(log)
	req := readRequest(t, postWith("Expect: 100-continue\r\n", "name=ab-1"))
	body := req.Body
	pm.takeRequest(req, time.Now())
	if got := log.list(); len(got) != 1 || !got[0].coming || req.Body == body {
		t.Fatalf("taken %+v, want one still coming, its body read as it passes", got)
	}
	b := make([]byte, 1)
	var fwd []byte
	for i := 0; i < len("name=ab-1"); i++ {
		if got := log.list(); !got[0].coming {
			t.Fatalf("given after %q of the body, before its last byte was read", fwd)
		}
		n, err := req.Body.Read(b)
		if err != nil && err != io.EOF {
			t.Fatal(err)
		}
		fwd = append(fwd, b[:n]...)
	}
	got := log.list()
	if string(fwd) != "name=ab-1" || got[0].coming || got[0].dones != 1 || got[0].unread != "" || got[0].req.Body != "name=ab-1" {
		t.Fatalf("forwarded %q, taken %+v; want the body given as its last byte was read", fwd, got)
	}
	_, _ = io.ReadAll(req.Body)
	_ = req.Body.Close()
	if got := log.list(); got[0].dones != 1 {
		t.Fatalf("given %d times", got[0].dones)
	}

	req = readRequest(t, "POST /u HTTP/1.1\r\nHost: api\r\nExpect: 100-continue\r\nContent-Length: 9\r\n\r\nname")
	log = &requestLog{}
	log.wants.Store(true)
	pm.SetRequestTaker(log)
	pm.takeRequest(req, time.Now())
	_, _ = io.ReadAll(req.Body)
	if got := log.list(); len(got) != 1 || got[0].coming || !strings.Contains(got[0].unread, "did not come whole") {
		t.Fatalf("a body its client sent less of than declared: taken %+v", got)
	}
	// One that ends, with no error of its own, short of its declared length.
	req = readRequest(t, postWith("Expect: 100-continue\r\n", "ab"))
	req.Body, req.ContentLength = io.NopCloser(strings.NewReader("ab")), 5
	log = &requestLog{}
	log.wants.Store(true)
	pm.SetRequestTaker(log)
	pm.takeRequest(req, time.Now())
	_, _ = io.ReadAll(req.Body)
	if got := log.list(); len(got) != 1 || got[0].coming || !strings.Contains(got[0].unread, "did not send the whole body it declared") {
		t.Fatalf("a body that ended short: taken %+v", got)
	}
}
