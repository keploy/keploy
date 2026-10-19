package conn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is in a test case's query, and in its request body.
const canary = "kp-canary-capture-1f5a"

// failingBody reads its bytes, and then fails.
type failingBody struct{ io.Reader }

func (b failingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("the client went away")
	}
	return n, err
}

func (failingBody) Close() error { return nil }

// A test case capture drops or trims is logged by its method and path, not
// its query's values or its body, unless KEPLOY_LOG_CONTENT is set.
func TestCapture_ADroppedOrTrimmedTestCaseIsLoggedWithoutItsContent(t *testing.T) {
	const overLimit = "HTTP test case data exceeds 5MB limit, skipping capture"
	bytesOf := func(b []byte) func() io.Reader { return func() io.Reader { return bytes.NewReader(b) } }
	unread := []byte(`{"note":"` + canary + `"}`)
	for name, tc := range map[string]struct {
		reqBody      func() io.Reader
		reqEncoding  string
		respBody     []byte
		respEncoding string
		want         string
		body         []byte // the bytes the line names as "body"
	}{
		"a request body that fails to read": {reqBody: func() io.Reader { return failingBody{bytes.NewReader(unread)} },
			respBody: []byte("ok"), want: "failed to read the http request body", body: unread},
		"a request body over the limit decompressed": {reqBody: bytesOf(gzipBomb(t)), reqEncoding: "gzip",
			respBody: []byte("ok"), want: overLimit},
		"a response body over the limit decompressed": {reqBody: bytesOf([]byte("hi")),
			respBody: gzipBomb(t), respEncoding: "gzip", want: overLimit},
		"bodies over the limit": {reqBody: bytesOf(make([]byte, MaxTestCaseSize)),
			respBody: []byte("ok!"), want: overLimit},
		"a response body over a megabyte": {reqBody: bytesOf([]byte("hi")),
			respBody: make([]byte, LargeBodyThreshold+1), want: "response body exceeds 1MB during recording, storing only body size"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				req, err := http.NewRequest(http.MethodPost, "http://localhost:8080/upload?ref="+canary, tc.reqBody())
				if err != nil {
					t.Fatal(err)
				}
				if tc.reqEncoding != "" {
					req.Header.Set("Content-Encoding", tc.reqEncoding)
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(tc.respBody))}
				if tc.respEncoding != "" {
					resp.Header.Set("Content-Encoding", tc.respEncoding)
				}
				logger, logs := logtest.Observe()
				Capture(context.Background(), logger, make(chan *models.TestCase, 1), req, resp, time.Now(), time.Now(), models.IncomingOptions{}, true, false, 8080)
				logtest.Check(t, logs, canary, on, tc.want)
				// Named as a logcontent.Bytes line names the same bytes, not
				// by the fingerprint of their base64.
				if tc.body != nil && !on {
					got := logs.FilterMessage(tc.want).All()[0].ContextMap()["body"]
					if want := map[string]interface{}{"len": len(tc.body), "fp": logcontent.Fingerprint(tc.body)}; !reflect.DeepEqual(got, want) {
						t.Fatalf("the body is named %v, want %v", got, want)
					}
				}
			})
		})
	}
}
