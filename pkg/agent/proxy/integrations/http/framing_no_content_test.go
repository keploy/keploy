package http

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The chunked and Content-Length framing reads the app's bytes, and what it
// says about them reaches the agent's log through the parser's logger (at Warn
// and Error), the error the parser returns (which the proxy logs again), and a
// recording's stopped-on reason (recordV2). None of it may carry any
// framingWindow-byte run of the app's bytes. A mis-framed chunk (a sized
// chunk, then the next chunk's data with no size line) makes that data the
// chunk-size line, a chunk size too large is the app's hex digits, and a
// client can send anything as a Content-Length. The errors name such a value
// by its length instead (parseChunkSize, contentLengthValue).

const (
	framingSecret = "s3cr3t-t0k3n-8823"
	framingEmail  = "jane.doe@example.org"
	// framingHexSecret is a secret that is all hex digits: as a chunk-size
	// line it parses, as a size larger than maxChunkSize, and its decimal
	// spells it back.
	framingHexSecret = "deadbeefcafe8823"
)

// framingForbidden is every spelling of the app's bytes a line or an error
// must not hold: the secrets, and the hex secret's decimal.
func framingForbidden(t *testing.T) []string {
	t.Helper()
	n, err := strconv.ParseUint(framingHexSecret, 16, 64)
	if err != nil {
		t.Fatalf("framingHexSecret: %v", err)
	}
	return []string{framingSecret, framingEmail, framingHexSecret, strconv.FormatUint(n, 10)}
}

// framingWindow is the shortest run of the app's bytes that counts as
// carrying them, as in enterprise's logkeytest.Carries: a cut-down copy (a
// %.16q preview of the line, say) still holds a run this long.
const framingWindow = 8

// framingForms is f as it is and %q-escaped (without the quotes): the two
// spellings this code had of the app's bytes. For an f of printable ASCII
// with no quote or backslash, the two are the same.
func framingForms(f string) []string {
	q := strconv.Quote(f)
	return []string{f, q[1 : len(q)-1]}
}

// framingNoContent fails the test when what holds any framingWindow-byte run
// of any framingForbidden spelling, in either of its framingForms, and names
// the first run it finds per spelling. Other encodings of the app's bytes
// (hex, base64, upper- or lower-cased) are a stated limit: nothing here
// encodes them so, and the row does not look for them.
func framingNoContent(t *testing.T, where, what string) {
	t.Helper()
	for _, f := range framingForbidden(t) {
		if run, ok := framingCarries(what, f); ok {
			t.Errorf("%s carries the app's bytes %q (a run of %q): %s", where, run, f, what)
		}
	}
}

// framingCarries reports the first framingWindow-byte run of f, in either of
// its framingForms, that what holds. A form shorter than the window is looked
// for whole.
func framingCarries(what, f string) (string, bool) {
	for _, form := range framingForms(f) {
		n := min(framingWindow, len(form))
		for i := 0; i+n <= len(form); i++ {
			if run := form[i : i+n]; strings.Contains(what, run) {
				return run, true
			}
		}
	}
	return "", false
}

// framingLines renders every logged entry, at every level, with its fields.
func framingLines(logs *observer.ObservedLogs) []string {
	var lines []string
	for _, e := range logs.All() {
		lines = append(lines, fmt.Sprintf("%s %s %v", e.Level, e.Message, e.ContextMap()))
	}
	return lines
}

// framingLevelSeen reports whether an entry at level or above was logged.
func framingLevelSeen(logs *observer.ObservedLogs, level zapcore.Level) bool {
	for _, e := range logs.All() {
		if e.Level >= level {
			return true
		}
	}
	return false
}

// framingHead and framingBody share no framingWindow-byte run with
// framingForbidden, so a line that names the host is not a finding.
const framingHead = "POST /v1/queue HTTP/1.1\r\n" +
	"Host: queue.svc.internal\r\n" +
	"Content-Type: application/json\r\n"

