package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/bodydigest"
	"go.keploy.io/server/v3/pkg/models"
)

// An answer held for its recorded place (models.RegisterAnswerRelease). The
// call was matched and consumed when it was made; only the write of each part
// of its answer waits until the replay's run reaches or passes the moment that
// part reached the application on record. Nothing is ever written later than
// the run's place allows, and every hold ends at the set's end.
//
// A hold is never one a test's own end waits for, as that test could then
// never end. A part's mark says its test did not wait for it: a mark is
// stamped before the relay's write, so one after the test's end is of a part
// that test ended without. A recording with no marks says no such thing
// (models.HTTPAnswerMarks) and is never held, nor is a recording the run has
// not reached: its call came ahead of its place, so it has no place to keep.

// heldAnswer is a matched answer whose write waits for the run's place.
type heldAnswer struct {
	// head is written first, once headAt is due: the status line and header
	// lines, and, for an answer written whole (!chunked), its body too.
	head   []byte
	headAt time.Time
	// pieces are the body's parts, each due at its own recorded moment,
	// written as one chunk each (chunked) after the head.
	pieces  []heldPiece
	chunked bool

	release models.AnswerRelease
	placer  integrations.RunPlacer
	staging uint64
}

type heldPiece struct {
	at   time.Time
	data []byte
}

// heldAnswerOf is stub's answer to call, the application's request it
// matched, as it must be held at the run's place now, or nil when it is
// written at once, as before: no release is registered for HTTP, the store
// tells no run place, the serve is not the first of a per-test recording (a
// session or connection recording, an answer served again: repeat), its
// recording kept no marks or is one the run has not reached, or its last byte
// is due already, and with it every part of it (a release is monotone in
// time). All that is read of the mock alone, so an answer not held is built
// once, by its write. stub is the serve's: the match loaded its response
// (claim), which an answer kept apart by the agent's disk store has in Spec
// from then on.
func (h *HTTP) heldAnswerOf(stub *models.Mock, call *http.Request, db integrations.MockMemDb, repeat bool) (*heldAnswer, error) {
	release, ok := models.AnswerReleaseOf(models.HTTP)
	if !ok || repeat || stub.Spec.HTTPResp == nil || stub.Spec.HTTPResp.Marks == nil {
		return nil, nil
	}
	if stub.TestModeInfo.Lifetime != models.LifetimePerTest {
		return nil, nil
	}
	placer, ok := db.(integrations.RunPlacer)
	if !ok {
		return nil, nil
	}
	place := placer.RunPlace()
	if release(place, stub.Spec.ResTimestampMock) || !release(place, stub.Spec.ReqTimestampMock) {
		return nil, nil
	}
	a, err := h.planHeld(stub, call)
	if err != nil {
		return nil, err
	}
	a.release, a.placer, a.staging = release, placer, place.Staging
	return a, nil
}

// planHeld splits stub's answer, its response in its Spec with its marks,
// into what is written when:
//   - an answer that went out in one write, or that cannot go in parts, whole
//     at its last mark;
//   - any other with its head at Marks.Head and each write's part at its own
//     time, sent chunked.
//
// It cannot go in parts when its marks do not fit the body served (its length
// differs: a body rewritten since), when it has no body by rule, and when the
// call is older than HTTP/1.1, which cannot take a chunked answer. The call
// is the application's request now, not the recorded one: it is the
// application's client that reads the answer.
func (h *HTTP) planHeld(stub *models.Mock, call *http.Request) (*heldAnswer, error) {
	resp := stub.Spec.HTTPResp
	marks := resp.Marks
	method := ""
	if req := stub.Spec.HTTPReq; req != nil {
		method = string(req.Method)
	}
	http11 := call != nil && call.ProtoAtLeast(1, 1)
	if !http11 || wentOutWhole(marks) || !marksFit(marks, len(resp.Body)) || responseHasNoBody(method, resp.StatusCode) {
		whole, err := h.buildMockResponseBytes(stub)
		if err != nil {
			return nil, err
		}
		return &heldAnswer{head: whole, headAt: marks.End()}, nil
	}
	return h.chunkedAnswer(stub, marks)
}

// wentOutWhole reports whether marks are of an answer that went out in one
// write, its head with its body.
func wentOutWhole(marks *models.HTTPAnswerMarks) bool {
	return len(marks.Writes) == 1 && marks.Writes[0].TS.Equal(marks.Head)
}

// marksFit reports whether marks' writes end, in order, inside a body of n
// bytes, the last at its end.
func marksFit(marks *models.HTTPAnswerMarks, n int) bool {
	if len(marks.Writes) == 0 {
		return false
	}
	prev := 0
	for _, w := range marks.Writes {
		if w.Offset < prev || w.Offset > n {
			return false
		}
		prev = w.Offset
	}
	return prev == n
}

