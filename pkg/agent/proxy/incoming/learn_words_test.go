package proxy

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// presentWord is a word the bodies of these rows hold, absentWord one they
// do not.
const presentWord, absentWord = "zq5presentword", "zq5absentword"

// holdsWords fails t unless words are the words of a body that holds
// presentWord and not absentWord: a later call whose value changed to the
// first may have the request as its source, one that changed to the second
// has not.
func holdsWords(t *testing.T, words *learn.RequestWords) {
	t.Helper()
	if words == nil {
		t.Fatal("no words of the body keploy could not read")
	}
	if !words.MayHoldChange("tok-1a2b3c4d5e6f", "tok-"+presentWord, false) {
		t.Errorf("the words lack %q, which the body holds", presentWord)
	}
	if words.MayHoldChange("tok-1a2b3c4d5e6f", "tok-"+absentWord, false) {
		t.Errorf("the words may hold %q, which the body lacks", absentWord)
	}
}

// bigBody is a body of more than 2 MB: a JSON head that holds presentWord,
// which an encoder codes (it is no literal of the encoded body), and then
// seeded random bytes, which no encoder makes much smaller.
func bigBody() []byte {
	var b bytes.Buffer
	b.WriteString(`{"rows":[`)
	for i := 0; b.Len() < 64<<10; i++ {
		fmt.Fprintf(&b, `{"row":%d,"kind":"upload","word":"rowvalue"},`, i)
		if i == 500 {
			b.WriteString(`{"row":"` + presentWord + `"},`)
		}
	}
	b.WriteString(`""],"file":"`)
	rnd := make([]byte, 2<<20)
	_, _ = rand.New(rand.NewSource(7)).Read(rnd)
	b.Write(rnd)
	b.WriteString(`"}`)
	return b.Bytes()
}

// smallBody is a body within what keploy reads that holds presentWord.
func smallBody() []byte {
	return []byte(strings.Repeat(`{"kind":"upload","word":"rowvalue"},`, 40) + `{"name":"` + presentWord + `"}`)
}

// encode is body sent with Content-Encoding enc.
func encode(t *testing.T, enc string, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	var w io.WriteCloser
	switch enc {
	case "gzip", "x-gzip":
		w = gzip.NewWriter(&b)
	case "zlib":
		w = zlib.NewWriter(&b)
	case "raw deflate":
		w, _ = flate.NewWriter(&b, flate.DefaultCompression)
	case "br":
		w = brotli.NewWriter(&b)
	case "zstd":
		zw, err := zstd.NewWriter(&b)
		if err != nil {
			t.Fatal(err)
		}
		w = zw
	default:
		t.Fatalf("no encoder %q", enc)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b.Bytes(), []byte(presentWord)) {
		t.Fatalf("the %s body holds %q as a literal: the row cannot tell a decode from none", enc, presentWord)
	}
	return b.Bytes()
}

// header is the Content-Encoding a row's encoder sends.
func header(enc string) string {
	switch enc {
	case "zlib", "raw deflate":
		return "deflate"
	}
	return enc
}