const framingBody = `{"QueueUrl":"https://queue.svc.internal/1/orders","MessageBody":"hi"}`

func framingChunked(chunks string) string {
	return framingHead + "Transfer-Encoding: chunked\r\n\r\n" + chunks + "\r\n0\r\n\r\n"
}

// framingChunkedCases are chunked messages whose body does not frame, each
// with the app's bytes where the framing reads a chunk-size line.
var framingChunkedCases = []struct{ name, body string }{
	{"a second chunk sent with no size line",
		"2\r\n{}\r\n" + `{"MessageBody":"session=` + framingSecret + `;user=` + framingEmail + `"}`},
	{"a chunk-size line that is not hex", framingSecret + framingEmail + "\r\n" + framingBody},
	{"a chunk size too large", framingHexSecret + "\r\n" + framingBody},
}

func framingBadContentLength(start string) string {
	return start + "Content-Length: " + framingSecret + framingEmail + "\r\n\r\n" + framingBody
}

// One logger, wired as a parser wires it (New(logger) is the HTTP parser's
// logger, and every line below is logged through it), sees the request
// framing fail, in replay (no destination) and in record (relayed).
func TestRequestFramingLogsNoContent(t *testing.T) {
	type tc struct {
		name, req string
		want      error
	}
	var cases []tc
	for _, c := range framingChunkedCases {
		cases = append(cases, tc{c.name, framingChunked(c.body), errMalformedChunkedBody})
	}
	cases = append(cases, tc{"a Content-Length that is not a number", framingBadContentLength(framingHead), errBadContentLength})

	for _, c := range cases {
		for _, record := range []bool{false, true} {
			mode := "replay"
			if record {
				mode = "record"
			}
			t.Run(c.name+"/"+mode, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					core, logs := observer.New(zapcore.DebugLevel)
					h := New(zap.New(core)).(*HTTP)
					client := newScriptedConn()
					t.Cleanup(func() { _ = client.Close() })
					var dest *scriptedConn
					buf := []byte(c.req)
					var err error
					if record {
						dest = newScriptedConn()
						t.Cleanup(func() { _ = dest.Close() })
						err = h.HandleChunkedRequests(context.Background(), &buf, client, dest)
					} else {
						err = h.HandleChunkedRequests(context.Background(), &buf, client, nil)
					}
					if err != nil {
						framingNoContent(t, "the returned error", err.Error())
					}
					for _, line := range framingLines(logs) {
						framingNoContent(t, "a log line", line)
					}
					if !framingLevelSeen(logs, zapcore.WarnLevel) {
						t.Errorf("no Warn or Error line: the framing failure went unsaid")
					}
					// Record relays a chunked body that does not frame (and
					// says so at Warn); everything else fails, and names the
					// value it could not read by its length.
					relayed := record && errors.Is(c.want, errMalformedChunkedBody)
					switch {
					case relayed && err != nil:
						t.Errorf("record relays a body that does not frame, but HandleChunkedRequests returned %v", err)
					case !relayed && !errors.Is(err, c.want):
						t.Errorf("HandleChunkedRequests returned %v, want %v", err, c.want)
					case !relayed && !strings.Contains(err.Error(), "-byte") && !strings.Contains(err.Error(), "-digit"):
						t.Errorf("the returned error does not name the value by its length: %v", err)
					}
				})
			})
		}
	}
}

