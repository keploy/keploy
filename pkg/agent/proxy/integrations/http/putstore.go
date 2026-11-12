package http

import (
	"net/http"
	"sort"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// HTTP's own store: a PUT stores the representation it encloses as the state
// of the resource its URL names, and a later GET of that URL answers with the
// state stored (RFC 9110, 9.3.4). A test that PUTs a record and GETs it back
// (a key store such as OpenBao's or Vault's KV engine) reads what this run
// wrote, while keploy answers the GET with its recording, which holds what
// the recorded run wrote: an id or a name the run made then fails the
// application's check of what it reads back. The read-back pass
// (readBackPlan) answers such a GET with what this run's PUT stored, as it
// does for a registered store reader's read (models.HTTPStoreReader); a GET
// a registered reader reads is that reader's.
//
// WHAT IS LINKED, AND WHERE IT STOPS. A record is one resource: the Host,
// the path and the query of the request, as recorded. The GET's answer
// leaves (its JSON body's) are linked to the record; the PUT's request
// leaves (its JSON body's) are marked as stored into it. learn.ReadBack then
// links one answer leaf to one stored leaf by their recorded values, within
// that record only. The link holds only where:
//   - the GET was answered 200, and each PUT 2xx (a 4xx PUT stored nothing);
//   - each PUT sent a JSON body, and went, in this run, to the URL the GET
//     went to (its live path and query are the GET's), so the two name one
//     resource in both runs;
//   - no call of the test to that URL other than a GET, a HEAD or an OPTIONS
//     came after the last PUT (a POST, a PATCH, a DELETE, or a PUT keploy
//     cannot read changes the resource in a way keploy does not follow), and
//     a later PUT replaces what an earlier one stored.
//
// A stated limit: a call to another URL that changes the resource (a key
// store's delete or destroy endpoint) is not seen; the GET's recorded answer
// then holds what that call left there, which is linked only where it is
// what the PUT stored when recorded.

// putTarget names the resource m's recorded request names: its Host, its
// path and its query, each as recorded (parsedOf), the query's parameters in
// order. ok is false for a request keploy cannot name one resource of (no
// Host, or no path).
func putTarget(m *models.Mock) (string, bool) {
	if m == nil || m.Spec.HTTPReq == nil {
		return "", false
	}
	p := parsedOf(m).parts
	if p.host == "" || p.path == "" {
		return "", false
	}
	keys := make([]string, 0, len(p.query))
	for k := range p.query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(p.host + " " + p.path + "?")
	for _, k := range keys {
		for _, v := range p.query[k] {
			b.WriteString(k + "=" + v + "&")
		}
	}
	return b.String(), true
}

// putReads reports whether m's recorded request is a GET that names a
// resource (putTarget).
func putReads(m *models.Mock) bool {
	if m == nil || m.Spec.HTTPReq == nil || m.Spec.HTTPReq.Method != http.MethodGet {
		return false
	}
	_, ok := putTarget(m)
	return ok
}

// putFollows reports whether call names the resource read does: every call
// to its URL, whatever its method, since any but a read may change it.
func putFollows(read, call *models.Mock) bool {
	t, ok := putTarget(read)
	c, cok := putTarget(call)
	return ok && cok && t == c
}

// putAccess is what HTTP's own store reads of read, a GET (putReads), and
// calls, the test's calls to its URL before it in the order served
// (putFollows), each with its recorded answer. earlier holds the exchanges
// the test served, which hold each call's live request; live is read's live
// request as the learn step reads it. ok is false where read was not
// answered 200.
func putAccess(read *models.Mock, calls []*models.Mock, earlier []learn.Earlier, live learn.Shape) (models.HTTPStoreAccess, bool) {
	target, ok := putTarget(read)
	if !ok || read.Spec.HTTPResp == nil || read.Spec.HTTPResp.StatusCode != http.StatusOK {
		return models.HTTPStoreAccess{}, false
	}
	a := models.HTTPStoreAccess{Written: make([]map[string]string, len(calls)), Read: map[string]string{}}
	stop := func(i int) {
		for j := 0; j <= i; j++ {
			a.Written[j] = nil
		}
	}
	for i, c := range calls {
		if t, ok := putTarget(c); !ok || t != target {
			continue
		}
		switch c.Spec.HTTPReq.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			continue
		case http.MethodPut:
			code := 0
			if c.Spec.HTTPResp != nil {
				code = c.Spec.HTTPResp.StatusCode
			}
			if code >= 400 && code < 500 {
				continue
			}
			stored, ok := putStored(c)
			if ok && code >= 200 && code < 300 && sameLiveURL(earlierOfMock(earlier, c.Name), live) {
				stop(i - 1)
				a.Written[i] = map[string]string{}
				for _, path := range stored {
					a.Written[i][path] = target
				}
				continue
			}
		}
		stop(i)
	}
	for _, l := range respLeaves(read.Spec.HTTPResp) {
		if strings.HasPrefix(l.Path, "body.") {
			a.Read[l.Path] = target
		}
	}
	return a, true
}

// putStored are the leaves of c's request that the PUT stores: its JSON
// body's, by the learn step's paths. ok is false for a body that is no JSON
// object or array.
func putStored(c *models.Mock) ([]string, bool) {
	v, ok := learn.DecodeJSON([]byte(c.Spec.HTTPReq.Body))
	if !ok {
		return nil, false
	}
	switch v.(type) {
	case map[string]interface{}, []interface{}:
	default:
		return nil, false
	}
	leaves := map[string]learn.Leaf{}
	learn.JSONLeaves("body", v, leaves)
	out := make([]string, 0, len(leaves))
	for path := range leaves {
		out = append(out, path)
	}
	return out, true
}

// earlierOfMock is the exchange of earlier served from mock; its zero value
// where earlier holds none.
func earlierOfMock(earlier []learn.Earlier, mock string) learn.Earlier {
	for _, e := range earlier {
		if e.Mock == mock && !e.Given {
			return e
		}
	}
	return learn.Earlier{}
}

// urlLeaf reports whether path is a leaf of a request's URL (a path segment
// or a query value).
func urlLeaf(path string) bool {
	return strings.HasPrefix(path, "url.path.") || strings.HasPrefix(path, "url.query.")
}

// sameLiveURL reports whether e, an exchange served earlier, went in this
// run to the URL live went to: each leaf of its URL holds the live value
// live holds there, and live has no other. e with no URL leaf (none served)
// went to none.
func sameLiveURL(e learn.Earlier, live learn.Shape) bool {
	n := 0
	for _, s := range e.Seen {
		if s.Side != learn.Request || !urlLeaf(s.Leaf.Path) {
			continue
		}
		l, ok := live.Leaves[s.Leaf.Path]
		if !ok || l.Value != s.Live {
			return false
		}
		n++
	}
	for path := range live.Leaves {
		if urlLeaf(path) {
			n--
		}
	}
	return n == 0 && len(e.Seen) > 0
}