// A client test's request whose body keploy does not read (larger than it
// reads, sent in an encoding a recording keeps as its client sent it, a
// form that sends a file, one that decodes larger than keploy reads) is
// taken as unread with the words of its body as the application reads it:
// decoded as its Content-Encoding says (gzip, x-gzip, deflate as zlib or
// raw, br, zstd), streamed as it passes where it is larger than keploy
// reads (of a declared length or chunked), never kept whole. The forward
// reads the body as its client sent it.
func TestTakeRequestKeepsTheWordsOfABodyItCannotRead(t *testing.T) {
	big, small := bigBody(), smallBody()
	var bomb bytes.Buffer
	zw := gzip.NewWriter(&bomb)
	for i := 0; i < 64; i++ {
		_, _ = zw.Write([]byte(strings.Repeat(`{"word":"rowvalue"},`, 2<<10)))
		if i == 32 {
			_, _ = zw.Write([]byte(presentWord))
		}
	}
	_ = zw.Close()
	form := []byte("--XX\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\napp-1\r\n" +
		"--XX\r\nContent-Disposition: form-data; name=\"f\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\n" + string(small) + "\r\n--XX--\r\n")
	type row struct {
		name, headers string
		sent          []byte
		chunked       bool
		why           string
	}
	padded := func(n int) []byte { return []byte(fmt.Sprintf("%-*s", n, `{"name":"`+presentWord+`"}`)) }
	rows := []row{
		{"2 MB as sent", "", big, false, "larger than"},
		{"2 MB chunked", "", big, true, "larger than"},
		{"2 MB identity", "Content-Encoding: identity\r\n", big, false, "larger than"},
		{"deflate, zlib", "Content-Encoding: deflate\r\n", encode(t, "zlib", small), false, "Content-Encoding deflate)"},
		{"deflate, zlib, chunked", "Content-Encoding: deflate\r\n", encode(t, "zlib", small), true, "Content-Encoding deflate)"},
		// Raw deflate whose first two bytes are each a zlib header's but
		// one: its method (0x00), its window (0x88), its check (0x08).
		{"deflate, raw, no zlib method", "Content-Encoding: deflate\r\n", storedDeflate(0x00, padded(62)), false, "Content-Encoding deflate)"},
		{"deflate, raw, no zlib window", "Content-Encoding: deflate\r\n", storedDeflate(0x88, padded(59)), false, "Content-Encoding deflate)"},
		{"deflate, raw, no zlib check", "Content-Encoding: deflate\r\n", storedDeflate(0x08, padded(40)), false, "Content-Encoding deflate)"},
		{"deflate, raw", "Content-Encoding: deflate\r\n", encode(t, "raw deflate", small), false, "Content-Encoding deflate)"},
		{"zstd", "Content-Encoding: zstd\r\n", encode(t, "zstd", small), false, "Content-Encoding zstd)"},
		{"x-gzip", "Content-Encoding: x-gzip\r\n", encode(t, "x-gzip", small), false, "Content-Encoding x-gzip)"},
		{"gzip larger than keploy reads once decoded", "Content-Encoding: gzip\r\n", bomb.Bytes(), false, "decoded, its body is larger than"},
		{"a form that sends a file", "Content-Type: multipart/form-data; boundary=XX\r\n", form, false, "form part f sends a file"},
	}
	for _, enc := range []string{"gzip", "zlib", "raw deflate", "br", "zstd"} {
		sent := encode(t, enc, big)
		if len(sent) <= maxTakenBody {
			t.Fatalf("the %s body is %d bytes, not larger than keploy reads", enc, len(sent))
		}
		rows = append(rows, row{"2 MB " + enc, "Content-Encoding: " + header(enc) + "\r\n", sent, false, "larger than"})
		rows = append(rows, row{"2 MB " + enc + " chunked", "Content-Encoding: " + header(enc) + "\r\n", sent, true, "larger than"})
	}
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			log := &requestLog{}
			log.wants.Store(true)
			pm := &IngressProxyManager{logger: zap.NewNop()}
			pm.SetRequestTaker(log)
			raw := postWith(c.headers, string(c.sent))
			if c.chunked {
				raw = fmt.Sprintf("POST /u HTTP/1.1\r\nHost: api\r\n%sTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n0\r\n\r\n", c.headers, len(c.sent), c.sent)
			}
			req := readRequest(t, raw)
			pm.takeRequest(req, time.Now())
			head := make([]byte, min(64<<10, len(c.sent)))
			if _, err := io.ReadFull(req.Body, head); err != nil {
				t.Fatal(err)
			}
			if cb, ok := req.Body.(*comingBody); ok && !c.chunked && cb.got != nil {
				t.Fatal("a body declared larger than keploy reads is held as it passes")
			}
			rest, err := io.ReadAll(pieces{req.Body})
			fwd := append(head, rest...)
			if err != nil || !bytes.Equal(fwd, c.sent) {
				t.Fatalf("the forward read %d bytes (%v), want the %d its client sent", len(fwd), err, len(c.sent))
			}
			got := log.list()
			coming := c.chunked || len(c.sent) > maxTakenBody
			if len(got) != 1 || !strings.Contains(got[0].unread, c.why) || got[0].coming || (got[0].dones == 1) != coming {
				t.Fatalf("taken %+v, want one unread for %q, read as it passed: %v", got, c.why, coming)
			}
			holdsWords(t, got[0].words)
		})
	}
}

