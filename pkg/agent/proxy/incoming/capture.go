package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
)

// Keep capture buffering aligned with the existing 5MB testcase budget.
const maxHTTPBodyCaptureBytes = 5 * 1024 * 1024

type captureBuffer struct {
	buf       bytes.Buffer
	limit     int
	total     int64
	truncated bool
}

func newCaptureBuffer(limit int) *captureBuffer {
	return &captureBuffer{limit: limit}
}

func (b *captureBuffer) Write(p []byte) (int, error) {
	b.total += int64(len(p))
	if b.limit <= 0 {
		b.truncated = true
		return len(p), nil
	}

	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}

	if len(p) > remaining {
		b.truncated = true
		_, _ = b.buf.Write(p[:remaining])
		return len(p), nil
	}

	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *captureBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func (b *captureBuffer) Total() int64 {
	return b.total
}

func (b *captureBuffer) Truncated() bool {
	return b.truncated
}

type teeReadCloser struct {
	reader io.Reader
	closer io.Closer
}

func newTeeReadCloser(body io.ReadCloser, dst io.Writer) io.ReadCloser {
	return &teeReadCloser{
		reader: io.TeeReader(body, dst),
		closer: body,
	}
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	return t.reader.Read(p)
}

func (t *teeReadCloser) Close() error {
	return t.closer.Close()
}

func cloneRequestForCapture(req *http.Request) *http.Request {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Trailer = req.Trailer.Clone()
	clone.TransferEncoding = append([]string(nil), req.TransferEncoding...)
	clone.Body = http.NoBody
	clone.GetBody = nil
	return clone
}

func cloneResponseForCapture(resp *http.Response) *http.Response {
	clone := new(http.Response)
	*clone = *resp
	clone.Header = resp.Header.Clone()
	clone.Trailer = resp.Trailer.Clone()
	clone.TransferEncoding = append([]string(nil), resp.TransferEncoding...)
	clone.Body = http.NoBody
	return clone
}

func dumpCapturedRequest(req *http.Request, body []byte) ([]byte, error) {
	reqCopy := cloneRequestForCapture(req)
	if len(body) > 0 {
		reqCopy.Body = io.NopCloser(bytes.NewReader(body))
	}
	return httputil.DumpRequest(reqCopy, true)
}

func dumpCapturedResponse(resp *http.Response, req *http.Request, body []byte) ([]byte, error) {
	respCopy := cloneResponseForCapture(resp)
	respCopy.Request = req
	if len(body) > 0 {
		respCopy.Body = io.NopCloser(bytes.NewReader(body))
	}
	return httputil.DumpResponse(respCopy, true)
}
