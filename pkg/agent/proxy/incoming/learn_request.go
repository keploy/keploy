package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// RequestTaker is what the ingress gives each request of a client test to
// before the application is given any byte of it: the agent's learn step
// (proxy.Proxy; learn.Tracker.Incoming), which reads the request as an
// earlier place of the calls it leads to. WantsIncoming reports whether it
// takes requests now (a client test is open whose recording sent requests
// not yet paired); TakeIncoming takes one that reached the ingress at the
// real instant at, unread saying why keploy could not read it ("" where it
// could) and words, where not nil, the words of the body it could not read
// (bodyWords). TakeComing takes one whose body is still coming
// (comingBody): the place is one keploy could not read until done gives the
// request with its body, once the body ended, unread and words as
// TakeIncoming's; done is nil where the taker takes nothing of it.
type RequestTaker interface {
	WantsIncoming() bool
	TakeIncoming(req *models.HTTPReq, unread string, words *learn.RequestWords, at time.Time)
	TakeComing(req *models.HTTPReq, at time.Time) (done func(req *models.HTTPReq, unread string, words *learn.RequestWords))
}

// SetRequestTaker makes the ingress give t each HTTP/1 request it forwards
// while t wants them (takeRequest). Set before the ingress starts; nil gives
// none.
func (pm *IngressProxyManager) SetRequestTaker(t RequestTaker) { pm.requestTaker = t }

// RequestTaker is what the ingress gives the requests it forwards to
// (SetRequestTaker): nil where none.
func (pm *IngressProxyManager) RequestTaker() RequestTaker { return pm.requestTaker }

// maxTakenBody is the largest request body the ingress reads before the
// application is given it, for the learn step: a recording keeps a larger
// one apart from its test case (hooksUtils.LargeBodyThreshold), which the
// learn step does not read either.
const maxTakenBody = hooksUtils.LargeBodyThreshold

// takeRequest gives the request taker req, which reached the ingress at at,
// where it wants it, before req is forwarded: no call the request leads the
// application to make can read the test's places without it. The request is
// read as a recording reads it (hooksUtils.Capture): its URL, its headers
// and its body (readTakenBody), of up to maxTakenBody bytes. A body of a
// declared length of up to maxTakenBody its client sends at once is read
// whole first and put back for the forward. One its client sends only after
// the application answers 100 (Continue), one of no declared length
// (chunked), and one of a declared length larger than maxTakenBody, are read
// as they pass to the application (comingBody): until they end the request
// is a place keploy could not read (TakeComing). A body larger than
// maxTakenBody, and one readTakenBody cannot read, is given as unread, so
// the learn step reads nothing from the request rather than part of it, with
// its words (bodyWords) where it came whole. A body of a declared length its
// client did not send whole (it left, or declared more than it sent) gives
// nothing, and the forward meets the same end as it would have without the
// read.
func (pm *IngressProxyManager) takeRequest(req *http.Request, at time.Time) {
	t := pm.requestTaker
	if t == nil || !t.WantsIncoming() {
		return
	}
	live := &models.HTTPReq{
		Method:     models.Method(req.Method),
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		URL:        fmt.Sprintf("http://%s%s", req.Host, req.URL.RequestURI()),
		Header:     pkg.ToYamlHTTPHeader(req.Header),
		URLParams:  pkg.URLParams(req),
		Timestamp:  at,
	}
	switch {
	case req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0:
		t.TakeIncoming(live, "", nil, at)
	case expectsContinue(req) || req.ContentLength < 0 || req.ContentLength > maxTakenBody:
		if done := t.TakeComing(live, at); done != nil {
			b := &comingBody{ReadCloser: req.Body, live: live, header: req.Header, left: req.ContentLength, done: done}
			if req.ContentLength > maxTakenBody {
				b.over, b.words = true, newBodyWords(req.Header)
			}
			req.Body = b
		}
	default:
		body, err := io.ReadAll(io.LimitReader(req.Body, req.ContentLength))
		rest := io.Reader(bytes.NewReader(body))
		if err == nil && int64(len(body)) < req.ContentLength {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			rest = io.MultiReader(rest, errReader{err})
		}
		req.Body = readCloser{Reader: rest, Closer: req.Body}
		if err != nil {
			return
		}
		why := readTakenBody(live, req.Header, body)
		t.TakeIncoming(live, why, unreadWords(why, req.Header, body), at)
	}
}

// unreadWords are the words of body, the whole body of a request with
// headers h that keploy could not read (why; readTakenBody), as the
// application reads it (bodyWords): nil where keploy read it.
func unreadWords(why string, h http.Header, body []byte) *learn.RequestWords {
	if why == "" {
		return nil
	}
	return wordsOfBody(h, body)
}

// tooLarge is why keploy did not read a request body larger than
// maxTakenBody.
var tooLarge = fmt.Sprintf("its body is larger than the %d bytes keploy reads of a client test's request", maxTakenBody)