// A body keploy reads gives no words, and one keploy could not read gives
// none where they cannot be read: an encoding keploy does not decode, two
// encodings, a body that does not decode as its encoding says, one its
// client did not send whole or that was closed before its end, and one sent
// encoded while every decoder is taken (wordDecoders), which a body sent as
// it is needs none of. A decode ends, freeing its decoder, once the words
// may hold any word, though the body goes on.
func TestTakeRequestGivesNoWordsWhereItCannotReadThem(t *testing.T) {
	small := smallBody()
	for _, c := range []struct{ name, headers, sent, why string }{
		{"read", "Content-Type: application/json\r\n", string(small), ""},
		{"gzip read", "Content-Encoding: gzip\r\n", string(encode(t, "gzip", small)), ""},
		{"an encoding keploy does not decode", "Content-Encoding: compress\r\n", string(small), "Content-Encoding compress)"},
		{"two encodings", "Content-Encoding: gzip, br\r\n", string(encode(t, "gzip", small)), "Content-Encoding gzip, br)"},
		{"gzip that does not decode", "Content-Encoding: gzip\r\n", string(small), "does not decode as its Content-Encoding gzip says"},
		{"deflate that does not decode", "Content-Encoding: deflate\r\n", string(encode(t, "zlib", small)[:20]), "Content-Encoding deflate)"},
		{"raw deflate that does not decode", "Content-Encoding: deflate\r\n", string(encode(t, "raw deflate", small)[:20]), "Content-Encoding deflate)"},
		{"zstd that does not decode", "Content-Encoding: zstd\r\n", string(encode(t, "zstd", small)[:20]), "Content-Encoding zstd)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			log := &requestLog{}
			log.wants.Store(true)
			pm := &IngressProxyManager{logger: zap.NewNop()}
			pm.SetRequestTaker(log)
			pm.takeRequest(readRequest(t, postWith(c.headers, c.sent)), time.Now())
			got := log.list()
			if len(got) != 1 || !strings.Contains(got[0].unread, c.why) || c.why == "" && got[0].unread != "" || got[0].words != nil {
				t.Fatalf("taken %+v, want unread %q and no words", got, c.why)
			}
		})
	}
	big := bigBody()
	cut := func(t *testing.T, headers string, sent []byte, closeFirst bool) {
		t.Helper()
		log := &requestLog{}
		log.wants.Store(true)
		pm := &IngressProxyManager{logger: zap.NewNop()}
		pm.SetRequestTaker(log)
		req := readRequest(t, fmt.Sprintf("POST /u HTTP/1.1\r\nHost: api\r\n%sContent-Length: %d\r\n\r\n%s", headers, len(sent)+10, sent))
		pm.takeRequest(req, time.Now())
		if closeFirst {
			_, _ = io.ReadFull(req.Body, make([]byte, maxTakenBody+10))
			_ = req.Body.Close()
		} else {
			_, _ = io.ReadAll(req.Body)
		}
		got := log.list()
		if len(got) != 1 || got[0].dones != 1 || !strings.Contains(got[0].unread, "whole") || got[0].words != nil {
			t.Fatalf("taken %+v, want one unread, not whole, with no words", got)
		}
		freed(t)
	}
	t.Run("not sent whole", func(t *testing.T) { cut(t, "", big, false) })
	t.Run("gzip not sent whole", func(t *testing.T) { cut(t, "Content-Encoding: gzip\r\n", encode(t, "gzip", big), false) })
	t.Run("gzip closed before its end", func(t *testing.T) { cut(t, "Content-Encoding: gzip\r\n", encode(t, "gzip", big), true) })
	t.Run("every decoder taken", func(t *testing.T) {
		freed(t)
		for i := 0; i < cap(wordDecoders); i++ {
			wordDecoders <- struct{}{}
		}
		defer func() {
			for i := 0; i < cap(wordDecoders); i++ {
				<-wordDecoders
			}
		}()
		for _, c := range []struct {
			headers string
			sent    []byte
			words   bool
		}{
			{"Content-Encoding: zstd\r\n", encode(t, "zstd", small), false},
			{"Content-Encoding: gzip\r\n", encode(t, "gzip", big), false},
			{"", big, true},
		} {
			log := &requestLog{}
			log.wants.Store(true)
			pm := &IngressProxyManager{logger: zap.NewNop()}
			pm.SetRequestTaker(log)
			req := readRequest(t, postWith(c.headers, string(c.sent)))
			pm.takeRequest(req, time.Now())
			_, _ = io.ReadAll(req.Body)
			got := log.list()
			if len(got) != 1 || got[0].unread == "" || (got[0].words != nil) != c.words {
				t.Fatalf("%q: taken %+v, want words %v", c.headers, got, c.words)
			}
		}
	})
	t.Run("a decode that may hold any word ends", func(t *testing.T) {
		var bomb bytes.Buffer
		zw := gzip.NewWriter(&bomb)
		zeros := make([]byte, 1<<20)
		for i := 0; i < 80; i++ {
			_, _ = zw.Write(zeros)
		}
		_ = zw.Close()
		log := &requestLog{}
		log.wants.Store(true)
		pm := &IngressProxyManager{logger: zap.NewNop()}
		pm.SetRequestTaker(log)
		pr, pw := io.Pipe()
		req := readRequest(t, fmt.Sprintf("POST /u HTTP/1.1\r\nHost: api\r\nContent-Encoding: gzip\r\nContent-Length: %d\r\n\r\n", 2<<20))
		req.Body = pr
		t.Cleanup(func() { _ = pw.CloseWithError(io.ErrUnexpectedEOF) })
		taken := make(chan struct{})
		go func() { pm.takeRequest(req, time.Now()); close(taken) }()
		select {
		case <-taken:
		case <-time.After(10 * time.Second):
			t.Fatal("the ingress waits to read a body declared larger than it reads before it forwards any of it")
		}
		go func() { _, _ = io.Copy(io.Discard, req.Body) }()
		if _, err := pw.Write(bomb.Bytes()); err != nil {
			t.Fatal(err)
		}
		freed(t)
	})
}

