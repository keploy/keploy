package pkg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/logleak"
	"go.keploy.io/server/v3/pkg/models"
)

// answerWith listens on loopback and answers every request it reads with
// answer, as it is, then closes the connection: an app whose answer net/http
// may not parse.
func answerWith(t *testing.T, answer string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					return
				}
				_, _ = c.Write([]byte(answer))
			}(c)
		}
	}()
	return ln.Addr().String()
}

// unparsedAnswers are answers net/http does not parse, one per error of
// netHTTPAnswerParsePhrases and a header line (textproto.ProtocolError), each
// with the phrase net/http's own error begins with and the bytes of the
// answer it quotes ("" where it quotes none).
var unparsedAnswers = []struct {
	name, answer, phrase, planted string
}{
	{"status line", "Zq7STATUSLINEsecret91\r\n\r\n", "malformed HTTP response ", "Zq7STATUSLINEsecret91"},
	{"status code", "HTTP/1.1 Zq7CODEsecret92 OK\r\n\r\n", "malformed HTTP status code ", "Zq7CODEsecret92"},
	{"version", "HTTP/9.Zq7VERSIONsecret93 200 OK\r\n\r\n", "malformed HTTP version ", "Zq7VERSIONsecret93"},
	{"header line", "HTTP/1.1 200 OK\r\nZq7HEADERLINEsecret94\r\n\r\n", "malformed MIME header", "Zq7HEADERLINEsecret94"},
	{"Content-Length", "HTTP/1.1 200 OK\r\nContent-Length: Zq7LENGTHsecret95\r\n\r\n", "bad Content-Length ", "Zq7LENGTHsecret95"},
	{"empty Content-Length", "HTTP/1.1 200 OK\r\nContent-Length: \r\n\r\n", "invalid empty Content-Length ", ""},
	{"two transfer encodings", "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTransfer-Encoding: Zq7ENCODINGSsecret96\r\n\r\n", "too many transfer encodings: ", "Zq7ENCODINGSsecret96"},
	{"transfer encoding", "HTTP/1.1 200 OK\r\nTransfer-Encoding: Zq7ENCODINGsecret97\r\n\r\n", "unsupported transfer encoding: ", "Zq7ENCODINGsecret97"},
	{"two Content-Lengths", "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nContent-Length: 6Zq7LENGTHSsecret98\r\n\r\nhello", "http: message cannot contain multiple Content-Length headers; got ", "6Zq7LENGTHSsecret98"},
}

// The test case's query value and a value segment of its path, which the
// Go client's *url.Error quotes whole.
const (
	clientErrQueryValue = "Zq7QUERYsecret81"
	clientErrSegment    = "acct-4411-9932-Zq7"
)

func clientErrTarget(addr string) string {
	return "http://" + addr + "/accounts/" + clientErrSegment + "/pay?token=" + clientErrQueryValue
}

// net/http's error for an answer whose head does not parse quotes the bytes
// that did not parse, and its *url.Error the request's URL whole. Each of
// netHTTPAnswerParsePhrases is provoked from a real answer here, so a change
// to net/http's text fails this test: clientError names the request by its
// shape and the answer as not parsing, with no byte of either.
func TestClientErrorNamesAnUnparsedAnswerByNoByteOfIt(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range unparsedAnswers {
		t.Run(c.name, func(t *testing.T) {
			addr := answerWith(t, c.answer)
			req, err := http.NewRequest(http.MethodGet, clientErrTarget(addr), nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: &http.Transport{}}
			t.Cleanup(client.CloseIdleConnections)
			_, raw := client.Do(req)
			if raw == nil {
				t.Fatalf("net/http parsed the answer %q", c.answer)
			}
			if !strings.Contains(raw.Error(), c.phrase) || c.planted != "" && !strings.Contains(raw.Error(), c.planted) {
				t.Fatalf("net/http's error for the answer %q no longer says %q with its bytes: %v", c.answer, c.phrase, raw)
			}
			if !answerParseError(raw) {
				t.Fatalf("answerParseError does not know net/http's error %v", raw)
			}
			seen[c.phrase] = true
			got := clientError(raw)
			var ue *url.Error
			if !errors.As(got, &ue) || ue.Op != "Get" {
				t.Fatalf("clientError(%v) = %#v, not the client's *url.Error", raw, got)
			}
			if !strings.Contains(got.Error(), "the app's answer does not parse") || !strings.Contains(got.Error(), "/pay?token={}") {
				t.Fatalf("clientError(%v) = %q: not the call's shape and that its answer does not parse", raw, got)
			}
			planted := []string{clientErrQueryValue, clientErrSegment}
			if c.planted != "" {
				planted = append(planted, c.planted)
			}
			for _, v := range planted {
				if found := logleak.Find(t, got.Error(), []byte(v)); len(found) > 0 {
					t.Errorf("clientError's text holds %q: %v\n%s", v, found, got)
				}
			}
		})
	}
	for _, p := range netHTTPAnswerParsePhrases {
		if !seen[p] && !t.Failed() {
			t.Errorf("no answer provokes net/http's %q: add one", p)
		}
	}
}

