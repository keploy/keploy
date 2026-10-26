package http

import (
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"weak"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/bodydigest"
	"go.keploy.io/server/v3/pkg/models"
)

// encodeAnswer is a recorded answer's wire bytes: the status line, the header
// lines (a Content-Length recomputed over the body sent, unless the answer has
// no body by rule) and the body, compressed per its Content-Encoding. stub's
// response is in its Spec (withResponse). The bytes are written into one buffer
// of their size: each header line was a fmt.Sprintf and a new string holding
// every line before it.
func (h *HTTP) encodeAnswer(stub *models.Mock) ([]byte, error) {
	resp := stub.Spec.HTTPResp
	protoMajor, protoMinor := 1, 1
	method := ""
	if stub.Spec.HTTPReq != nil {
		protoMajor, protoMinor = stub.Spec.HTTPReq.ProtoMajor, stub.Spec.HTTPReq.ProtoMinor
		method = string(stub.Spec.HTTPReq.Method)
	}
	body := resp.Body
	header := pkg.ToWireHTTPHeader(resp.Header, resp.HeaderLineLengths)
	respBody := body
	if encoding, ok := header["Content-Encoding"]; ok && len(encoding) > 0 {
		compressed, err := pkg.Compress(h.Logger, encoding[0], []byte(body))
		if err != nil {
			return nil, err
		}
		respBody = string(compressed)
	}
	// The digests the recorded body matched as its server sent it, over the
	// bytes sent now (models.MetadataBodyDigestsKey): obfuscation, encryption
	// and a variable rewrite a body, and keploy's compressor is not the
	// server's. Those it did not match are sent as recorded.
	bodydigest.Apply(stub.Spec.Metadata[models.MetadataBodyDigestsKey], header, body, respBody)
	noBody := responseHasNoBody(method, resp.StatusCode)
	var lengthDigits [20]byte
	length := strconv.AppendInt(lengthDigits[:0], int64(len(respBody)), 10)
	status := http.StatusText(resp.StatusCode)

	// "HTTP/" major "." minor " " code " " text CRLF, each int at most 20 bytes.
	size := len("HTTP/. \r\n ") + 3*20 + len(status) + len("\r\n") + len(respBody)
	for key, values := range header {
		if key == "Content-Length" && !noBody {
			size += len(key) + len(": \r\n") + len(length)
			continue
		}
		for _, value := range values {
			size += len(key) + len(": \r\n") + len(value)
		}
	}
	out := make([]byte, 0, size)
	out = append(out, "HTTP/"...)
	out = strconv.AppendInt(out, int64(protoMajor), 10)
	out = append(out, '.')
	out = strconv.AppendInt(out, int64(protoMinor), 10)
	out = append(out, ' ')
	out = strconv.AppendInt(out, int64(resp.StatusCode), 10)
	out = append(out, ' ')
	out = append(out, status...)
	out = append(out, "\r\n"...)
	for key, values := range header {
		if key == "Content-Length" && !noBody {
			out = append(out, key...)
			out = append(out, ": "...)
			out = append(out, length...)
			out = append(out, "\r\n"...)
			continue
		}
		for _, value := range values {
			out = appendHeaderLine(out, key, value)
		}
	}
	out = append(out, "\r\n"...)
	out = append(out, respBody...)
	return out, nil
}

func appendHeaderLine(out []byte, key, value string) []byte {
	out = append(out, key...)
	out = append(out, ": "...)
	out = append(out, value...)
	return append(out, "\r\n"...)
}

// keepsAnswer reports whether stub's answer bytes may be kept for its later
// serves (keptAnswers): stub is a pooled recording (Mock.Pooled), its response
// is in the mock itself, and nothing serves it rewritten. A response kept apart
// from the mock (Mock.HasSpilledResponse) is read back into a copy with a new
// response on every serve (withResponse), and the variable pass
// (renderAnswers) and the correlation pass (renderCorrelations) answer with a
// copy holding a new, rewritten response on every serve: none of them is
// pooled, and each is built per serve, as are a mock with variables and one
// with correlations.
func keepsAnswer(stub *models.Mock) bool {
	return stub != nil && stub.Pooled() && stub.Spec.HTTPResp != nil && !stub.HasSpilledResponse() &&
		!hasVariables(stub) && len(stub.Spec.Correlations) == 0
}

