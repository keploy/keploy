package pkg

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// An answer that comes back but does not decode as its Content-Encoding says
// is the app's answer, not a dropped connection or a missing answer: the test
// fails saying so; neither the reset re-send nor APP_CONNECTION_ERROR takes it
// for a drop, and replay's no-answer mark does not take it for no answer.
// Before, the decoder's own error came back as is: io.EOF for a gzip body with
// no bytes, and io.ErrUnexpectedEOF for one cut short and for a br body with
// none or cut short, the errors of a dropped connection and of an answer the
// connection cut short. A body that decodes but inflates past the cap is not
// one: it keeps its own error.
func TestAnAnswerThatDoesNotDecodeIsNotADroppedConnection(t *testing.T) {
	var whole bytes.Buffer
	zw := gzip.NewWriter(&whole)
	if _, err := zw.Write([]byte(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	var wholeBr bytes.Buffer
	bw := brotli.NewWriter(&wholeBr)
	if _, err := bw.Write([]byte(`{"ok":true,"padding":"` + strings.Repeat("x", 64) + `"}`)); err != nil {
		t.Fatal(err)
	}
	if err := bw.Close(); err != nil {
		t.Fatal(err)
	}

	// answer replays one test request at a server that answers it with status,
	// encoding and body, through SimulateHTTPStreaming if stream and
	// SimulateHTTP if not, and returns the body SimulateHTTP decoded.
	answer := func(t *testing.T, method string, status int, encoding string, body []byte, stream bool) (string, error) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			w.Header().Set("Content-Encoding", encoding)
			w.WriteHeader(status)
			_, _ = w.Write(body)
		}))
		t.Cleanup(srv.Close)
		tc := &models.TestCase{Name: "t", Kind: models.HTTP, HTTPReq: models.HTTPReq{
			Method: models.Method(method), URL: srv.URL + "/x",
			// As recorded traffic carries it: the request asks for the
			// encoding, so net/http leaves the body for keploy to decode.
			Header: map[string]string{"Accept-Encoding": encoding},
		}}
		cfg := SimulationConfig{APITimeout: 5}
		if stream {
			resp, err := SimulateHTTPStreaming(context.Background(), tc, "set", zap.NewNop(), cfg)
			if err == nil {
				_ = resp.Reader.Close()
			}
			return "", err
		}
		resp, err := SimulateHTTP(context.Background(), tc, "set", zap.NewNop(), cfg)
		if err != nil {
			return "", err
		}
		return resp.Body, nil
	}
	// noRuleTakes fails t if a rule takes err, the error of an answer the app
	// gave, for a dropped connection or for no answer.
	noRuleTakes := func(t *testing.T, err error) {
		t.Helper()
		if IsTransportConnReset(err) {
			t.Errorf("the app's answer is taken for a dropped connection by the reset re-send: %v", err)
		}
		if IsAppConnectionError(err) {
			t.Errorf("the app's answer is labelled APP_CONNECTION_ERROR: %v", err)
		}
		if IsAppNoAnswer(err) {
			t.Errorf("the app's answer is taken for no answer by replay's no-answer mark: %v", err)
		}
	}

	for _, tt := range []struct {
		name     string
		method   string
		status   int
		encoding string
		body     []byte
		stream   bool
		want     string
	}{
		{"a 204 with no body", http.MethodGet, http.StatusNoContent, "gzip", nil, false,
			`the app's 204 answer does not decode as its Content-Encoding "gzip" says: EOF`},
		{"the answer to a HEAD", http.MethodHead, http.StatusOK, "gzip", whole.Bytes(), false,
			`the app's 200 answer does not decode as its Content-Encoding "gzip" says: EOF`},
		{"a body cut short", http.MethodGet, http.StatusOK, "gzip", whole.Bytes()[:whole.Len()-6], false,
			`the app's 200 answer does not decode as its Content-Encoding "gzip" says: unexpected EOF`},
		{"a br 204 with no body", http.MethodGet, http.StatusNoContent, "br", nil, false,
			`the app's 204 answer does not decode as its Content-Encoding "br" says: unexpected EOF`},
		{"the answer to a HEAD, br", http.MethodHead, http.StatusOK, "br", wholeBr.Bytes(), false,
			`the app's 200 answer does not decode as its Content-Encoding "br" says: unexpected EOF`},
		{"a br body cut short", http.MethodGet, http.StatusOK, "br", wholeBr.Bytes()[:wholeBr.Len()/2], false,
			`the app's 200 answer does not decode as its Content-Encoding "br" says: unexpected EOF`},
		{"a stream with no body", http.MethodGet, http.StatusOK, "gzip", nil, true,
			`the app's 200 answer does not decode as its Content-Encoding "gzip" says: EOF`},
		{"a stream that is not gzip", http.MethodGet, http.StatusOK, "gzip", []byte("data: not gzip\n\n"), true,
			`the app's 200 answer does not decode as its Content-Encoding "gzip" says: gzip: invalid header`},
		{"a whole body", http.MethodGet, http.StatusOK, "gzip", whole.Bytes(), false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, err := answer(t, tt.method, tt.status, tt.encoding, tt.body, tt.stream)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("a whole gzip body failed to decode: %v", err)
				}
				if body != `{"ok":true}` {
					t.Fatalf("decoded body %q, want %q", body, `{"ok":true}`)
				}
				return
			}
			if err == nil {
				t.Fatal("an answer that does not decode replayed as one that does")
			}
			noRuleTakes(t, err)
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("got %q; want it to say %q", err, tt.want)
			}
		})
	}

	// A body that inflates past MaxDecompressedSize decodes; it is only too
	// large. It fails with Decompress's own error, as on main, by which a
	// caller tells it from a body that does not decode (errors.Is(err,
	// ErrDecompressedTooLarge)), and no rule takes it for a drop or for no
	// answer.
	t.Run("a gzip body that inflates past the cap", func(t *testing.T) {
		var bomb bytes.Buffer
		zw, err := gzip.NewWriterLevel(&bomb, gzip.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(zw, zeroReader{}, MaxDecompressedSize+64*1024); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = answer(t, http.MethodGet, http.StatusOK, "gzip", bomb.Bytes(), false)
		if !errors.Is(err, ErrDecompressedTooLarge) {
			t.Fatalf("got %v; want the error of a body that inflates past the cap, which errors.Is finds ErrDecompressedTooLarge in", err)
		}
		if strings.Contains(err.Error(), "does not decode") {
			t.Errorf("a body that decodes, and is only too large, is said not to decode: %v", err)
		}
		noRuleTakes(t, err)
	})
}