// locationCode is a redirect's Location value: an OAuth-style code after a
// path with an unescaped '%', which url.Parse refuses.
const locationCode = "Zq7LOCATIONsecret61"

// net/http's client parses a 3xx answer's Location header before it asks
// CheckRedirect, so a client that follows no redirect (SimulateHTTP's, the
// readiness probe's) still fails on one url.Parse refuses, with an error
// that quotes the header whole. It is provoked from a real answer here, so
// a change to net/http's text fails this test: AnswerCause names it by the
// header's length alone, and clientError the call by its shape.
func TestAnswerCauseNamesAnUnparsedLocationByItsLength(t *testing.T) {
	location := "/callback/100%off?code=" + locationCode
	addr := answerWith(t, "HTTP/1.1 302 Found\r\nLocation: "+location+"\r\nContent-Length: 0\r\n\r\n")
	for _, follow := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodGet, clientErrTarget(addr), nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Transport: &http.Transport{}}
		if !follow {
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		}
		_, raw := client.Do(req)
		if raw == nil || !strings.Contains(raw.Error(), netHTTPLocationParsePhrase+fmt.Sprintf("%q", location)) {
			t.Fatalf("net/http's error for the Location %q no longer says %q with it quoted: %v", location, netHTTPLocationParsePhrase, raw)
		}
		client.CloseIdleConnections()
		var ue *url.Error
		if !errors.As(raw, &ue) {
			t.Fatalf("precondition: client.Do's error is a *url.Error, got %T", raw)
		}
		want := fmt.Sprintf("the app's Location header (%d bytes) does not parse", len(location))
		if got := AnswerCause(ue.Err); got.Error() != want {
			t.Errorf("AnswerCause(%v) = %q, want %q", ue.Err, got, want)
		}
		got := clientError(raw)
		for _, v := range []string{clientErrQueryValue, clientErrSegment, locationCode} {
			if found := logleak.Find(t, got.Error(), []byte(v)); len(found) > 0 {
				t.Errorf("clientError's text holds %q: %v\n%s", v, found, got)
			}
		}
	}
	if got := AnswerCause(errors.New(netHTTPLocationParsePhrase + "unquoted")); got.Error() != "the app's Location header does not parse" {
		t.Errorf("AnswerCause of a Location error with no quote = %q, want it named with no length", got)
	}
}

// A failure that is not an answer net/http could not parse keeps its cause,
// so errors.Is and errors.As tell it as before, and loses only the URL's
// values: a refusal (which the re-send and APP_CONNECTION_ERROR ask about)
// and a connection the app closed with no answer.
func TestClientErrorKeepsAConnectionErrorsCause(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusing := ln.Addr().String()
	_ = ln.Close()
	closing := answerWith(t, "")
	cut := answerWith(t, "HTTP/1.1 200 OK\r\nContent-Length: 10\r\n")
	for _, c := range []struct {
		name string
		addr string
		is   func(error) bool
	}{
		{"refused", refusing, isPreResponseConnRefused},
		{"closed with no answer", closing, isConnDropped},
		// A head cut short is no head that does not parse: net/http's
		// error for it quotes nothing, and is kept as it came.
		{"a head cut short", cut, func(err error) bool {
			return strings.Contains(err.Error(), "transport connection broken: unexpected EOF")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, clientErrTarget(c.addr), strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: &http.Transport{}}
			t.Cleanup(client.CloseIdleConnections)
			_, raw := client.Do(req)
			if raw == nil || !c.is(raw) {
				t.Fatalf("the request did not fail as %s: %v", c.name, raw)
			}
			got := clientError(raw)
			if !c.is(got) || IsAppConnectionError(got) != IsAppConnectionError(raw) {
				t.Fatalf("clientError(%v) = %v: it no longer tells %s", raw, got, c.name)
			}
			if strings.Contains(got.Error(), "does not parse") || !strings.Contains(got.Error(), "/pay?token={}") {
				t.Fatalf("clientError(%v) = %q: not the call's shape with its own cause", raw, got)
			}
			for _, v := range []string{clientErrQueryValue, clientErrSegment} {
				if found := logleak.Find(t, got.Error(), []byte(v)); len(found) > 0 {
					t.Errorf("clientError's text holds %q: %v\n%s", v, found, got)
				}
			}
		})
	}
}