// readTakenBody reads body, the whole body of a request with headers h,
// into live as a recording reads it (hooksUtils.Capture), and is why keploy
// could not read it ("" where it could): a body sent gzip or br
// (Content-Encoding) is decoded (pkg.Decompress), one sent identity is its
// text as sent, one of another encoding is not read, as a recording keeps
// it as its client sent it; a multipart form is
// taken apart into its text parts (live.Form), its Content-Type kept
// without its boundary, and a file part in it is not read; a form sent
// URL-encoded is unescaped; any other body is its text.
func readTakenBody(live *models.HTTPReq, h http.Header, body []byte) string {
	if enc := strings.TrimSpace(h.Get("Content-Encoding")); enc != "" {
		switch strings.ToLower(enc) {
		case "gzip", "br":
			decoded, err := pkg.Decompress(zap.NewNop(), enc, body, maxTakenBody)
			switch {
			case errors.Is(err, pkg.ErrDecompressedTooLarge):
				return "decoded, " + tooLarge
			case err != nil:
				return "its body does not decode as its Content-Encoding " + enc + " says: " + err.Error()
			}
			body = decoded
		case "identity":
		default:
			return "its body is encoded (Content-Encoding " + enc + "), which a recording keeps as its client sent it, undecoded"
		}
	}
	ct := h.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		form, why := formTextParts(ct, body)
		if why != "" {
			return why
		}
		live.Form = form
		live.Header["Content-Type"] = strings.TrimSpace(strings.Split(ct, ";")[0])
	case ct == "application/x-www-form-urlencoded":
		live.Body = string(body)
		if decoded, err := url.QueryUnescape(live.Body); err == nil {
			live.Body = decoded
		}
	default:
		live.Body = string(body)
	}
	return ""
}

// formTextParts are the parts of body, a multipart form of Content-Type ct,
// in the order sent, each its name's value (hooksUtils.ExtractFormData), or
// why keploy does not read them: a part that sends a file (a file name), and
// a form that does not parse.
func formTextParts(ct string, body []byte) ([]models.FormData, string) {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil || params["boundary"] == "" {
		return nil, "its multipart form names no boundary keploy can read"
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var form []models.FormData
	for {
		part, err := r.NextPart()
		if err == io.EOF {
			return form, ""
		}
		if err != nil {
			return nil, "its multipart form does not parse: " + err.Error()
		}
		if part.FormName() == "" {
			continue
		}
		if part.FileName() != "" {
			return nil, "its form part " + part.FormName() + " sends a file, which keploy does not read for a client test's request"
		}
		v, err := io.ReadAll(part)
		if err != nil {
			return nil, "its multipart form does not parse: " + err.Error()
		}
		form = append(form, models.FormData{Key: part.FormName(), Values: []string{string(v)}})
	}
}

// comingBody is a request body the ingress gives the application as it
// comes (takeRequest), read as it passes for the learn step: up to
// maxTakenBody bytes of it, given to done (readTakenBody) once the body
// ended, and the words of one larger (bodyWords, from its first byte
// where its declared length is larger), or one keploy cannot read. Where its length is declared (left), done is given it before the
// read that brings its last byte returns, so before the application has
// it; where not (chunked), as the read meets its end, before the forward
// sends its last chunk, which the application waits for to know the body
// ended. So no call the application makes once it has the body whole reads
// the request as still coming. One that does not end whole (an error, a
// close first, fewer bytes than declared) or is larger than maxTakenBody
// is given done as unread.
type comingBody struct {
	io.ReadCloser
	live   *models.HTTPReq
	header http.Header
	// left is how many bytes the body declares still to come; < 0 where it
	// declares no length.
	left int64
	done func(*models.HTTPReq, string, *learn.RequestWords)

	mu   sync.Mutex
	got  []byte
	over bool
	// words, once over, take the body's bytes as they pass (nil where
	// they take none: newBodyWords).
	words *bodyWords
	ended bool
}

func (b *comingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if b.ended {
		b.mu.Unlock()
		return n, err
	}
	if !b.over && len(b.got)+n > maxTakenBody {
		b.over, b.words = true, newBodyWords(b.header)
		b.words.write(b.got)
		b.got = nil
	}
	if b.over {
		b.words.write(p[:n])
	} else {
		b.got = append(b.got, p[:n]...)
	}
	if b.left >= 0 {
		b.left -= int64(n)
	}
	why, end := "", true
	switch {
	case b.left == 0 || b.left < 0 && err == io.EOF:
	case err == io.EOF:
		why = "its client did not send the whole body it declared"
	case err != nil:
		why = "its body did not come whole: " + err.Error()
	default:
		end = false
	}
	b.ended = end
	got, over, words := b.got, b.over, b.words
	b.mu.Unlock()
	if end {
		var kept *learn.RequestWords
		switch {
		case why != "":
			words.abort()
		case over:
			why, kept = tooLarge, words.wordsOf()
		default:
			why = readTakenBody(b.live, b.header, got)
			kept = unreadWords(why, b.header, got)
		}
		b.done(b.live, why, kept)
	}
	return n, err
}

// Close closes the body: one that had not ended is given done as unread,
// with no words.
func (b *comingBody) Close() error {
	b.mu.Lock()
	ended, words := b.ended, b.words
	b.ended = true
	b.mu.Unlock()
	if !ended {
		words.abort()
		b.done(b.live, "its body did not come whole: it was closed before its end", nil)
	}
	return b.ReadCloser.Close()
}

// readCloser reads from Reader and closes Closer: a body read ahead, put
// back with its own Close.
type readCloser struct {
	io.Reader
	io.Closer
}

// errReader is a read that fails with err: the end a body read ahead met,
// met again by the forward that reads it back.
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }
