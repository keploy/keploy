package conn

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// errorAfter reads its data, then fails: a client that hung up mid-body.
type errorAfter struct{ r io.Reader }

func (e *errorAfter) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, errors.New("unexpected EOF")
	}
	return n, err
}

// The capture of an incoming call logs facts about the call (its method,
// its host, its path's shape, its query's names, its sizes), never its
// body or its URL as sent: a body holds what the application was sent (a
// login's password), and a path or a query a token. It logged a body it
// could not read whole, base64 encoded, and the whole URL of a call whose
// answer it did not keep.
func TestCaptureLogsNoneOfTheCall(t *testing.T) {
	const s = logleak.Canary
	target := "/v1/reset/" + s + "?access_token=" + s
	for _, tc := range []struct {
		name string
		body io.Reader
		resp string
		line string
	}{
		{"a body the client cut short", &errorAfter{strings.NewReader(`{"password":"` + s + `"}`)}, "ok", "failed to read the http request body"},
		{"an answer over 1MB", strings.NewReader(`{"password":"` + s + `"}`), strings.Repeat("a", LargeBodyThreshold) + s, "response body exceeds 1MB"},
		{"a call over the test case limit", strings.NewReader(`{"password":"` + s + `"}`), strings.Repeat("b", MaxTestCaseSize) + s, "exceeds 5MB limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger, logs := logleak.Observe()
			req := httptest.NewRequest(http.MethodPost, target, tc.body)
			req.Header.Set("Authorization", "Bearer "+s)
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.resp))}
			ch := make(chan *models.TestCase, 1)
			at := time.Unix(1000, 0)
			Capture(context.Background(), logger, ch, req, resp, at, at.Add(time.Millisecond), models.IncomingOptions{}, false, false, 8080)
			if logs.FilterMessageSnippet(tc.line).Len() == 0 {
				t.Fatalf("precondition: the capture did not log the case's line (%q); the test drove nothing", tc.line)
			}
			logleak.Check(t, logs, s, "the capture")
		})
	}
}

// A gRPC call over the test case limit is said by its size, and its method by
// the path's shape, as an HTTP call over the limit is: the path is the app's,
// and the line is at ERROR.
func TestCaptureGRPCOverTheLimitNamesTheMethodByItsShape(t *testing.T) {
	const s = logleak.Canary
	logger, logs := logleak.Observe()
	stream := &pkg.HTTP2Stream{
		GRPCReq: &models.GrpcReq{
			Headers: models.GrpcHeaders{PseudoHeaders: map[string]string{":path": "/" + s + ".Orders/Get", ":authority": "app:9090", ":method": "POST", ":scheme": "http"}, OrdinaryHeaders: map[string]string{}},
			Body:    models.GrpcLengthPrefixedMessage{DecodedData: strings.Repeat("c", MaxTestCaseSize) + s},
		},
		GRPCResp: &models.GrpcResp{},
	}
	keys := models.NewLogKeySession()
	ctx := models.WithLogKeySession(context.Background(), keys)
	CaptureGRPC(ctx, logger, make(chan *models.TestCase, 1), stream, 8080, false, false)
	said := logs.FilterMessageSnippet("gRPC test case data exceeds 5MB limit").All()
	if len(said) != 1 || said[0].ContextMap()["path"] != "/{}/{}" {
		t.Fatalf("want one over-the-limit line naming the method by its shape, /{}/{}, got %+v", logs.All())
	}
	wantTag := models.URLPathTag(ctx, http.MethodPost, &url.URL{Path: "/" + s + ".Orders/Get"})
	if got := said[0].ContextMap()["path_tag"]; got != wantTag {
		t.Errorf("path_tag = %q, want %q", got, wantTag)
	}
	logleak.Check(t, logs, s, "the gRPC capture")
}

// Capturing a multipart upload logs only the file count, never temp or upload
// file paths; errors removing or inlining form files also withhold the file's
// name from both fields and os errors.
func TestCaptureFormDataLogsNoFilePaths(t *testing.T) {
	const s = logleak.Canary
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("document", "passport-"+s+".pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte("binary-contents")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	logger, logs := logleak.Observe()
	req := httptest.NewRequest(http.MethodPost, "/upload", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("ok"))}
	ch := make(chan *models.TestCase, 1)
	at := time.Unix(1000, 0)
	Capture(context.Background(), logger, ch, req, resp, at, at.Add(time.Millisecond), models.IncomingOptions{}, false, false, 8080)
	tc := <-ch
	t.Cleanup(func() { RemoveFormFiles(logger, tc.HTTPReq.Form) })

	said := logs.FilterMessage("Detected binary file in request form data").All()
	if len(said) != 1 {
		t.Fatalf("precondition: want 1 binary form debug line, got %d (%+v)", len(said), logs.All())
	}
	fields := said[0].ContextMap()
	if _, hasPaths := fields["paths"]; hasPaths {
		t.Errorf("binary form debug line leaked raw paths field: %+v", fields)
	}
	if got := fields["files"]; got != int64(1) {
		t.Errorf("binary form debug line files = %v, want 1", got)
	}

	// InlineFormFiles on a missing upload path whose basename holds the canary
	// must not leak the canary in either the path field or the os error.
	badLogger, badLogs := logleak.Observe()
	missingPath := filepath.Join(t.TempDir(), "passport-"+s+".pdf")
	InlineFormFiles(badLogger, &models.TestCase{
		HasBinaryFile: true,
		HTTPReq: models.HTTPReq{Form: []models.FormData{
			{Key: "document", FileNames: []string{"passport-" + s + ".pdf"}, Paths: []string{missingPath}},
		}},
	})
	if badLogs.FilterMessageSnippet("Error reading multipart temp file").Len() == 0 {
		t.Fatal("precondition: InlineFormFiles did not log missing file error")
	}
	logleak.Check(t, badLogs, s, "InlineFormFiles missing file error")

	// RemoveFormFiles on a non-empty directory whose basename holds the canary
	// must not leak the canary in either the path field or the os error.
	rmLogger, rmLogs := logleak.Observe()
	nonEmptyDir := filepath.Join(t.TempDir(), "passport-"+s+".dir")
	if err := os.Mkdir(nonEmptyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyDir, "child"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RemoveFormFiles(rmLogger, []models.FormData{{Key: "document", Paths: []string{nonEmptyDir}}})
	if rmLogs.FilterMessageSnippet("failed to remove the file a form's file part was written to").Len() == 0 {
		t.Fatal("precondition: RemoveFormFiles did not log remove error")
	}
	logleak.Check(t, rmLogs, s, "RemoveFormFiles error")
}
