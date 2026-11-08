package http

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
	yamlLib "gopkg.in/yaml.v3"
)

var marksBase = time.Unix(1_700_000_000, 0).UTC()

// markAt is marksBase plus n seconds: write n's time in these rows.
func markAt(n int) time.Time { return marksBase.Add(time.Duration(n) * time.Second) }

// recordWrites records req answered by pieces, piece i written to the client
// at writtenAt[i], and returns the mock.
func recordWrites(t *testing.T, req string, pieces []string, writtenAt []time.Time) *models.Mock {
	t.Helper()
	newStream := func(dir fakeconn.Direction, parts []string, times []time.Time) *fakeconn.FakeConn {
		ch := make(chan fakeconn.Chunk, len(parts))
		for i, p := range parts {
			ch <- fakeconn.Chunk{Dir: dir, ConnSeq: fakeconn.ConnSeqOf(uint64(i + 1)), Bytes: []byte(p),
				ReadAt: times[i], WrittenAt: times[i], SeqNo: uint64(i + 1)}
		}
		close(ch)
		c := fakeconn.New(ch, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 2})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	mocks := make(chan *models.Mock, 2)
	sess := &supervisor.Session{
		ClientStream: newStream(fakeconn.FromClient, []string{req}, []time.Time{marksBase}),
		DestStream:   newStream(fakeconn.FromDest, pieces, writtenAt),
		Mocks:        mocks, Logger: zaptest.NewLogger(t), ClientConnID: "marks-1", DestConnID: "marks-dest-1",
		Ctx: context.Background(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (&HTTP{Logger: zaptest.NewLogger(t)}).recordV2(ctx, sess); err != nil {
		t.Fatalf("recordV2: %v", err)
	}
	select {
	case m := <-mocks:
		return m
	default:
		t.Fatal("no mock recorded")
		return nil
	}
}

const marksReq = "GET /events HTTP/1.1\r\nHost: upstream.test\r\n\r\n"

func chunk(b string) string { return fmt.Sprintf("%x\r\n%s\r\n", len(b), b) }

func wantMarks(t *testing.T, got *models.HTTPAnswerMarks, head time.Time, writes ...models.HTTPAnswerWrite) {
	t.Helper()
	if got == nil {
		t.Fatalf("marks = nil, want head %v and %d writes", head, len(writes))
	}
	if !got.Head.Equal(head) {
		t.Errorf("head = %v, want %v", got.Head, head)
	}
	if len(got.Writes) != len(writes) {
		t.Fatalf("writes = %+v, want %+v", got.Writes, writes)
	}
	for i := range writes {
		if got.Writes[i].Offset != writes[i].Offset || !got.Writes[i].TS.Equal(writes[i].TS) {
			t.Errorf("write %d = %+v, want %+v", i, got.Writes[i], writes[i])
		}
	}
}

func w(offset, sec int) models.HTTPAnswerWrite {
	return models.HTTPAnswerWrite{Offset: offset, TS: markAt(sec)}
}

// An SSE stream the upstream sent in three writes keeps one mark per write,
// at its offset in the de-chunked body, and its head's time. Its YAML keeps
// each frame at its write's time, and the marks on the body its frames read
// back as, each piece ending after its frame's blank line.
func TestRecordKeepsAMarkPerWriteOfAnSSEStream(t *testing.T) {
	head := "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n"
	m := recordWrites(t, marksReq, []string{
		head + chunk("data: a\n\n"),
		chunk("data: b\n\n"),
		chunk("data: c\n\n") + "0\r\n\r\n",
	}, []time.Time{markAt(1), markAt(2), markAt(3)})
	resp := m.Spec.HTTPResp
	if resp.Body != "data: a\n\ndata: b\n\ndata: c\n\n" {
		t.Fatalf("body = %q", resp.Body)
	}
	wantMarks(t, resp.Marks, markAt(1), w(9, 1), w(18, 2), w(27, 3))
	if !m.Spec.ResTimestampMock.Equal(markAt(3)) {
		t.Errorf("ResTimestampMock = %v, want the last write's %v", m.Spec.ResTimestampMock, markAt(3))
	}

	out, err := yamlLib.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var back models.HTTPResp
	if err := yamlLib.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.StreamBody) != 3 {
		t.Fatalf("frames = %d, want 3:\n%s", len(back.StreamBody), out)
	}
	for i, f := range back.StreamBody {
		if !f.TS.Equal(markAt(i + 1)) {
			t.Errorf("frame %d ts = %v, want its write's %v", i, f.TS, markAt(i+1))
		}
	}
	// The frames read back as "data:a" (the parse took the space off): the
	// marks are on that body, not the recorded one.
	wantMarks(t, back.Marks, markAt(1), w(8, 1), w(16, 2), w(len(back.Body), 3))
	for i, piece := range []string{"data:a\n\n", "data:b\n\n", "data:c\n\n"} {
		start := 0
		if i > 0 {
			start = back.Marks.Writes[i-1].Offset
		}
		end := back.Marks.Writes[i].Offset
		if end < start || end > len(back.Body) {
			t.Fatalf("mark %d at %d does not fit after %d in a body of %d: %q", i, end, start, len(back.Body), back.Body)
		}
		if got := back.Body[start:end]; got != piece {
			t.Errorf("piece %d = %q, want %q", i, got, piece)
		}
	}
	again, err := yamlLib.Marshal(back)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, out) {
		t.Errorf("a read-back recording writes other YAML:\n%s\nwant\n%s", again, out)
	}
}