// keploy test sends a test case's request with SimulateHTTP, which logs a
// failed send at Error and returns the error, which replay logs at Error too
// (sayNoAnswer). Neither the log nor the error holds a byte of the test
// case's URL past its shape, nor of the app's answer: for an app that is not
// up (its refusals are re-sent, each logged at Warn), for an answer that
// does not parse, and for a redirect whose Location does not parse.
func TestSimulateHTTPLogsAFailedSendByNoByteOfTheCallOrTheAnswer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusing := ln.Addr().String()
	_ = ln.Close()
	const status = "Zq7STATUSLINEsecret71"
	location := "/callback/100%off?code=" + locationCode
	for _, c := range []struct {
		name string
		addr string
		want string
	}{
		{"not up", refusing, "connection refused"},
		{"unparsed answer", answerWith(t, status+"\r\n\r\n"), "the app's answer does not parse"},
		{"unparsed Location", answerWith(t, "HTTP/1.1 302 Found\r\nLocation: "+location+"\r\nContent-Length: 0\r\n\r\n"),
			fmt.Sprintf("the app's Location header (%d bytes) does not parse", len(location))},
	} {
		t.Run(c.name, func(t *testing.T) {
			logger, out := logleak.Logger(nil)
			tc := &models.TestCase{
				Name: "pay",
				Kind: models.HTTP,
				HTTPReq: models.HTTPReq{
					Method: http.MethodGet,
					URL:    clientErrTarget(c.addr),
					Header: map[string]string{},
				},
			}
			_, err := SimulateHTTP(context.Background(), tc, "set", logger, SimulationConfig{APITimeout: 10})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("SimulateHTTP = %v, want an error saying %q", err, c.want)
			}
			text := out.String() + "\n" + err.Error()
			if !strings.Contains(out.String(), "failed to send testcase request to app") {
				t.Fatalf("the failed send was not logged:\n%s", out.String())
			}
			for _, v := range []string{clientErrQueryValue, clientErrSegment, status, locationCode} {
				if found := logleak.Find(t, text, []byte(v)); len(found) > 0 {
					t.Errorf("the log or the error holds %q: %v\n%s", v, found, text)
				}
			}
		})
	}
}

// keploy test's sender reads the app's chunked answer, whose trailer line has
// no colon: net/http reads the trailer as it reads the body, and its error
// quotes the line. The Error line ("failed reading response body") and the
// error SimulateHTTP returns name the trailer, never the line
// (models.BodyReadError).
func TestSimulateHTTPLogsNoByteOfAnUnparsedTrailer(t *testing.T) {
	const secret = "Zq7TRAILERsecret91"
	addr := answerWith(t, "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n2\r\nok\r\n0\r\nX-Sig "+secret+"\r\n\r\n")
	logger, out := logleak.Logger(nil)
	tc := &models.TestCase{
		Name:    "pay",
		Kind:    models.HTTP,
		HTTPReq: models.HTTPReq{Method: http.MethodGet, URL: clientErrTarget(addr), Header: map[string]string{}},
	}
	_, err := SimulateHTTP(context.Background(), tc, "set", logger, SimulationConfig{APITimeout: 10})
	const want = "the app's answer's body's trailer does not parse"
	if err == nil || err.Error() != want {
		t.Fatalf("SimulateHTTP = %v, want %q", err, want)
	}
	if !strings.Contains(out.String(), "failed reading response body") || !strings.Contains(out.String(), want) {
		t.Errorf("no Error line names the trailer:\n%s", out.String())
	}
	if found := logleak.Find(t, out.String(), []byte(secret)); len(found) > 0 {
		t.Errorf("the sender's log holds the app's trailer line: %v\n%s", found, out.String())
	}
}
