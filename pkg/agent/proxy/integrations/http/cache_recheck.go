package http

import (
	"net/http"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// A browser keeps an answer in its HTTP cache for as long as the answer says
// (Cache-Control max-age, Expires, against its Date), and once that time is
// over it asks the server again whether the copy it holds is still current: a
// conditional GET carrying the copy's validators, If-None-Match with its ETag
// and If-Modified-Since with its Last-Modified (RFC 9111 §4.3.1). A replay
// serves an answer with the headers it was recorded with, its Date included,
// so a copy keploy serves may be stale on arrival: a fetch the record's
// browser made unconditionally, or answered from a fresh copy, is made
// conditionally at replay, by timing alone. js.stripe.com's
// /v3/.deploy_status_henson.json (max-age=60), fetched twice a few
// milliseconds apart, is one: its second fetch carries validators at replay
// that no recording holds.
//
// So, as a CORS preflight is (cors_preflight.go), under the proven switch a
// conditional re-check of an answer keploy already gave the open test is read
// as a browser cache step, not as the test's data: a GET whose validators
// are those of an answer the test got for the same call (cacheRecheckOf) is
// decided as the fetch it re-checks, without them. A recording of the test
// that the fetch is still owed is served to it as for any fetch (the record's
// browser made it unconditionally); where none is left, it is answered again
// with the earlier answer (cacheRecheck.servedBy), as a server that ignores
// the conditions answers it (RFC 9110 §13.2.1). It is never a call more than
// the test recorded, and nothing is required of it. A re-check the recording
// itself holds (a recorded call of the test carries validators) stays the
// test's data, matched as recorded. A conditional GET whose validators are of
// no answer the test got this run is no re-check of one: it is matched as it
// came, and misses where its recording holds no validators.

// recheckValidators are the request headers a browser carries a cached copy's
// validators in when it re-checks the copy, each with the answer header it
// names.
var recheckValidators = []struct{ request, answer string }{
	{"If-None-Match", "ETag"},
	{"If-Modified-Since", "Last-Modified"},
}

// recheckRefusers are request headers that make a GET more than a re-check of
// a cached copy: a precondition of another kind, or a request for a part.
var recheckRefusers = []string{"If-Match", "If-Unmodified-Since", "If-Range", "Range"}

// cacheRecheck is a conditional GET read as a re-check of an answer the open
// test already got (cacheRecheckOf); the zero value is no re-check.
type cacheRecheck struct {
	// earlier is the answered recording whose validators the re-check
	// carries, nil for no re-check; validators are the request headers it
	// carries them in, and values their values, by those names.
	earlier    *models.Mock
	validators []string
	values     map[string]string
}

// cacheRecheckOf returns, for a request under the proven switch, the request
// as the fetch it re-checks and the re-check, when it is a conditional GET
// re-checking an answer the open test already got; otherwise input itself
// and no re-check.
//
// It is one when: it is a GET carrying If-None-Match, If-Modified-Since or
// both, each once and not empty, and no other precondition or range; one of
// the test's answered recordings (integrations.ConsumedInCallView, read of
// db: none where the call's view moved) answered 200 with exactly those
// validators (each carried one equal to the answer's ETag or Last-Modified,
// the one the browser holds) and is, as the matcher judges it (same), the
// same call as the request without them; and no recording of the test that
// is still unanswered, of the request's method and path, carries a validator
// (conditionalOwed): the test recorded conditional fetches of the call, which
// are its data.
//
// The request returned is a copy of input without the validators (its header
// cloned); input is left as it came.
func cacheRecheckOf(input *req, db integrations.MockMemDb, same func(stripped *req, m *models.Mock) bool) (*req, cacheRecheck) {
	if !provenswitch.On() || input == nil || input.method != http.MethodGet || input.url == nil {
		return input, cacheRecheck{}
	}
	for _, h := range recheckRefusers {
		if len(input.header.Values(h)) > 0 {
			return input, cacheRecheck{}
		}
	}
	rc := cacheRecheck{values: map[string]string{}}
	for _, v := range recheckValidators {
		got := input.header.Values(v.request)
		if len(got) == 0 {
			continue
		}
		if len(got) > 1 || strings.TrimSpace(got[0]) == "" {
			return input, cacheRecheck{}
		}
		rc.values[v.request] = strings.TrimSpace(got[0])
		rc.validators = append(rc.validators, v.request)
	}
	if len(rc.validators) == 0 || conditionalOwed(input, db) {
		return input, cacheRecheck{}
	}
	stripped := *input
	stripped.header = input.header.Clone()
	for _, n := range rc.validators {
		stripped.header.Del(n)
	}
	consumed, _ := integrations.ConsumedInCallView(db)
	answered := FilterHTTPMocks(consumed)
	for i := len(answered) - 1; i >= 0; i-- {
		if a := answered[i]; rc.names(a) && same(&stripped, a) {
			rc.earlier = a
			return &stripped, rc
		}
	}
	return input, cacheRecheck{}
}

// names reports whether m answered 200 with exactly the validators the
// re-check carries: each one the answer's header it names. A response keploy
// kept apart is read back first; one it cannot read back names none.
func (rc cacheRecheck) names(m *models.Mock) bool {
	served, err := withResponse(m)
	if err != nil {
		return false
	}
	if served == nil || served.Spec.HTTPResp == nil || served.Spec.HTTPResp.StatusCode != http.StatusOK {
		return false
	}
	for _, v := range recheckValidators {
		want, carried := rc.values[v.request]
		if !carried {
			continue
		}
		if strings.TrimSpace(headerValue(served.Spec.HTTPResp.Header, v.answer)) != want {
			return false
		}
	}
	return true
}

// servedBy is, of the open test's answered recordings the request without its
// validators matches each on its own (extraCallOf's matched: read of the
// call's hold), the latest whose answer the re-check names, to answer it
// again with; nil for no re-check, and when none is: the call's view moved,
// or the answer it re-checks is no longer among them.
func (rc cacheRecheck) servedBy(matched []*models.Mock) *models.Mock {
	if rc.earlier == nil {
		return nil
	}
	for i := len(matched) - 1; i >= 0; i-- {
		if rc.names(matched[i]) {
			return matched[i]
		}
	}
	return nil
}

// conditionalOwed reports whether a recording of the open test the store
// still holds (one answered is gone from it, but for one kept to answer its
// repeats, which answers the request as it came), of input's method and URL
// path, carries a validator: the test recorded a conditional fetch of the
// call, its data. A store whose per-test read fails counts as holding one:
// matched as it came is the side that hides nothing.
func conditionalOwed(input *req, db integrations.MockMemDb) bool {
	perTest, err := db.GetPerTestMocksInWindow()
	if err != nil {
		return true
	}
	owned := openTestOwner(db, perTest)
	for _, m := range FilterHTTPMocks(perTest) {
		if m == nil || m.Spec.HTTPReq == nil || !owned(m) || string(m.Spec.HTTPReq.Method) != input.method {
			continue
		}
		if p, ok := recordedURLPath(m.Spec.HTTPReq.URL); !ok || p != input.url.Path {
			continue
		}
		for _, v := range recheckValidators {
			if headerValue(m.Spec.HTTPReq.Header, v.request) != "" {
				return true
			}
		}
	}
	return false
}
