package http

import (
	"net/http"
	"sort"
	"strconv"
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
//     a later PUT replaces what an earlier one stored;
//   - no call of the test after that PUT to another URL of the Host, other
//     than a read, may have written the field's value otherwise
//     (putWrittenElsewhere): a store's delete, reset or transfer endpoint
//     whose request held the value, as recorded, and sent another in this
//     run, or held it where keploy does not read it, leaves the field
//     answered as recorded. One that sent this run's value there (an index
//     of the record the application keeps at another URL) wrote what the PUT
//     wrote.
//
// A stored field the PUT's learn step learned as a read of the application's
// clock (a createdAt or an expiresAt the application stamps) is answered as
// recorded (readBackPlan's clockWritten): the HTTP replay cannot hold one
// place of a call to both a variable and a read of the clock, so a variable
// bound there would refuse the PUT in every later replay. That recorded
// stamp is what the PUT wrote: the run clock begins each test at its
// recorded instant, so the application stamps the PUT with the recorded
// moment.

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
// calls, the test's calls to its URL before it in the order served, each
// with its recorded answer: readBackPlan passes only the calls putFollows
// names, so each call names read's resource. earlier holds the exchanges
// the test served, which hold each call's live request; live is read's live
// request as the learn step reads it. ok is false where read was not
// answered 200.
func putAccess(read *models.Mock, calls []*models.Mock, earlier []learn.Earlier, live learn.Shape) (models.HTTPStoreAccess, bool) {
	// readBackPlan passes only a read putReads names, whose resource
	// putTarget names.
	target, _ := putTarget(read)
	if read.Spec.HTTPResp == nil || read.Spec.HTTPResp.StatusCode != http.StatusOK {
		return models.HTTPStoreAccess{}, false
	}
	a := models.HTTPStoreAccess{Written: make([]map[string]string, len(calls)), Read: map[string]string{}}
	stop := func(i int) {
		for j := 0; j <= i; j++ {
			a.Written[j] = nil
		}
	}
	for i, c := range calls {
		if putReadOnly(c) {
			continue
		}
		if c.Spec.HTTPReq.Method == http.MethodPut {
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
	if !storedAny(a.Written) {
		// Nothing stored: no leaf of the answer is read back, so it is not
		// decoded.
		return a, true
	}
	for _, l := range respLeaves(read.Spec.HTTPResp) {
		if strings.HasPrefix(l.Path, "body.") {
			a.Read[l.Path] = target
		}
	}
	return a, true
}

// putReadOnly reports whether c's recorded request is a read, which changes
// no resource: a GET, a HEAD or an OPTIONS.
func putReadOnly(c *models.Mock) bool {
	switch c.Spec.HTTPReq.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// storedAny reports whether a call of written stored a leaf.
func storedAny(written []map[string]string) bool {
	for _, w := range written {
		if len(w) > 0 {
			return true
		}
	}
	return false
}

// putWrittenElsewhere names a call of served (the test's calls before the
// read, in the order served) that may have written recorded, a value of
// read's answer that learn.ReadBack links to write (the PUT that stored
// it), into read's record otherwise than write did: a call after write, to
// read's Host and to another URL (putAccess judges each call to read's URL),
// other than a read (putReadOnly), whose recorded request holds recorded
// and that did not send live, the value write stored there, in its place
// (sentAsWritten). Such a call (a key store's delete, reset or transfer
// endpoint) may have left the recorded value in the record, or another, so
// the read answers it as recorded. A call that sent live in its place
// wrote what write wrote, in whichever order the two came. earlier holds
// the exchanges the test served (each call's live request); ok is false
// where no call does.
func putWrittenElsewhere(read *models.Mock, served []*models.Mock, earlier []learn.Earlier, write, recorded, live string) (string, bool) {
	host := parsedOf(read).parts.host
	after := false
	for _, c := range served {
		if c.Name == write {
			after = true
			continue
		}
		if !after || c.Spec.HTTPReq == nil || putReadOnly(c) || putFollows(read, c) || parsedOf(c).parts.host != host {
			continue
		}
		if !sentAsWritten(earlierOfMock(earlier, c.Name), c, recorded, live) {
			return c.Name, true
		}
	}
	return "", false
}

// sentAsWritten reports whether c, a call served from e (its exchange),
// sent live wherever its recorded request held recorded: each leaf of e's
// request that holds recorded held, in this run, that leaf with live in its
// place, and no part of c's recorded request holds recorded where no leaf
// of e does. e holds a leaf for each part keploy read of the call (its path
// segments unmasked, its query, its body whole or by leaf and its keys, its
// headers, an excused one where the call sent it); a part it read nothing
// of (an excused header the call did not send this time, a keploy header,
// the Host), and every part where e is the zero Earlier (no exchange of c's
// mock), is one keploy cannot tell what the call sent in, so it is false.
func sentAsWritten(e learn.Earlier, c *models.Mock, recorded, live string) bool {
	at := map[string]bool{}
	for _, s := range e.Seen {
		if s.Side != learn.Request || !strings.Contains(s.Leaf.Value, recorded) {
			continue
		}
		if s.Live != strings.ReplaceAll(s.Leaf.Value, recorded, live) {
			return false
		}
		part := s.Leaf.Path
		if strings.HasPrefix(part, "body") {
			// A body's leaves are its one part.
			part = "body"
		}
		at[part] = true
	}
	p := parseRecordedParts(c)
	parts := map[string]string{"body": string(p.body)}
	for i, seg := range strings.Split(strings.TrimPrefix(p.path, "/"), "/") {
		parts["url.path."+strconv.Itoa(i)] = seg
	}
	for k, vs := range p.query {
		parts["url.query."+k] = strings.Join(vs, "\x00")
	}
	for k, v := range p.header {
		parts["header."+k] = v
	}
	for part, text := range parts {
		if strings.Contains(text, recorded) && !at[part] {
			return false
		}
	}
	return true
}

// putStored are the leaves of c's request that the PUT stores: its JSON
// body's, by the learn step's paths. ok is false for a body that is no JSON
// object or array.
func putStored(c *models.Mock) ([]string, bool) {
	// A body that is no JSON decodes to nil, which is neither.
	v, _ := learn.DecodeJSON([]byte(c.Spec.HTTPReq.Body))
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
// went to none. Only a request's leaves are a URL's (an answer's are its
// status, its headers' and its body's).
func sameLiveURL(e learn.Earlier, live learn.Shape) bool {
	n := 0
	for _, s := range e.Seen {
		if !urlLeaf(s.Leaf.Path) {
			continue
		}
		// A leaf live lacks reads "" here, and is counted below.
		if live.Leaves[s.Leaf.Path].Value != s.Live {
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
