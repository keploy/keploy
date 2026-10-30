package http

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// parseLeakToken is the planted value: net/http's parse errors quote the
// request line or the header line it is in.
const parseLeakToken = "sk_live_9fQ2xLp7Rw"

// unparsedExchanges are exchanges net/http cannot parse, or that are not
// framed as their head says, each holding parseLeakToken: a header line
// without its colon (request, then response), a control character in the
// query, and a body sent as chunked that is not framed in chunks (request,
// then response).
var unparsedExchanges = []struct{ name, req, resp string }{
	{"request header without its colon", "GET /pay HTTP/1.1\r\nHost: api.example.com\r\nAuthorization Bearer " + parseLeakToken + "\r\n\r\n", okAnswer},
	{"control character in the query", "GET /pay?token=" + parseLeakToken + "\x01 HTTP/1.1\r\nHost: api.example.com\r\n\r\n", okAnswer},
	{"response header without its colon", "GET /me HTTP/1.1\r\nHost: api.example.com\r\n\r\n", "HTTP/1.1 200 OK\r\nSet-Cookie session=" + parseLeakToken + "\r\nContent-Length: 2\r\n\r\nok"},
	{"chunked request not framed", "POST /pay HTTP/1.1\r\nHost: api.example.com\r\nTransfer-Encoding: chunked\r\n\r\n{\"token\":\"" + parseLeakToken + "\"}\r\n0\r\n\r\n", okAnswer},
	{"chunked response not framed", "GET /me HTTP/1.1\r\nHost: api.example.com\r\n\r\n", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n{\"token\":\"" + parseLeakToken + "\"}\r\n0\r\n\r\n"},
}

const okAnswer = "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"

// The V2 recorder, the default record path, logs an exchange it cannot parse
// at Error and puts why in the stopped-on reason it logs at Warn: by what did
// not parse and its length, never net/http's error text, which quotes the
// line, nor the body's first line as a chunk size.
func TestRecordV2_AnUnparsedExchangeIsLoggedByItsLength(t *testing.T) {
	for _, c := range unparsedExchanges {
		t.Run(c.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			h := &HTTP{Logger: logger}
			sess, sendReq, closeReq, sendResp, closeResp, _ := newTestSession(t)
			sess.Logger = logger
			at := time.Unix(1_700_005_000, 0)
			sendReq([]byte(c.req), at, at)
			sendResp([]byte(c.resp), at.Add(time.Millisecond), at.Add(time.Millisecond))
			closeReq()
			closeResp()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.recordV2(ctx, sess)
			cancel()
			warnPlus := 0
			for _, e := range logs.All() {
				if e.Level >= zapcore.WarnLevel {
					warnPlus++
				}
			}
			if warnPlus == 0 {
				t.Fatalf("precondition: recordV2 (= %v) logged nothing at Warn or above for an exchange that does not parse", err)
			}
			if found := logleak.Find(t, out.String(), []byte(parseLeakToken)); len(found) > 0 {
				t.Errorf("the log holds the planted value: %v\n%s", found, out.String())
			}
			if err != nil && strings.Contains(err.Error(), parseLeakToken) {
				t.Errorf("recordV2's error holds the planted value: %v", err)
			}
		})
	}
}

// The legacy recorder (parseFinalHTTP) logs a request or a response it cannot
// parse at Error, and returns why: by what did not parse and its length.
func TestParseFinalHTTP_AnUnparsedExchangeIsLoggedByItsLength(t *testing.T) {
	for _, c := range unparsedExchanges[:3] {
		t.Run(c.name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			h := &HTTP{Logger: logger}
			mocks := make(chan *models.Mock, 1)
			err := h.parseFinalHTTP(context.Background(), &FinalHTTP{Req: []byte(c.req), Resp: []byte(c.resp)}, 80, mocks, models.OutgoingOptions{}, nil)
			if err == nil {
				t.Fatal("precondition: parseFinalHTTP parsed an exchange net/http cannot parse")
			}
			if !strings.Contains(err.Error(), "does not parse") {
				t.Errorf("parseFinalHTTP = %v, want what did not parse named by its length", err)
			}
			if found := logleak.Find(t, out.String()+err.Error(), []byte(parseLeakToken)); len(found) > 0 {
				t.Errorf("the log or the error holds the planted value: %v\n%s\n%v", found, out.String(), err)
			}
		})
	}
}

// A call passed through to its real server at replay whose answer net/http
// cannot parse ends with an error that names the destination, never the
// answer's line.
func TestPassOneCallThrough_AnUnparsedAnswerIsNamedWithoutItsText(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nSet-Cookie session=" + parseLeakToken + "\r\n\r\n"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, proxySide := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = proxySide.Close() })
	_, err = passOneCallThrough(ctx, zap.NewNop(), proxySide, &models.ConditionalDstCfg{Addr: ln.Addr().String()}, []byte("GET /a HTTP/1.1\r\nHost: x\r\n\r\n"), "GET")
	if err == nil {
		t.Fatal("precondition: an answer net/http cannot parse was passed through")
	}
	if strings.Contains(err.Error(), parseLeakToken) || !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("passOneCallThrough = %v, want the answer named without its text", err)
	}
}