// chunkedAnswer is stub's answer with its head at marks.Head and each write's
// part at its time, each part one chunk: the header lines as encodeAnswer
// writes them, with Transfer-Encoding: chunked for the recorded framing, and
// the body compressed per its Content-Encoding part by part, each part
// flushed, so the application decodes each as it arrives.
func (h *HTTP) chunkedAnswer(stub *models.Mock, marks *models.HTTPAnswerMarks) (*heldAnswer, error) {
	resp := stub.Spec.HTTPResp
	body := resp.Body
	header := pkg.ToWireHTTPHeader(resp.Header, resp.HeaderLineLengths)
	encoding := ""
	if e, ok := header["Content-Encoding"]; ok && len(e) > 0 {
		encoding = strings.ToLower(strings.TrimSpace(e[0]))
	}
	parts, wire, err := encodeParts(encoding, body, marks.Writes)
	if err != nil {
		return nil, err
	}
	bodydigest.Apply(stub.Spec.Metadata[models.MetadataBodyDigestsKey], header, body, string(wire))

	// HTTP/1.1, whatever the recorded call was: a chunked answer is
	// HTTP/1.1's framing (a client reads an older answer's
	// Transfer-Encoding as none), and only an HTTP/1.1 call is answered
	// chunked (planHeld).
	out := make([]byte, 0, 256)
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(resp.StatusCode), 10)
	out = append(out, ' ')
	out = append(out, reasonPhrase(resp)...)
	out = append(out, "\r\n"...)
	names := make([]string, 0, len(header)+1)
	for key := range header {
		if key == "Content-Length" || key == "Transfer-Encoding" {
			continue
		}
		names = append(names, key)
	}
	names = append(names, "Transfer-Encoding")
	slices.Sort(names)
	for _, key := range names {
		if key == "Transfer-Encoding" {
			out = appendHeaderLine(out, key, "chunked")
			continue
		}
		for _, value := range header[key] {
			out = appendHeaderLine(out, key, value)
		}
	}
	out = append(out, "\r\n"...)

	a := &heldAnswer{head: out, headAt: marks.Head, chunked: true}
	for i, w := range marks.Writes {
		a.pieces = append(a.pieces, heldPiece{at: w.TS, data: parts[i]})
	}
	return a, nil
}

// encodeParts is body cut at each write's offset, compressed per encoding as
// one stream flushed at each cut: parts[i] is write i's bytes on the wire,
// and wire all of them. An encoding keploy does not compress (pkg.Compress)
// is sent as it is.
func encodeParts(encoding, body string, writes []models.HTTPAnswerWrite) (parts [][]byte, wire []byte, err error) {
	var buf bytes.Buffer
	var w interface {
		Write([]byte) (int, error)
		Flush() error
		Close() error
	}
	switch encoding {
	case "gzip":
		w = gzip.NewWriter(&buf)
	case "br":
		w = brotli.NewWriter(&buf)
	}
	prev, cut := 0, 0
	for i, wr := range writes {
		piece := body[prev:wr.Offset]
		prev = wr.Offset
		if w == nil {
			buf.WriteString(piece)
		} else {
			if _, err := w.Write([]byte(piece)); err != nil {
				return nil, nil, err
			}
			if i == len(writes)-1 {
				err = w.Close()
			} else {
				err = w.Flush()
			}
			if err != nil {
				return nil, nil, err
			}
		}
		parts = append(parts, buf.Bytes()[cut:buf.Len():buf.Len()])
		cut = buf.Len()
	}
	return parts, buf.Bytes(), nil
}

// writeHeld writes a to conn as the run's place makes each part due: the
// head once headAt is, then each piece, one chunk each, then the last chunk.
// While it waits, the connection is marked idle (integrations.MarkIdle): a
// test's end and begin do not wait for a hold, as the application has its
// call's answer coming. The mark lasts until the place's next change, which
// releases it before the change's call returns
// (integrations.RunPlacer.IdleUntilRunPlaceChanges): a read that settles the
// application's calls right after it (the set's end read) waits for the
// part it made due and for what the application does with it. Every part is
// due once the set ends, the set's staging changes, or the store closes. A
// write that fails (the application closed its connection) drops the rest
// and returns the error.
func (h *HTTP) writeHeld(ctx context.Context, conn net.Conn, a *heldAnswer) error {
	headDone, next := false, 0
	for {
		changed := a.placer.RunPlaceChanged()
		place := a.placer.RunPlace()
		all := place.SetEnd || place.Staging != a.staging
		due := func(t time.Time) bool { return all || a.release(place, t) }
		var out []byte
		if !headDone && due(a.headAt) {
			out = append(out, a.head...)
			headDone = true
		}
		for headDone && next < len(a.pieces) && due(a.pieces[next].at) {
			out = appendChunk(out, a.pieces[next].data)
			next++
			if next == len(a.pieces) {
				out = append(out, "0\r\n\r\n"...)
			}
		}
		if len(out) > 0 {
			if _, err := conn.Write(out); err != nil {
				return err
			}
		}
		if headDone && next == len(a.pieces) {
			return nil
		}
		idle, ok := a.placer.IdleUntilRunPlaceChanges(changed, func() func() { return integrations.MarkIdle(ctx) })
		if !ok {
			// The place changed since it was read: read it again.
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
		}
		idle()
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// appendChunk appends data to out as one chunk; an empty one appends nothing,
// as a zero-size chunk would end the body.
func appendChunk(out, data []byte) []byte {
	if len(data) == 0 {
		return out
	}
	out = strconv.AppendInt(out, int64(len(data)), 16)
	out = append(out, "\r\n"...)
	out = append(out, data...)
	return append(out, "\r\n"...)
}
