package http

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// servedLiveView is F's view of m, which F served to the request in with m's
// recorded answer whole (servedExchange, a pairing: reportPairing): m as the
// application sent in and F answered it, read back where a later call's rule
// reads it (an answer the agent parked on disk read back then, withResponse),
// and the key that names it among m's views (learn.Tracker.ServedMockLive):
// the request as the view holds it, hashed. Two serves of m whose requests
// the view holds the same gave the same view: F gave both the recorded
// answer. What a set keeps of it is the live request, never the answer.
func servedLiveView(m *models.Mock, in *req) (func() (*models.Mock, error), string) {
	return servedView(m, in, answerView{})
}

// answerView is how F answered a serve of a recording where it did not give
// the recorded answer whole: render makes the answer F gave from the
// recording with its answer read back (nil: the recorded answer), and key
// names it among the answers F gave of that recording, the same for two
// serves render answers the same.
type answerView struct {
	render func(rec *models.Mock) (*models.Mock, error)
	key    string
}

// servedView is F's view of m, which F served to the request in with the
// answer av gives (servedLiveView where it gave the recorded one): m as the
// application sent in and F answered it, read back where a later call's rule
// reads it, and the key that names it among m's views: the request as the
// view holds it and av's key, hashed apart. What a set keeps of it is the
// live request and what av renders the answer with, never the answer.
func servedView(m *models.Mock, in *req, av answerView) (func() (*models.Mock, error), string) {
	lr := liveRequest(m.Spec.HTTPReq, in)
	key := requestKey(lr)
	if av.render != nil {
		h := sha256.New()
		h.Write([]byte(key))
		h.Write([]byte{1})
		h.Write([]byte(av.key))
		key = string(h.Sum(nil))
	}
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
	}, key
}

// valuesKey names the values a learned answer was rendered with
// (learnedView): each variable's name, value and recorded value in name
// order, each with its length, so no two sets of values that differ share it.
func valuesKey(values map[string]variables.Binding) string {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, k := range names {
		for _, w := range []string{k, values[k].Value, values[k].Recorded} {
			b.WriteString(strconv.Itoa(len(w)))
			b.WriteByte(0)
			b.WriteString(w)
		}
	}
	return b.String()
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

// requestKey names a live request among a recording's views (servedLiveView): a
// hash of its method, URL, how many headers it has and each in name order,
// and its body, each with its length, so no two requests that differ in any
// of them share it.
func requestKey(r *models.HTTPReq) string {
	h := sha256.New()
	put := func(s string) {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{0})
		h.Write([]byte(s))
	}
	put(string(r.Method))
	put(r.URL)
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
	return string(h.Sum(nil))
}
