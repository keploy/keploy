package http

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// recordedAnswerKey names, among a recording's views
// (learn.Tracker.ServedMockLive), the one every serve that gave its recorded
// answer whole gave: one answer, so one view, whatever request each serve
// answered. answerKey never gives it.
const recordedAnswerKey = "recorded"

// servedLiveView is F's view of m, which F served to the request in with m's
// recorded answer whole (servedExchange, a pairing: reportPairing): m as the
// application sent in and F answered it, read back where a later call's rule
// reads it (an answer the agent parked on disk read back then, withResponse),
// and the key that names it among m's views: what F gave the application,
// m's recorded answer (recordedAnswerKey). Every serve of m that gave it is
// one view, the first one's: a poll the application signs anew each time (a
// date, a signature, an invocation id) gets the same recorded answer on
// every serve, and fills no more of the views a set keeps of m than one.
// What a set keeps of it is the live request, never the answer.
func servedLiveView(m *models.Mock, in *req) (func() (*models.Mock, error), string) {
	load, _ := servedView(m, in, answerView{}, nil)
	return load, recordedAnswerKey
}

// answerView is how F answered a serve of a recording where it did not give
// the recorded answer whole: render makes the answer F gave from the
// recording with its answer read back (nil: the recorded answer).
type answerView struct {
	render func(rec *models.Mock) (*models.Mock, error)
}

// writtenView is av with the answer the caller of the learn step writes
// (req.written) in place of the one av renders (the recorded one where av
// renders none): the view of the answer the application read.
func writtenView(av answerView, written func(*models.Mock) *models.Mock) answerView {
	render := av.render
	return answerView{
		render: func(rec *models.Mock) (*models.Mock, error) {
			if render != nil {
				var err error
				if rec, err = render(rec); err != nil {
					return nil, err
				}
			}
			return written(rec), nil
		},
	}
}

// servedView is F's view of m, which F served to the request in with the
// answer av gives, given (servedLiveView where it gave the recorded one): m
// as the application sent in and F answered it, read back where a later
// call's rule reads it, and the key that names it among m's views: the
// answer F gave the application (answerKey of given), never the request. Two
// serves that gave the same answer are one view, the first one's, whatever
// each request was; two that gave different answers, a learned variable's
// value or a written digest apart, are two. What a set keeps of it is the
// live request and what av renders the answer with, never the answer.
func servedView(m *models.Mock, in *req, av answerView, given *models.HTTPResp) (func() (*models.Mock, error), string) {
	lr := liveRequest(m.Spec.HTTPReq, in)
	return func() (*models.Mock, error) {
		rec, err := withResponse(m)
		if err != nil {
			return nil, err
		}
		if av.render != nil {
			if rec, err = av.render(rec); err != nil {
				return nil, err
			}
		}
		v := rec.ShallowCopy()
		v.Spec.HTTPReq = lr
		return v, nil
	}, answerKey(given)
}

// answerKey names the answer r among the answers F gave of one recording
// (servedView): a hash of what answerAsRecorded compares of two answers,
// its status, protocol version, headers in name order, body and how its
// body was kept, each with its length, so no two answers that differ in any
// of them share it; a hash, it never is recordedAnswerKey. "" for no answer: a
// view keploy cannot name, which a later call reads as one it could not
// read (fail closed).
func answerKey(r *models.HTTPResp) string {
	if r == nil {
		return ""
	}
	h := sha256.New()
	put := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	put(strconv.Itoa(r.StatusCode))
	put(r.StatusMessage)
	put(strconv.Itoa(r.ProtoMajor) + "." + strconv.Itoa(r.ProtoMinor))
	names := make([]string, 0, len(r.Header))
	for k := range r.Header {
		names = append(names, k)
	}
	sort.Strings(names)
	put(strconv.Itoa(len(names)))
	for _, k := range names {
		put(k)
		put(r.Header[k])
	}
	put(r.Body)
	put(r.Binary)
	put(strconv.FormatBool(r.BodySkipped) + "." + strconv.FormatInt(r.BodySize, 10))
	put(strconv.Itoa(len(r.StreamBody)))
	for _, c := range r.StreamBody {
		put(fmt.Sprintf("%d %v", c.TS.UnixNano(), c.Data))
	}
	return string(h.Sum(nil))
}

// liveRequest is the request in as a recording holds one, as record writes
// it (http.go's recorded exchange): its method, URL, headers (a repeated
// header's values joined), body and query parameters, each as the
// application sent it; its protocol version and recorded time are rec's,
// which keploy's wire reading gave the application none of. A request whose
// multipart form record parsed holds the form in its body, whose words the
// view reads, not in form fields.
func liveRequest(rec *models.HTTPReq, in *req) *models.HTTPReq {
	lr := &models.HTTPReq{Method: models.Method(in.method), Header: pkg.ToYamlHTTPHeader(in.header), Body: string(in.body)}
	if in.url != nil {
		lr.URL = in.url.String()
		if q := in.url.Query(); len(q) > 0 {
			lr.URLParams = make(map[string]string, len(q))
			for k, v := range q {
				lr.URLParams[k] = strings.Join(v, ", ")
			}
		}
	}
	if rec != nil {
		lr.ProtoMajor, lr.ProtoMinor, lr.Timestamp = rec.ProtoMajor, rec.ProtoMinor, rec.Timestamp
	}
	return lr
}
