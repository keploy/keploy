package http

import (
	"crypto/sha256"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
)

// servedLiveView is F's view of m, which F served to the request in with m's
// recorded answer whole (servedExchange): m as the application sent in
// (liveRequest) and F answered it, read back where a later call's rule reads
// it (an answer the agent parked on disk read back then, withResponse), and
// the key that names it among m's views (learn.Tracker.ServedMockLive): the
// request as the view holds it, hashed. Two serves of m whose requests the
// view holds the same gave the same view: F gave both the recorded answer.
// What a set keeps of it is the live request, never the answer.
func servedLiveView(m *models.Mock, in *req) (func() (*models.Mock, error), string) {
	lr := liveRequest(m.Spec.HTTPReq, in)
	return func() (*models.Mock, error) {
		rec, err := withResponse(m)
		if err != nil {
			return nil, err
		}
		v := rec.ShallowCopy()
		v.Spec.HTTPReq = lr
		return v, nil
	}, requestKey(lr)
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