// trailerLeakToken is planted in a trailer line without its colon, after a
// chunked body framed in chunks: net/http reads a trailer as it reads the
// body, and its error (textproto's) quotes the line whole.
const trailerLeakToken = "Zq7TRAILERsecret92"

// trailerExchanges are a chunked request, then a chunked response, each
// framed in chunks, whose trailer line has no colon.
var trailerExchanges = []struct{ name, req, resp, what string }{
	{"request trailer without its colon", "POST /pay HTTP/1.1\r\nHost: api.example.com\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\nX-Sig " + trailerLeakToken + "\r\n\r\n", okAnswer, "the HTTP request's body's trailer does not parse"},
	{"response trailer without its colon", "GET /me HTTP/1.1\r\nHost: api.example.com\r\n\r\n", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\nX-Sig " + trailerLeakToken + "\r\n\r\n", "the HTTP response's body's trailer does not parse"},
}

// The V2 recorder reads a body whose trailer does not parse: the exchange is
// not recorded, and the Warn and Error lines that say so name the trailer,
// never net/http's error text, which quotes the line (models.BodyReadError).
func TestRecordV2_AnUnparsedTrailerIsLoggedByNoByteOfIt(t *testing.T) {
	for _, c := range trailerExchanges {
		t.Run(c.name, func(t *testing.T) {
			obs, logs := observer.New(zapcore.DebugLevel)
			logger, out := logleak.Logger(obs)
			h := &HTTP{Logger: logger}
			sess, sendReq, closeReq, sendResp, closeResp, _ := newTestSession(t)
			sess.Logger = logger
			at := time.Unix(1_700_005_000, 0)
			sendReq([]byte(c.req), at, at)
			sendResp([]byte(c.resp), at.Add(time.Millisecond), at.Add(time.Millisecond))
			closeReq()
			closeResp()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = h.recordV2(ctx, sess)
			cancel()
			named := false
			for _, e := range logs.All() {
				if e.Level >= zapcore.WarnLevel && strings.Contains(fmt.Sprint(e.ContextMap()), c.what) {
					named = true
				}
			}
			if !named {
				t.Errorf("no line at Warn or above names %q:\n%s", c.what, out.String())
			}
			if found := logleak.Find(t, out.String(), []byte(trailerLeakToken)); len(found) > 0 {
				t.Errorf("the V2 recorder's log holds the trailer line: %v\n%s", found, out.String())
			}
		})
	}
}

// The legacy recorder (parseFinalHTTP) logs a body whose trailer does not
// parse at Error, and returns why, by the trailer, never its line.
func TestParseFinalHTTP_AnUnparsedTrailerIsLoggedByNoByteOfIt(t *testing.T) {
	for _, c := range trailerExchanges {
		t.Run(c.name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			h := &HTTP{Logger: logger}
			mocks := make(chan *models.Mock, 1)
			err := h.parseFinalHTTP(context.Background(), &FinalHTTP{Req: []byte(c.req), Resp: []byte(c.resp)}, 80, mocks, models.OutgoingOptions{}, nil)
			if err == nil || err.Error() != c.what {
				t.Fatalf("parseFinalHTTP = %v, want %q", err, c.what)
			}
			if !strings.Contains(out.String(), c.what) {
				t.Errorf("the Error line does not name the trailer:\n%s", out.String())
			}
			if found := logleak.Find(t, out.String(), []byte(trailerLeakToken)); len(found) > 0 {
				t.Errorf("the legacy recorder's log holds the trailer line: %v\n%s", found, out.String())
			}
		})
	}
}

// A call passed through to its real server whose chunked answer's trailer
// does not parse ends with an error that names the trailer, never its line:
// resp.Write reads the trailer as it writes the answer to the app.
func TestPassOneCallThrough_AnUnparsedTrailerIsNamedWithoutItsText(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
		buf := make([]byte, 4096)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte(trailerExchanges[1].resp))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, proxySide := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = proxySide.Close() })
	go func() { _, _ = io.Copy(io.Discard, client) }()
	_, err = passOneCallThrough(ctx, zap.NewNop(), proxySide, &models.ConditionalDstCfg{Addr: ln.Addr().String()}, []byte("GET /a HTTP/1.1\r\nHost: x\r\n\r\n"), "GET")
	if err == nil {
		t.Fatal("precondition: an answer whose trailer does not parse was passed through")
	}
	if strings.Contains(err.Error(), trailerLeakToken) || !strings.Contains(err.Error(), "the server's answer's trailer does not parse") {
		t.Errorf("passOneCallThrough = %v, want the trailer named without its text", err)
	}
}
