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
// wire sizes when it starts the capture, lets the capture's decodes hold only
// what it has reserved for them (DecodeReserveFunc), and settles the weight
// here, so each body is inflated once, by the capture. What the decodes
// reserved is still reserved when it is called: the decoded bodies'
// buffers, at their capacity.
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

// DecodeReserveFunc is asked, as a capture decodes a compressed body, for
// room for the memory the decode is about to hold: n > 0 before it holds n
// more bytes (its output buffer as it grows it, the decoder's own state), n
// < 0 once it holds -n bytes less (a buffer it outgrew, a decoder it closed;
// the answer is then ignored). It returns false to refuse: the decode stops
// at once, holding nothing, and the capture leaves the exchange out and
// sends nothing, as when BodiesDecodedFunc refuses; the caller that refused
// counts and reports it. A decode that is not refused leaves its output's
// buffer reserved, at its capacity, until the caller settles the weight in
// BodiesDecodedFunc; one that fails leaves nothing reserved
// (pkg.DecompressReserving).
//
// With it a caller that bounds what its captures hold bounds them while they
// decode too: no decoded byte is held that it has not counted, however many
// captures decode at once.
type DecodeReserveFunc func(n int64) bool

// decodeReserveCtxKey carries a capture's DecodeReserveFunc (WithDecodeReserve).
type decodeReserveCtxKey struct{}

// WithDecodeReserve returns a context carrying fn, which Capture's decodes
// ask for room before they hold their output (DecodeReserve). The enterprise
// agent sets one per exchange; OSS callers set none, and Capture decodes as
// before.
func WithDecodeReserve(ctx context.Context, fn DecodeReserveFunc) context.Context {
	return context.WithValue(ctx, decodeReserveCtxKey{}, fn)
}

// DecodeReserve is the DecodeReserveFunc ctx carries, or nil (which
// pkg.DecompressReserving takes as no bound). A CaptureHook that decodes the
// bodies itself passes it to pkg.DecompressReserving, as Capture does.
func DecodeReserve(ctx context.Context) DecodeReserveFunc {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(decodeReserveCtxKey{}).(DecodeReserveFunc)
	return fn
}