// A head the upstream sent before its body keeps its own time, and a body in
// two writes under a Content-Length keeps both.
func TestRecordKeepsTheHeadsOwnTime(t *testing.T) {
	m := recordWrites(t, marksReq, []string{
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 11\r\n\r\n",
		`{"a":`,
		`"done"}`,
	}, []time.Time{markAt(1), markAt(2), markAt(4)})
	wantMarks(t, m.Spec.HTTPResp.Marks, markAt(1), w(5, 2), w(11, 4))

	two := recordWrites(t, marksReq, []string{
		"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n",
		"ok",
	}, []time.Time{markAt(1), markAt(3)})
	wantMarks(t, two.Spec.HTTPResp.Marks, markAt(1), w(2, 3))
}

// Bytes after a Content-Length are no part of the body, and a body that
// does not read back to the length kept keeps no marks.
func TestAnswerMarksReadsTheBodyAsKept(t *testing.T) {
	head := "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\n"
	resp := []byte(head + "okEXTRA")
	writes := []respWrite{{end: len(head), at: markAt(1)}, {end: len(resp), at: markAt(2)}}
	wantMarks(t, answerMarks(resp, writes, "", 2), markAt(1), w(2, 2))
	if m := answerMarks(resp, writes, "", 3); m != nil {
		t.Errorf("a body kept at another length: marks = %+v, want none", m)
	}
}

// A Content-Length that is no length, or a negative one, is framing
// answerMarks does not read: the answer keeps no marks, and reading it never
// panics. Go's reader takes such a head when the line is folded onto the one
// before it: Go joins it to that header, and keploy's header scan reads it as
// a Content-Length of its own.
func TestAnswerMarksReadsNoLengthItCannot(t *testing.T) {
	for _, tc := range []struct {
		name    string
		resp    string
		bodyLen int
	}{
		{"a negative length on a 304", "HTTP/1.1 304 Not Modified\r\nContent-Length: -5\r\n\r\n", 0},
		{"a length of -1 on a 304", "HTTP/1.1 304 Not Modified\r\nContent-Length: -1\r\n\r\n", 0},
		{"a length that is no number on a 304", "HTTP/1.1 304 Not Modified\r\nContent-Length: x5\r\n\r\n", 0},
		{"a negative length folded into a close-delimited 200",
			"HTTP/1.1 200 OK\r\nConnection: close\r\nX-A: b\r\n Content-Length: -5\r\n\r\nhello", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := []byte(tc.resp)
			writes := []respWrite{{end: 20, at: markAt(1)}, {end: len(resp), at: markAt(2)}}
			marks := func() *models.HTTPAnswerMarks {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("answerMarks(%q) panicked: %v", tc.resp, r)
					}
				}()
				return answerMarks(resp, writes, "", tc.bodyLen)
			}()
			if marks != nil {
				t.Errorf("answerMarks(%q) = %+v, want none", tc.resp, marks)
			}
		})
	}
}

