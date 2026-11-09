package proxy

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// bodyWords streams the body of a client test's request keploy does not
// read (takeRequest: one larger than maxTakenBody, one of an encoding a
// recording keeps as its client sent it, a form that sends a file) into the
// words the learn step keeps of it (learn.RequestWords), as the application
// reads it: decoded as its Content-Encoding says (gzip, deflate as zlib or
// raw, br, zstd), a multipart form whole, its file parts too. The body is
// never kept: it passes through a filter of a fixed size. A nil bodyWords
// takes nothing, and gives no words (wordsOf).
type bodyWords struct {
	words *learn.RequestWords
	// pw is where the body goes to be decoded (decodeWords): nil where it
	// goes to words as it is.
	pw *io.PipeWriter
	// ended is closed once the decode ended, err then its end: nil where
	// it read the body to its end or words held any word.
	ended chan struct{}
	err   error
}

// wordDecoders are the decodes of bodies' words that may run at once
// (maxWordDecoders): a br decoder holds up to 16 MiB, a zstd one up to
// zstdWordsWindow and its buffers. A body sent encoded while all are taken
// gives no words.
var wordDecoders = make(chan struct{}, maxWordDecoders)

const (
	maxWordDecoders = 4
	// zstdWordsWindow is the largest window of a zstd body whose words
	// are read: the most HTTP's zstd coding uses (RFC 9659).
	zstdWordsWindow = 8 << 20
)

// newBodyWords is a bodyWords of a body sent with headers h: nil where h
// names an encoding (Content-Encoding) it does not decode, more than one,
// or where no decoder is free (wordDecoders).
func newBodyWords(h http.Header) *bodyWords {
	var open func(io.Reader) (io.ReadCloser, error)
	switch strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding"))) {
	case "", "identity":
		return &bodyWords{words: learn.NewRequestWords()}
	case "gzip", "x-gzip":
		open = func(r io.Reader) (io.ReadCloser, error) { return gzip.NewReader(r) }
	case "deflate":
		open = openDeflate
	case "br":
		open = func(r io.Reader) (io.ReadCloser, error) { return io.NopCloser(brotli.NewReader(r)), nil }
	case "zstd":
		open = openZstd
	default:
		return nil
	}
	select {
	case wordDecoders <- struct{}{}:
	default:
		return nil
	}
	pr, pw := io.Pipe()
	b := &bodyWords{words: learn.NewRequestWords(), pw: pw, ended: make(chan struct{})}
	go b.decodeWords(open, pr)
	return b
}

// errWordsRead ends the pipe of a decode that ended: what is written after
// it is not read.
var errWordsRead = errors.New("the body's words were read")

// decodeWords decodes what comes through pr as open does into b.words,
// until it ends, fails or b.words holds any word, and then frees its
// decoder (wordDecoders) and ends the pipe, so a write after it returns at
// once.
func (b *bodyWords) decodeWords(open func(io.Reader) (io.ReadCloser, error), pr *io.PipeReader) {
	defer close(b.ended)
	defer func() { <-wordDecoders }()
	r, err := open(pr)
	if err == nil {
		buf := make([]byte, 32<<10)
		for err == nil && !b.words.MayHoldAny() {
			var n int
			n, err = r.Read(buf)
			_, _ = b.words.Write(buf[:n])
		}
		if err == io.EOF {
			err = nil
		}
		_ = r.Close()
	}
	b.err = err
	_ = pr.CloseWithError(errWordsRead)
}

// openDeflate is a reader of a deflate body: zlib (RFC 1950), as RFC 9110
// says, or raw deflate (RFC 1951), which some clients send, where it does
// not begin with a zlib header.
func openDeflate(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	if h, err := br.Peek(2); err == nil && h[0]&0x0f == 8 && h[0]>>4 <= 7 && (uint16(h[0])<<8|uint16(h[1]))%31 == 0 {
		return zlib.NewReader(br)
	}
	return flate.NewReader(br), nil
}

// openZstd is a reader of a zstd body of a window of up to zstdWordsWindow,
// one decode at a time.
func openZstd(r io.Reader) (io.ReadCloser, error) {
	d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true), zstd.WithDecoderMaxWindow(zstdWordsWindow))
	if err != nil {
		return nil, err
	}
	return d.IOReadCloser(), nil
}

// write gives b the next piece of the body.
func (b *bodyWords) write(p []byte) {
	switch {
	case b == nil:
	case b.pw != nil:
		_, _ = b.pw.Write(p) // fails only once the decode ended
	default:
		_, _ = b.words.Write(p)
	}
}

// wordsOf ends the body: its words once all of it was read, nil where b is
// nil or its decode failed (a body that does not decode as its encoding
// says may hold any word).
func (b *bodyWords) wordsOf() *learn.RequestWords {
	if b == nil {
		return nil
	}
	if b.pw != nil {
		_ = b.pw.Close()
		<-b.ended
		if b.err != nil {
			return nil
		}
	}
	b.words.End()
	return b.words
}

// abort ends a body that did not come whole: its decode stops.
func (b *bodyWords) abort() {
	if b != nil && b.pw != nil {
		_ = b.pw.CloseWithError(io.ErrUnexpectedEOF)
	}
}

// wordsOfBody are the words of body, a whole body sent with headers h
// (bodyWords): nil where they cannot be read.
func wordsOfBody(h http.Header, body []byte) *learn.RequestWords {
	b := newBodyWords(h)
	b.write(body)
	return b.wordsOf()
}
