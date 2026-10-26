package http

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// preStreamResp has the shape of a keploy <= v3.5.84 agent's AgentResp: its
// error is an interface gob cannot encode a value of unless registered.
type preStreamResp struct {
	Error     error
	IsSuccess bool
}

// preStreamBody is that agent's 400 body: the type, whole, and no value.
func preStreamBody(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(preStreamResp{Error: errors.New("gob: bad data")}); err == nil || b.Len() == 0 {
		t.Fatalf("building the pre-stream agent's reply: encode returned %v after %d bytes; want an error after the type", err, b.Len())
	}
	return b.Bytes()
}

// decodeErr is what decodeStoreMocksResp's decode returns for body.
func decodeErr(body []byte) error {
	var v preStreamResp
	return gob.NewDecoder(bytes.NewReader(body)).Decode(&v)
}

// cutTypeID is a gob message of one byte that holds only the start of a
// two-byte type id: the decoder runs out inside the message.
var cutTypeID = []byte{0x01, 0xfe}

// The pre-stream agent's reply is told by its media type and by holding whole
// type definitions and nothing more. Each part is needed: a caller takes the
// answer as permanent. Each row's decode error is the one the decoder gives
// for its body, but where the row is about that error.
func TestPreStreamAgentReply(t *testing.T) {
	old := preStreamBody(t)
	if err := decodeErr(old); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("decoding the pre-stream agent's reply returned %v; want io.ErrUnexpectedEOF", err)
	}
	past := preStreamBodyOfSize(t, maxStoreMocksFailBody+1)
	text := []byte("Bad Request\n")
	cut := old[:len(old)-1]
	cutID := append(append([]byte(nil), cutTypeID...), old...)
	for name, body := range map[string][]byte{"a text body": text, "the reply cut short": cut, "a body past the bound": past, "a type id cut off in its message": cutID} {
		if err := decodeErr(body); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("decoding %s returned %v; want io.ErrUnexpectedEOF, or its row would not reach the check it is for", name, err)
		}
	}

	cases := []struct {
		name        string
		contentType string
		body        []byte
		decodeErr   error
		want        bool
	}{
		{"that agent's reply", "application/x-gob", old, decodeErr(old), true},
		{"that agent's reply with a media type parameter", "application/x-gob; charset=binary", old, decodeErr(old), true},
		{"a text media type", "text/plain; charset=utf-8", old, decodeErr(old), false},
		{"no media type", "", old, decodeErr(old), false},
		{"a malformed media type", "application/x-gob; =", old, decodeErr(old), false},
		{"a decode that ended at io.EOF", "application/x-gob", old, io.EOF, false},
		{"a decode that failed otherwise", "application/x-gob", old, errors.New("gob: bad data"), false},
		{"a text body", "application/x-gob", text, decodeErr(text), false},
		{"the reply cut short", "application/x-gob", cut, decodeErr(cut), false},
		{"a type id cut off in its message", "application/x-gob", cutID, decodeErr(cutID), false},
		{"a body past the bound", "application/x-gob", past, decodeErr(past), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := preStreamAgentReply(c.contentType, c.body, c.decodeErr); got != c.want {
				t.Fatalf("preStreamAgentReply(%q, %d bytes, %v) = %v; want %v", c.contentType, len(c.body), c.decodeErr, got, c.want)
			}
		})
	}
}

// gobTypeDefinitionsOnly takes whole type definitions and nothing else, read
// as gob frames them.
func TestGobTypeDefinitionsOnly(t *testing.T) {
	old := preStreamBody(t)
	// A value after the type: what an encoder that did not fail sends.
	var whole bytes.Buffer
	if err := gob.NewEncoder(&whole).Encode(struct{ IsSuccess bool }{true}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		body []byte
		want bool
	}{
		{"one type definition", old, true},
		{"two type definitions", append(append([]byte(nil), old...), old...), true},
		{"nothing", nil, false},
		{"a zero-length message", append([]byte{0}, old...), false},
		{"a value after the type", whole.Bytes(), false},
		{"a type definition cut short", old[:len(old)-1], false},
		{"a type id cut off in its message", append(append([]byte(nil), cutTypeID...), old...), false},
		{"a message length of more than 8 bytes", append([]byte{0xf7, 0, 0, 0, 0, 0, 0, 0, 0, 1}, old...), false},
	} {
		if got := gobTypeDefinitionsOnly(c.body); got != c.want {
			t.Errorf("gobTypeDefinitionsOnly(%s) = %v; want %v", c.name, got, c.want)
		}
	}
}