// A Content-Length of 0 is a length: an answer whose head took two writes
// keeps its marks, at the write that ended its head.
func TestAnswerMarksKeepsAZeroLength(t *testing.T) {
	resp := []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
	writes := []respWrite{{end: 20, at: markAt(1)}, {end: len(resp), at: markAt(2)}}
	wantMarks(t, answerMarks(resp, writes, "", 0), markAt(2), w(0, 2))
}

// A response whose folded line keploy's header scan reads as a negative
// Content-Length is recorded as Go reads it, with no marks: the record does
// not panic and lose the exchange.
func TestRecordKeepsNoMarksForALengthItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pieces []string
		status int
		body   string
	}{
		{"a 304", []string{"HTTP/1.1 304 Not Modified\r\nX-A: b\r\n", " Content-Length: -5\r\n\r\n"}, 304, ""},
		{"a 200 the connection's close ends",
			[]string{"HTTP/1.1 200 OK\r\nConnection: close\r\nX-A: b\r\n", " Content-Length: -5\r\n\r\nhello"}, 200, "hello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := func() *models.Mock {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("recordV2 panicked: %v", r)
					}
				}()
				return recordWrites(t, marksReq, tc.pieces, []time.Time{markAt(1), markAt(2)})
			}()
			resp := m.Spec.HTTPResp
			if resp.StatusCode != tc.status || resp.Body != tc.body {
				t.Errorf("recorded %d %q, want %d %q", resp.StatusCode, resp.Body, tc.status, tc.body)
			}
			if resp.Marks != nil {
				t.Errorf("marks = %+v, want none", resp.Marks)
			}
		})
	}
}