// The response side: handleChunkedResponses relays a server's response in
// record, through the same logger.
func TestResponseFramingLogsNoContent(t *testing.T) {
	const status = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"
	type tc struct {
		name, resp string
		want       error // nil: relayed, the failure said at Warn
	}
	var cases []tc
	for _, c := range framingChunkedCases {
		cases = append(cases, tc{c.name, status + "Transfer-Encoding: chunked\r\n\r\n" + c.body + "\r\n0\r\n\r\n", nil})
	}
	cases = append(cases, tc{"a Content-Length that is not a number", framingBadContentLength(status), errBadContentLength})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				core, logs := observer.New(zapcore.DebugLevel)
				h := New(zap.New(core)).(*HTTP)
				client, dest := newScriptedConn(), newScriptedConn()
				t.Cleanup(func() { _ = client.Close(); _ = dest.Close() })
				finalResp := []byte(c.resp)
				err := h.handleChunkedResponses(context.Background(), &finalResp, client, dest, []byte(c.resp), "GET")
				if err != nil {
					framingNoContent(t, "the returned error", err.Error())
				}
				for _, line := range framingLines(logs) {
					framingNoContent(t, "a log line", line)
				}
				if !framingLevelSeen(logs, zapcore.WarnLevel) {
					t.Errorf("no Warn or Error line: the framing failure went unsaid (err %v)", err)
				}
				// A chunked body that does not frame is relayed; a
				// Content-Length that does not parse fails, and the error
				// names the value by its length.
				switch {
				case c.want == nil && err != nil:
					t.Errorf("record relays a body that does not frame, but handleChunkedResponses returned %v", err)
				case c.want != nil && !errors.Is(err, c.want):
					t.Errorf("handleChunkedResponses returned %v, want %v", err, c.want)
				case c.want != nil && !strings.Contains(err.Error(), "-byte"):
					t.Errorf("the returned error does not name the value by its length: %v", err)
				}
			})
		})
	}
}

// The V2 readers return the framing error to recordV2, which logs it at Error
// and keeps it as the recording's stopped-on reason. readRequestV2 and
// readResponseV2 log nothing themselves, so this row checks only the error
// they return (and runs them on a Nop logger).
func TestV2FramingErrorsCarryNoContent(t *testing.T) {
	for _, c := range framingChunkedCases {
		t.Run("request/"+c.name, func(t *testing.T) {
			h := New(zap.NewNop()).(*HTTP)
			stream, _, _ := makeStream(t, fakeconn.FromClient, 1)
			req := []byte(framingChunked(c.body))
			err := h.readRequestV2(context.Background(), stream, &req)
			framingV2Err(t, "readRequestV2", err, errMalformedChunkedBody)
		})
		t.Run("response/"+c.name, func(t *testing.T) {
			h := New(zap.NewNop()).(*HTTP)
			stream, _, _ := makeStream(t, fakeconn.FromDest, 1)
			resp := []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + c.body + "\r\n0\r\n\r\n")
			_, err := h.readResponseV2(context.Background(), stream, &resp, "GET")
			framingV2Err(t, "readResponseV2", err, errMalformedChunkedBody)
		})
	}
	t.Run("request/a Content-Length that is not a number", func(t *testing.T) {
		h := New(zap.NewNop()).(*HTTP)
		stream, _, _ := makeStream(t, fakeconn.FromClient, 1)
		req := []byte(framingBadContentLength(framingHead))
		err := h.readRequestV2(context.Background(), stream, &req)
		framingV2Err(t, "readRequestV2", err, errBadContentLength)
	})
	t.Run("response/a Content-Length that is not a number", func(t *testing.T) {
		h := New(zap.NewNop()).(*HTTP)
		stream, _, _ := makeStream(t, fakeconn.FromDest, 1)
		resp := []byte(framingBadContentLength("HTTP/1.1 200 OK\r\n"))
		_, err := h.readResponseV2(context.Background(), stream, &resp, "GET")
		framingV2Err(t, "readResponseV2", err, errBadContentLength)
	})
}

// framingV2Err checks the error a V2 reader returned: it carries none of the
// app's bytes, is want, and names the value it could not read by its length.
func framingV2Err(t *testing.T, reader string, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s returned nil, want %v", reader, want)
	}
	framingNoContent(t, reader+"'s error", err.Error())
	if !errors.Is(err, want) {
		t.Errorf("%s returned %v, want %v", reader, err, want)
	}
	if !strings.Contains(err.Error(), "-byte") && !strings.Contains(err.Error(), "-digit") {
		t.Errorf("%s's error does not name the value by its length: %v", reader, err)
	}
}