// answerKeepLimit is how many bytes of answers keptAnswers holds at most; an
// answer served again once it is full is built on every serve, as before.
const answerKeepLimit = 64 << 20

// keptAnswers holds the wire bytes of the recorded answers served more than
// once (an answer a reusable recording gives every test, a config mock, a
// health check), so a serve after the second writes the bytes built before.
var keptAnswers = newAnswerTable(answerKeepLimit)

// answerTable keeps a recorded answer's wire bytes, by the identity of its
// recorded response (Mock.Spec.HTTPResp). Pooled mocks are never changed: a
// copy that serves a different answer has a response of its own (withResponse,
// renderAnswers, renderCorrelations), and the mock manager's copies of a
// pooled mock (a consumed mock's update, a staging's own copy) share their
// response with it. The table holds no
// response: an entry goes when its response is collected, and holds bytes only
// once a second serve showed that the answer is served again, so the answers a
// test consumes once cost a small entry, never a second copy of the answer.
type answerTable struct {
	entries sync.Map // weak.Pointer[models.HTTPResp] -> *keptAnswer
	held    atomic.Int64
	limit   int64
	// builds counts the answers encoded through the table (rows, diagnostics).
	builds atomic.Int64
}

// keptAnswer is one recorded answer's entry: what of its mock outside the
// response the bytes were built from, and the bytes, once kept.
type keptAnswer struct {
	from answerInputs
	wire atomic.Pointer[[]byte]
}

// answerInputs is what of a mock outside its response its answer bytes read.
type answerInputs struct {
	major, minor int
	method       string
	digests      string
}

func inputsOf(stub *models.Mock) answerInputs {
	in := answerInputs{major: 1, minor: 1, digests: stub.Spec.Metadata[models.MetadataBodyDigestsKey]}
	if r := stub.Spec.HTTPReq; r != nil {
		in.major, in.minor, in.method = r.ProtoMajor, r.ProtoMinor, string(r.Method)
	}
	return in
}

func newAnswerTable(limit int64) *answerTable {
	return &answerTable{limit: limit}
}

// serve returns stub's answer bytes: the ones kept for its response, else
// encode's, kept once the answer is served a second time and the table has
// room. The bytes are shared by every later serve and are never written to.
// stub must keep its answer (keepsAnswer).
func (t *answerTable) serve(stub *models.Mock, encode func(*models.Mock) ([]byte, error)) ([]byte, error) {
	resp := stub.Spec.HTTPResp
	key := weak.Make(resp)
	in := inputsOf(stub)
	v, seen := t.entries.Load(key)
	if seen {
		k := v.(*keptAnswer)
		if k.from == in {
			if w := k.wire.Load(); w != nil {
				return *w, nil
			}
		}
	}
	t.builds.Add(1)
	wire, err := encode(stub)
	if err != nil {
		return nil, err
	}
	if !seen {
		if _, loaded := t.entries.LoadOrStore(key, &keptAnswer{from: in}); !loaded {
			runtime.AddCleanup(resp, t.forget, key)
		}
		return wire, nil
	}
	k := v.(*keptAnswer)
	if k.from != in {
		// The response is served from a mock that reads it otherwise (a
		// different request line or digest note): built for this serve only.
		return wire, nil
	}
	n := int64(len(wire))
	if t.held.Add(n) > t.limit {
		t.held.Add(-n)
		return wire, nil
	}
	if !k.wire.CompareAndSwap(nil, &wire) {
		t.held.Add(-n)
	}
	return wire, nil
}

// forget drops a collected response's entry.
func (t *answerTable) forget(key weak.Pointer[models.HTTPResp]) {
	v, ok := t.entries.LoadAndDelete(key)
	if !ok {
		return
	}
	if w := v.(*keptAnswer).wire.Load(); w != nil {
		t.held.Add(-int64(len(*w)))
	}
}