// An answer the upstream sent in one write keeps one mark, at its head's
// time: marks are what lets replay hold an answer. So does one whose head and
// whole body went out in the same last write, after a head split in two, and
// one in one write whose body is framed or compressed, which is not decoded
// again to say where its one write ended.
func TestRecordKeepsOneMarkForAnAnswerInOneWrite(t *testing.T) {
	one := recordWrites(t, marksReq, []string{"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"}, []time.Time{markAt(1)})
	wantMarks(t, one.Spec.HTTPResp.Marks, markAt(1), w(2, 1))
	split := recordWrites(t, marksReq, []string{"HTTP/1.1 200 OK\r\n", "Content-Length: 2\r\n\r\nok"}, []time.Time{markAt(1), markAt(2)})
	wantMarks(t, split.Spec.HTTPResp.Marks, markAt(2), w(2, 2))
	framed := recordWrites(t, marksReq, []string{"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + chunk("ok") + "0\r\n\r\n"}, []time.Time{markAt(1)})
	wantMarks(t, framed.Spec.HTTPResp.Marks, markAt(1), w(2, 1))
	raw := []byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\nabc")
	wantMarks(t, answerMarks(raw, []respWrite{{end: len(raw), at: markAt(1)}}, "gzip", 9), markAt(1), w(9, 1))
	if m := answerMarks(raw, nil, "", 3); m != nil {
		t.Errorf("no writes: marks = %+v, want none", m)
	}
}

// A close-delimited answer (no Content-Length, not chunked) keeps a mark per
// write, as its body is what the relay wrote until the connection closed.
func TestRecordMarksAnAnswerTheConnectionsCloseEnds(t *testing.T) {
	m := recordWrites(t, marksReq, []string{"HTTP/1.1 200 OK\r\n\r\nab", "cd"}, []time.Time{markAt(1), markAt(2)})
	if m.Spec.HTTPResp.Body != "abcd" {
		t.Fatalf("body %q", m.Spec.HTTPResp.Body)
	}
	wantMarks(t, m.Spec.HTTPResp.Marks, markAt(1), w(2, 1), w(4, 2))
}

// A write the capture gave no time keeps no marks at all: a part with no
// place cannot be placed.
func TestRecordKeepsNoMarksForAWriteWithNoTime(t *testing.T) {
	m := recordWrites(t, marksReq, []string{
		"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nab",
		"cd",
	}, []time.Time{markAt(1), {}})
	if m.Spec.HTTPResp.Marks != nil {
		t.Errorf("marks = %+v, want none", m.Spec.HTTPResp.Marks)
	}
	out, err := yamlLib.Marshal(m.Spec.HTTPResp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "marks") {
		t.Errorf("a recording with no marks writes a marks key:\n%s", out)
	}
}

// compressedPieces compresses each of lines with enc, flushed after each, and
// closed after the last: the bytes the upstream wrote each time.
func compressedPieces(t *testing.T, enc string, lines []string) []string {
	t.Helper()
	var buf bytes.Buffer
	type flusher interface {
		Write([]byte) (int, error)
		Flush() error
		Close() error
	}
	var zw flusher
	if enc == "gzip" {
		zw = gzip.NewWriter(&buf)
	} else {
		zw = brotli.NewWriter(&buf)
	}
	var pieces []string
	for i, l := range lines {
		if _, err := zw.Write([]byte(l)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Flush(); err != nil {
			t.Fatal(err)
		}
		if i == len(lines)-1 {
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
		}
		pieces = append(pieces, buf.String())
		buf.Reset()
	}
	return pieces
}

// A compressed stream is decoded write by write: each mark is the length of
// what the bytes written so far decode to, in the body as kept.
func TestRecordMarksACompressedStreamInTheDecodedBody(t *testing.T) {
	for _, enc := range []string{"gzip", "br"} {
		t.Run(enc, func(t *testing.T) {
			z := compressedPieces(t, enc, []string{`{"n":1}` + "\n", `{"n":2}` + "\n"})
			head := "HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nContent-Encoding: " + enc + "\r\nTransfer-Encoding: chunked\r\n\r\n"
			m := recordWrites(t, marksReq, []string{
				head + chunk(z[0]),
				chunk(z[1]),
				"0\r\n\r\n",
			}, []time.Time{markAt(1), markAt(2), markAt(3)})
			if m.Spec.HTTPResp.Body != `{"n":1}`+"\n"+`{"n":2}`+"\n" {
				t.Fatalf("body = %q", m.Spec.HTTPResp.Body)
			}
			wantMarks(t, m.Spec.HTTPResp.Marks, markAt(1), w(8, 1), w(16, 2), w(16, 3))
		})
	}
}

// answerMarks reads a chunked body cut by writes anywhere: in a chunk's size
// line, in its data, and in the CRLF after it.
func TestAnswerMarksReadsChunksCutAnywhere(t *testing.T) {
	head := "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"
	resp := head + chunk("hello") + chunk("world") + "0\r\n\r\n"
	b := len(head)
	cuts := []respWrite{
		{end: b + 1, at: markAt(1)},  // in the first size line: no body yet
		{end: b + 5, at: markAt(2)},  // "he"
		{end: b + 11, at: markAt(3)}, // "hello", its CRLF, and into the next size line
		{end: b + 13, at: markAt(4)}, // the end of that size line: nothing more
		{end: len(resp), at: markAt(5)},
	}
	marks := answerMarks([]byte(resp), cuts, "", 10)
	wantMarks(t, marks, markAt(1), w(2, 2), w(5, 3), w(10, 5))
}

// wholeInTwoWrites is a whole Content-Length answer the relay wrote in two
// writes, and those writes.
func wholeInTwoWrites() ([]byte, []respWrite) {
	resp := []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nabcd")
	return resp, []respWrite{{end: len(resp) - 2, at: markAt(1)}, {end: len(resp), at: markAt(2)}}
}

// A whole answer the upstream ended in a fault after it is recorded with no
// fault and keeps its marks, as any whole answer; one its client read in part
// keeps none: the rest has no place.
func TestRecordMarksAWholeAnswerOnlyAsTheClientReadItAll(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	build := func(m *FinalHTTP) *models.Mock {
		t.Helper()
		m.Req, m.ReqTimestampMock, m.ResTimestampMock = []byte(marksReq), markAt(0), markAt(2)
		mock, err := h.buildHTTPMock(m, 80, "marks-conn", models.OutgoingOptions{})
		if err != nil || mock == nil {
			t.Fatalf("buildHTTPMock = (%v, %v)", mock, err)
		}
		return mock
	}
	resp, writes := wholeInTwoWrites()
	reset := build(&FinalHTTP{Resp: resp, RespWrites: writes,
		Fault: &models.Fault{Kind: models.FaultReset, AfterHead: true}})
	if reset.Spec.Fault != nil {
		t.Fatalf("a whole answer is recorded with a fault: %+v", reset.Spec.Fault)
	}
	wantMarks(t, reset.Spec.HTTPResp.Marks, markAt(1), w(2, 1), w(4, 2))

	resp, writes = wholeInTwoWrites()
	if m := build(&FinalHTTP{Resp: resp, RespWrites: writes, RespReadInPart: true}).Spec.HTTPResp.Marks; m != nil {
		t.Errorf("read in part: marks = %+v, want none", m)
	}
	resp, writes = wholeInTwoWrites()
	if m := build(&FinalHTTP{Resp: resp[:len(resp)-1], RespWrites: writes[:1], RespReadInPart: true}).Spec.HTTPResp.Marks; m != nil {
		t.Errorf("cut: marks = %+v, want none", m)
	}
}

// An answer a fault ended before its body did keeps no marks: a reset before
// its head was whole, which records no answer, and one after its head, which
// records the bytes on the wire and then the fault. Replay writes either as
// the fault says, at once.
func TestRecordKeepsNoMarksForAFaultBeforeTheBodyEnded(t *testing.T) {
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	resp, writes := wholeInTwoWrites()
	head := []byte("HTTP/1.1 200 OK\r\nContent-Le")
	for _, c := range []struct {
		name string
		m    *FinalHTTP
	}{
		{"a reset before the head was whole", &FinalHTTP{Resp: head,
			RespWrites: []respWrite{{end: len(head), at: markAt(1)}},
			Fault:      &models.Fault{Kind: models.FaultReset}}},
		{"a reset after the head, the body cut", &FinalHTTP{Resp: resp[:len(resp)-1], RespWrites: writes[:1],
			Fault: &models.Fault{Kind: models.FaultReset, AfterHead: true}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.m.Req, c.m.ReqTimestampMock, c.m.ResTimestampMock = []byte(marksReq), markAt(0), markAt(2)
			mock, err := h.buildHTTPMock(c.m, 80, "marks-conn", models.OutgoingOptions{})
			if err != nil || mock == nil {
				t.Fatalf("buildHTTPMock = (%v, %v)", mock, err)
			}
			if mock.Spec.Fault == nil {
				t.Fatal("precondition: the mock keeps no fault")
			}
			if r := mock.Spec.HTTPResp; r != nil && r.Marks != nil {
				t.Errorf("marks = %+v, want none", r.Marks)
			}
		})
	}
}

// The terminal chunk of an NDJSON stream, written later than its last line,
// keeps its own mark at the body's end.
func TestRecordKeepsTheTerminalChunksOwnMark(t *testing.T) {
	m := recordWrites(t, marksReq, []string{
		"HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nTransfer-Encoding: chunked\r\n\r\n" + chunk("{\"n\":1}\n"),
		chunk("{\"n\":2}\n"),
		"0\r\n\r\n",
	}, []time.Time{markAt(1), markAt(2), markAt(5)})
	wantMarks(t, m.Spec.HTTPResp.Marks, markAt(1), w(8, 1), w(16, 2), w(16, 5))
}

// dechunkedAt places each end where it falls, ends short of the wire too, and
// reads only framing it can: a size line that is empty, not hex, negative, or
// longer than the bytes after it is no framing (ok false), never a panic.
func TestDechunkedAtReadsOnlyFramingItCan(t *testing.T) {
	wire := chunk("abc") + chunk("def") + "0\r\n\r\n"
	for _, tc := range []struct {
		name    string
		wire    string
		ends    []int
		decoded []int
		ok      bool
	}{
		{"an end at each chunk's frame end", wire, []int{8, 16, len(wire)}, []int{3, 6, 6}, true},
		{"one end inside the first chunk", wire, []int{4}, []int{1}, true},
		{"one end before the second chunk", wire, []int{8}, []int{3}, true},
		{"an empty size line", "\r\nabc\r\n0\r\n\r\n", []int{12}, nil, false},
		{"a size that is not hex", "zz\r\nabc\r\n0\r\n\r\n", []int{14}, nil, false},
		{"a negative size", "-3\r\nabc\r\n0\r\n\r\n", []int{14}, nil, false},
		{"a size past the wire", "7fffffffffffffff\r\nabc\r\n0\r\n\r\n", []int{28}, nil, false},
		{"a size within the wire, past the bytes after its line", "c\r\nabc\r\n0\r\n\r\n", []int{13}, nil, false},
		{"a size one past the bytes after its line", "4\r\nabc", []int{6}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, data, ok := func() (decoded []int, data []byte, ok bool) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("dechunkedAt(%q, %v) panicked: %v", tc.wire, tc.ends, r)
					}
				}()
				return dechunkedAt([]byte(tc.wire), tc.ends)
			}()
			if ok != tc.ok {
				t.Fatalf("dechunkedAt(%q, %v) ok = %v, want %v (decoded %v)", tc.wire, tc.ends, ok, tc.ok, decoded)
			}
			if !ok {
				return
			}
			if string(data) != "abcdef" || fmt.Sprint(decoded) != fmt.Sprint(tc.decoded) {
				t.Errorf("dechunkedAt(%q, %v) = %v, %q; want %v, %q", tc.wire, tc.ends, decoded, data, tc.decoded, "abcdef")
			}
		})
	}
}

// A chunk the wire ends with, before its CRLF, is read whole.
func TestDechunkedAtReadsAChunkTheWireEndsWith(t *testing.T) {
	decoded, data, ok := dechunkedAt([]byte("3\r\nabc"), []int{6})
	if !ok || string(data) != "abc" || fmt.Sprint(decoded) != "[3]" {
		t.Fatalf("dechunkedAt = %v, %q, %v; want [3], %q, true", decoded, data, ok, "abc")
	}
}

// decodedAt is, at each cut of a compressed body, what a client decoding as
// it reads had of it; a body its decoder refuses before the last cut does
// not decode, and does not panic.
func TestDecodedAtReadsEachCut(t *testing.T) {
	call := func(enc string, data []byte, ends []int) (out []int, ok bool) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("decodedAt(%s, %d bytes, %v) panicked: %v", enc, len(data), ends, r)
			}
		}()
		return decodedAt(enc, data, ends)
	}
	for _, enc := range []string{"gzip", "br"} {
		z := compressedPieces(t, enc, []string{"hello\n", "world\n"})
		data := []byte(z[0] + z[1])
		if out, ok := call(enc, data, []int{len(z[0]), len(data), len(data)}); !ok || fmt.Sprint(out) != "[6 12 12]" {
			t.Errorf("%s: decodedAt = %v, %v; want [6 12 12], true", enc, out, ok)
		}
	}
	if out, ok := call("gzip", []byte("not a gzip body"), []int{10, 12, 15}); ok {
		t.Errorf("a body that is not gzip: decodedAt = %v, true; want false", out)
	}
}