// gobUint reads gob's unsigned integers as encoding/gob writes them.
func TestGobUint(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want uint64
		ok   bool
	}{
		{[]byte{0x00}, 0, true},
		{[]byte{0x7f}, 127, true},
		{[]byte{0xff, 0x80}, 128, true},
		{[]byte{0xfe, 0x01, 0x00}, 256, true},
		{[]byte{0xf8, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ^uint64(0), true},
		{[]byte{0xfe, 0x01}, 0, false},
		{[]byte{0x80}, 0, false},
		{[]byte{0xf7, 0, 0, 0, 0, 0, 0, 0, 0, 0}, 0, false},
		{nil, 0, false},
	} {
		in := c.in
		if c.ok {
			// What follows the integer is left for the caller.
			in = append(append([]byte(nil), c.in...), 0x2a)
		}
		got, rest, ok := gobUint(in)
		if ok != c.ok || (ok && (got != c.want || len(rest) != 1 || rest[0] != 0x2a)) {
			t.Errorf("gobUint(% x) = %d, % x, %v; want %d, %v", in, got, rest, ok, c.want, c.ok)
		}
	}
}

// preStreamBodyOfSize is a reply of that agent's form, a type whole and no
// value, of exactly n bytes: its type has a bool field whose name is as long
// as it takes.
func preStreamBodyOfSize(t *testing.T, n int) []byte {
	t.Helper()
	for l := n - 100; l < n; l++ {
		v := reflect.New(reflect.StructOf([]reflect.StructField{
			{Name: "A" + strings.Repeat("a", l), Type: reflect.TypeOf(false)},
			{Name: "Error", Type: reflect.TypeOf((*error)(nil)).Elem()},
		})).Elem()
		v.Field(1).Set(reflect.ValueOf(errors.New("gob: bad data")))
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).EncodeValue(v); err == nil {
			t.Fatal("encoding a value whose error is not registered succeeded")
		}
		if b.Len() == n {
			return b.Bytes()
		}
	}
	t.Fatalf("no field name length gives a reply of %d bytes", n)
	return nil
}

// preStreamBodyPast is a reply of that agent's form, type definitions and no
// value, longer than n bytes and with a definition that ends at byte n. Its
// type holds a struct whose bool field's name is as long as it takes, another
// struct, sent after it, and the error. The first struct's name repeats its
// fields, so a field name grows it by two bytes; a second field, named B,
// sets its parity: "B bool" and "B int" differ by one byte.
func preStreamBodyPast(t *testing.T, n int) []byte {
	t.Helper()
	boolType := reflect.TypeOf(false)
	for l := n/2 - 100; l < n/2; l++ {
		for _, extra := range [][]reflect.StructField{nil, {{Name: "B", Type: boolType}}, {{Name: "B", Type: reflect.TypeOf(0)}}} {
			inner := reflect.StructOf(append([]reflect.StructField{{Name: "A" + strings.Repeat("a", l), Type: boolType}}, extra...))
			v := reflect.New(reflect.StructOf([]reflect.StructField{
				{Name: "In", Type: inner},
				{Name: "Sm", Type: reflect.StructOf([]reflect.StructField{{Name: "C", Type: boolType}})},
				{Name: "Error", Type: reflect.TypeOf((*error)(nil)).Elem()},
			})).Elem()
			v.Field(2).Set(reflect.ValueOf(errors.New("gob: bad data")))
			var b bytes.Buffer
			if err := gob.NewEncoder(&b).EncodeValue(v); err == nil {
				t.Fatal("encoding a value whose error is not registered succeeded")
			}
			body := b.Bytes()
			if len(body) <= n || !gobTypeDefinitionsOnly(body) || !gobTypeDefinitionsOnly(body[:n]) {
				continue
			}
			if err := decodeErr(body); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("decoding the type definitions returned %v; want io.ErrUnexpectedEOF", err)
			}
			return body
		}
	}
	t.Fatalf("no field name length gives type definitions with one ending at byte %d", n)
	return nil
}

