package proxy

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"go.keploy.io/server/v3/pkg"
	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/models"
)

// RequestTaker is what the ingress gives each request of a client test to
// before the application is given any byte of it: the agent's learn step
// (proxy.Proxy; learn.Tracker.Incoming), which reads the request as an
// earlier place of the calls it leads to. WantsIncoming reports whether it
// takes requests now (a client test is open whose recording sent requests
// not yet paired); TakeIncoming takes one that reached the ingress at the
// real instant at, unread saying why keploy could not read it ("" where it
// could).
type RequestTaker interface {
	WantsIncoming() bool
	TakeIncoming(req *models.HTTPReq, unread string, at time.Time)
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
// and its body, which takeRequest reads whole first, of up to maxTakenBody
// bytes, and puts back for the forward. A body keploy does not read so (one
// its client sends only after the application answers 100, one of no
// declared length or larger than maxTakenBody, one encoded) is given as
// unread, so the learn step reads nothing from the request rather than part
// of it. A body the client did not send whole (it left, or declared more than
// it sent) gives nothing, and the forward meets the same end as it would
// have without the read.
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
	unread := ""
	switch {
	case req.Body == nil || req.Body == http.NoBody || req.ContentLength == 0:
	case expectsContinue(req):
		unread = "its client sent its body only once the application answered 100 (Continue), and keploy reads no body after the application was given the request"
	case req.ContentLength < 0:
		unread = "its body declares no length, and keploy reads no body as it streams to the application"
	case req.ContentLength > maxTakenBody:
		unread = fmt.Sprintf("its body is larger than the %d bytes keploy reads before the application is given the request", maxTakenBody)
	case req.Header.Get("Content-Encoding") != "":
		unread = "its body is encoded (Content-Encoding " + req.Header.Get("Content-Encoding") + "), which keploy does not read for a client test's request"
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
		live.Body = string(body)
		if req.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
			if decoded, derr := url.QueryUnescape(live.Body); derr == nil {
				live.Body = decoded
			}
		}
	}
	t.TakeIncoming(live, unread, at)
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
