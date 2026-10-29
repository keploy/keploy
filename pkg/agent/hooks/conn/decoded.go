package conn

import "context"

// BodiesDecodedFunc is told, once a capture has read and decoded an
// exchange's bodies and before it builds the exchange's test case, how many
// bytes of those bodies the test case will hold. It returns false to leave the
// exchange out: the capture then builds no test case and sends nothing, as for
// one over MaxTestCaseSize, and the caller that carried it counts and reports
// what it left out.
//
// reqBytes is the request body decoded (pkg.Decompress, by its
// Content-Encoding), before a multipart or url-encoded body is taken apart:
// the test case holds no more of it than that. respBytes is the response body
// decoded, or 0 when it is over LargeBodyThreshold, as the test case then
// keeps only its size.
//
// It lets a caller that holds room for its captures weigh each one by what it
// holds without inflating a compressed body itself: it reserves the bodies'
// wire sizes when it starts the capture, and tops that up here, so each body
// is inflated once, by the capture.
type BodiesDecodedFunc func(reqBytes, respBytes int) bool

// bodiesDecodedCtxKey carries a capture's BodiesDecodedFunc (WithBodiesDecoded).
type bodiesDecodedCtxKey struct{}

// WithBodiesDecoded returns a context carrying fn, which Capture calls once the
// exchange's bodies are decoded (BodiesDecoded). The enterprise agent sets one
// per exchange; OSS callers set none, and Capture is unchanged.
func WithBodiesDecoded(ctx context.Context, fn BodiesDecodedFunc) context.Context {
	return context.WithValue(ctx, bodiesDecodedCtxKey{}, fn)
}

// BodiesDecoded calls the BodiesDecodedFunc ctx carries, and returns its
// answer; true when ctx carries none. Capture calls it once per exchange it
// gets as far as building a test case for, and so does a CaptureHook that
// reads and decodes the bodies itself instead of calling Capture.
func BodiesDecoded(ctx context.Context, reqBytes, respBytes int) bool {
	if ctx == nil {
		return true
	}
	fn, _ := ctx.Value(bodiesDecodedCtxKey{}).(BodiesDecodedFunc)
	if fn == nil {
		return true
	}
	return fn(reqBytes, respBytes)
}