// What is read of a failed reply to judge it is bounded, and the bound is
// exact: a reply of that agent's form that fills it is still the agent's, and
// one a byte longer is not. Nor is one whose first maxStoreMocksFailBody bytes
// are that reply and which goes on with another type definition: the byte past
// the bound is read.
func TestDecodeStoreMocksRespBound(t *testing.T) {
	full := preStreamBodyOfSize(t, maxStoreMocksFailBody)
	for _, c := range []struct {
		name string
		body []byte
		want bool
	}{
		{"a reply that fills the bound", full, true},
		{"a reply of that form a byte past the bound", preStreamBodyOfSize(t, maxStoreMocksFailBody+1), false},
		{"a reply that fills the bound and goes on with another type", append(append([]byte(nil), full...), preStreamBody(t)...), false},
		{"type definitions past the bound, one ending at it", preStreamBodyPast(t, maxStoreMocksFailBody), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": {"application/x-gob"}},
				Body:       io.NopCloser(bytes.NewReader(c.body)),
			}
			err := decodeStoreMocksResp(res)
			if got := errors.Is(err, models.ErrAgentPredatesMockStream); got != c.want {
				t.Fatalf("decodeStoreMocksResp(%d bytes) = %v; called too old: %v, want %v", len(c.body), err, got, c.want)
			}
		})
	}
}

// A failed reply's reason is read whole, however long: the bound is only on
// what is judged.
func TestDecodeStoreMocksRespReadsALongReason(t *testing.T) {
	reason := strings.Repeat("r", 2*maxStoreMocksFailBody)
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(models.AgentResp{ErrorMsg: reason}); err != nil {
		t.Fatal(err)
	}
	res := &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     http.Header{"Content-Type": {"application/x-gob"}},
		Body:       io.NopCloser(&b),
	}
	if err := decodeStoreMocksResp(res); err == nil || !strings.HasSuffix(err.Error(), reason) {
		t.Fatalf("decodeStoreMocksResp of a %d-byte reason did not return it whole (%d bytes of error)", len(reason), len(fmt.Sprint(err)))
	}
}

// endlessReader gives c for ever, as a proxy's endless reply would, up to
// limit bytes, past which it fails; read is what was read of it.
type endlessReader struct {
	c           byte
	read, limit int
}

func (r *endlessReader) Read(p []byte) (int, error) {
	if r.read >= r.limit {
		return 0, errors.New("read past the limit")
	}
	n := min(len(p), r.limit-r.read)
	for i := range p[:n] {
		p[i] = r.c
	}
	r.read += n
	return n, nil
}

// An endless failed reply is read only as far as the bound and what the
// decode asks for, not to its end: gob reads 'A' as a message length and fails
// on the type that follows.
func TestDecodeStoreMocksRespStopsReadingAnEndlessReply(t *testing.T) {
	body := &endlessReader{c: 'A', limit: 64 * maxStoreMocksFailBody}
	res := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:       io.NopCloser(body),
	}
	err := decodeStoreMocksResp(res)
	if err == nil || errors.Is(err, models.ErrAgentPredatesMockStream) {
		t.Fatalf("decodeStoreMocksResp of an endless text reply = %v; want the plain error", err)
	}
	if body.read > 2*maxStoreMocksFailBody {
		t.Fatalf("decodeStoreMocksResp read %d bytes of an endless reply; want at most %d", body.read, 2*maxStoreMocksFailBody)
	}
}

// Only a 400 is judged: that agent's bytes under any other status, a 4xx among
// them, keep the plain error.
func TestDecodeStoreMocksRespJudgesOnlyA400(t *testing.T) {
	old := preStreamBody(t)
	for _, code := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnsupportedMediaType, http.StatusInternalServerError} {
		res := &http.Response{
			StatusCode: code,
			Header:     http.Header{"Content-Type": {"application/x-gob"}},
			Body:       io.NopCloser(bytes.NewReader(old)),
		}
		err := decodeStoreMocksResp(res)
		if got, want := errors.Is(err, models.ErrAgentPredatesMockStream), code == http.StatusBadRequest; got != want {
			t.Fatalf("decodeStoreMocksResp(%d, that agent's reply) = %v; called too old: %v, want %v", code, err, got, want)
		}
	}
}
