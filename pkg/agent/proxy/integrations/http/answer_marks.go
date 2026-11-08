package http

import (
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg/models"
)

// respWrite is one write the relay made of a response to the client: where
// in the response's bytes it ended, and when it was written.
type respWrite struct {
	end int
	at  time.Time
}

// noteWrite notes a write that ended at end, written at at.
func noteWrite(writes *[]respWrite, end int, at time.Time) {
	*writes = append(*writes, respWrite{end: end, at: at})
}

// answerMarks is the marks of an answer the relay wrote to the client
// (models.HTTPAnswerMarks): the time of the write that completed its head,
// then, for each write, the offset in the body as kept (de-chunked and
// decompressed, bodyLen long) it ended at, and its time. A compressed body is
// decoded write by write, so an offset is what the bytes written so far
// decode to.
//
// It is nil for an answer it cannot place write by write: a write with no
// time, framing it does not read (a Content-Length that is no length, or a
// negative one), a body whose framing or encoding does not read back to
// bodyLen bytes.
// Such an answer is written at once, as a recording with no marks is.
func answerMarks(resp []byte, writes []respWrite, encoding string, bodyLen int) *models.HTTPAnswerMarks {
	if len(writes) == 0 {
		return nil
	}
	for _, w := range writes {
		if w.at.IsZero() {
			return nil
		}
	}
	if len(writes) == 1 {
		// One write ends the body, whatever its framing.
		at := writes[0].at
		return &models.HTTPAnswerMarks{Head: at, Writes: []models.HTTPAnswerWrite{{Offset: bodyLen, TS: at}}}
	}
	head, bodyStart, _, ok := messageHead(resp, true)
	if !ok {
		return nil
	}
	// The head's write is the first that reached its end; the last write,
	// which ends at the response's end, always does.
	first := len(writes) - 1
	for i, w := range writes {
		if w.end >= bodyStart {
			first = i
			break
		}
	}
	marks := &models.HTTPAnswerMarks{Head: writes[first].at}
	bodyWrites := writes[first:]
	ends := make([]int, len(bodyWrites))
	for i, w := range bodyWrites {
		ends[i] = w.end - bodyStart
	}
	wire := resp[bodyStart:]
	contentLength, transferEncoding := parseHeaders(head)
	var decoded []int
	switch {
	case strings.Contains(strings.ToLower(transferEncoding), "chunked"):
		var data []byte
		if decoded, data, ok = dechunkedAt(wire, ends); !ok {
			return nil
		}
		wire = data
	case contentLength != "":
		// A length that does not read as one, or a negative one, is framing
		// this does not read. Bytes after the declared length are no part
		// of the body.
		n, err := contentLengthValue(contentLength)
		if err != nil || n < 0 {
			return nil
		}
		n = min(n, len(wire))
		wire = wire[:n]
		decoded = make([]int, len(ends))
		for i, e := range ends {
			decoded[i] = min(e, n)
		}
	default:
		decoded = ends
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "br":
		if decoded, ok = decodedAt(strings.ToLower(strings.TrimSpace(encoding)), wire, decoded); !ok {
			return nil
		}
	}
	if decoded[len(decoded)-1] != bodyLen {
		return nil
	}
	// A write that added nothing to the body is no part of its own; the
	// last write, which ends the answer, is kept whatever it added.
	last, prev := len(bodyWrites)-1, 0
	for i, w := range bodyWrites {
		if i < last && decoded[i] <= prev {
			continue
		}
		marks.Writes = append(marks.Writes, models.HTTPAnswerWrite{Offset: decoded[i], TS: w.at})
		prev = decoded[i]
	}
	return marks
}

// dechunkedAt is, for each wire offset in ends (ascending, into a chunked
// body's bytes), the count of body bytes the chunks before it carry, a
// chunk the offset cuts counted to the cut, and the body's data. ok is false
// for framing it does not read.
func dechunkedAt(wire []byte, ends []int) (decoded []int, data []byte, ok bool) {
	decoded = make([]int, len(ends))
	pos, ei := 0, 0
	for pos < len(wire) {
		line := bytes.Index(wire[pos:], []byte("\r\n"))
		if line < 0 {
			break
		}
		sizeField, _, _ := strings.Cut(string(wire[pos:pos+line]), ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeField), 16, 0)
		if err != nil || size < 0 || size > int64(len(wire)) {
			return nil, nil, false
		}
		start := pos + line + 2
		for ei < len(ends) && ends[ei] <= start {
			decoded[ei] = len(data)
			ei++
		}
		if size == 0 {
			break
		}
		end := start + int(size)
		for ei < len(ends) && ends[ei] < end {
			decoded[ei] = len(data) + max(ends[ei]-start, 0)
			ei++
		}
		if end > len(wire) {
			return nil, nil, false
		}
		data = append(data, wire[start:end]...)
		pos = end + 2
	}
	for ; ei < len(ends); ei++ {
		decoded[ei] = len(data)
	}
	return decoded, data, true
}

// decodedAt is, for each offset in ends (ascending, into data, a body
// compressed with encoding), the count of bytes its decoder gives once it has
// read data up to it: what a client that decodes as it reads had of the body
// then. ok is false when data does not decode.
func decodedAt(encoding string, data []byte, ends []int) ([]int, bool) {
	feed := &feedReader{in: make(chan []byte), need: make(chan struct{})}
	done := make(chan struct{})
	produced := 0
	var failed bool
	go func() {
		defer close(done)
		var r io.Reader = brotli.NewReader(feed)
		if encoding == "gzip" {
			zr, err := gzip.NewReader(feed)
			if err != nil {
				failed = true
				return
			}
			r = zr
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			produced += n
			if err == io.EOF {
				return
			}
			if err != nil {
				// A body cut where the feed ended reads short, which the
				// caller's check against the body's length finds.
				failed = !feed.closed
				return
			}
		}
	}()
	out := make([]int, len(ends))
	prev := 0
	for i, end := range ends {
		// The decoder has asked for more: it has given all the bytes before
		// prev decode to. One that ended has given all: bytes after its
		// stream's end decode to nothing.
		select {
		case <-feed.need:
		case <-done:
			for j := max(i-1, 0); j < len(out); j++ {
				out[j] = produced
			}
			return out, !failed
		}
		if i > 0 {
			out[i-1] = produced
		}
		feed.in <- data[prev:end]
		prev = end
	}
	select {
	case <-feed.need:
		close(feed.in)
		<-done
	case <-done:
	}
	out[len(out)-1] = produced
	return out, !failed
}

// feedReader gives its reader the bytes sent on in, one send at a time, and
// says on need each time it has none left to give.
type feedReader struct {
	in     chan []byte
	need   chan struct{}
	cur    []byte
	closed bool
}

func (f *feedReader) Read(p []byte) (int, error) {
	for len(f.cur) == 0 {
		f.need <- struct{}{}
		b, ok := <-f.in
		if !ok {
			f.closed = true
			return 0, io.EOF
		}
		f.cur = b
	}
	n := copy(p, f.cur)
	f.cur = f.cur[n:]
	return n, nil
}