// storedDeflate is data as raw deflate (RFC 1951): one stored block, not
// the last, whose header byte is first (BFINAL and BTYPE 0, its padding
// bits as first sets them), then an empty last one. Its first two bytes
// are first and the low byte of len(data).
func storedDeflate(first byte, data []byte) []byte {
	n := len(data)
	out := append([]byte{first, byte(n), byte(n >> 8), ^byte(n), ^byte(n >> 8)}, data...)
	return append(out, 0x01, 0x00, 0x00, 0xff, 0xff)
}

// pieces reads its reader in pieces of 4093 bytes at most, as a forward
// reads a body.
type pieces struct{ io.Reader }

func (r pieces) Read(p []byte) (int, error) {
	if len(p) > 4093 {
		p = p[:4093]
	}
	return r.Reader.Read(p)
}

// freed fails t unless every decoder is free (wordDecoders) within a few
// seconds: no decode is left running.
func freed(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); len(wordDecoders) > 0; {
		if time.Now().After(end) {
			t.Fatalf("%d decodes still run", len(wordDecoders))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A client test's request of 2 MB passes through the ingress, in both of
// its HTTP/1 loops, to the application whole, and the learn step is given
// its words once its body ended, before the application has it whole.
func TestTheIngressGivesTheWordsOfALargeClientRequestBeforeTheAppHasItWhole(t *testing.T) {
	stubIngressPaused(t, func() bool { return false })
	body := bigBody()
	for _, mode := range []string{"sync", "zero-copy"} {
		t.Run(mode, func(t *testing.T) {
			log := &requestLog{}
			log.wants.Store(true)
			wordsFirst := make(chan *learn.RequestWords, 1)
			gotBody := make(chan []byte, 1)
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer func() { _ = c.Close() }()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				b := make([]byte, len(body))
				if _, err := io.ReadFull(req.Body, b); err != nil {
					return
				}
				var w *learn.RequestWords
				if got := log.list(); len(got) == 1 && got[0].dones == 1 {
					w = got[0].words
				}
				wordsFirst <- w
				gotBody <- b
				_, _ = io.WriteString(c, "HTTP/1.1 201 Created\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
			}()
			pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: mode == "sync"}
			pm.SetRequestTaker(log)
			ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), syncMock.New(zap.NewNop())), 20*time.Second)
			defer cancel()
			client, done := ingressPipe(t, ctx, pm, ln.Addr().String())
			go func() {
				for range pm.tcChan {
				}
			}()
			go func() {
				_, _ = fmt.Fprintf(client, "POST /upload HTTP/1.1\r\nHost: api\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", len(body))
				_, _ = client.Write(body)
			}()
			resp, err := http.ReadResponse(bufio.NewReader(client), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			holdsWords(t, <-wordsFirst)
			if got := <-gotBody; !bytes.Equal(got, body) {
				t.Fatalf("the application got %d bytes, want the %d sent", len(got), len(body))
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("the ingress did not end the exchange")
			}
		})
	}
}
