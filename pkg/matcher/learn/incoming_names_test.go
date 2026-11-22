package learn

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// TestAClientTestsNamesAreBoundAtItsRequestWhereverACallReadsThem: api-server's
// e2e client names what it makes by its own clock (UnixNano): a suite
// "[E2E] suite-<nanos>", a version history's "hist-<nanos>", an app
// "[E2E] findme<nanos>-<nanos>" it then searches for by "findme<nanos>", and a
// trust policy's repository "keploy/repo-<nanos>". The application stores
// and reads each back, whole or as one word of a longer text (a trust
// policy's subject "repo:keploy/repo-<nanos>:ref:refs/heads/main"). Each
// changed word is a Value bound where the client's request sent it, at its
// word there where the request's leaf is a longer text: never a read of the
// call's clock, nor a value the application made.
func TestAClientTestsNamesAreBoundAtItsRequestWhereverACallReadsThem(t *testing.T) {
	rNanos := int64(1794207702156240454)
	rReq := time.Unix(0, rNanos).Add(131 * time.Microsecond)
	gap := 69*time.Second + 3*time.Millisecond
	fNanos, fReq := rNanos+gap.Nanoseconds(), rReq.Add(gap)
	rCall, fCall := rReq.Add(2*time.Millisecond), fReq.Add(2*time.Millisecond)
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	post := func(url, body string) func(int64, time.Time) *models.HTTPReq {
		return func(v int64, at time.Time) *models.HTTPReq {
			return &models.HTTPReq{Method: "POST", URL: "http://localhost:8083" + url, Timestamp: at,
				Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
				Body:   `{"name":"` + body + n(v) + `"}`}
		}
	}
	trust := func(v int64, at time.Time) *models.HTTPReq {
		return &models.HTTPReq{Method: "PUT", URL: "http://localhost:8083/client/v1/service-accounts/sa-1/trust", Timestamp: at,
			Header: map[string]string{"Content-Type": "application/json", "Host": "localhost:8083"},
			Body:   `{"repository":"keploy/repo-` + n(v) + `","ref":"refs/heads/main"}`}
	}
	search := func(v int64, at time.Time) *models.HTTPReq {
		return &models.HTTPReq{Method: "GET", URL: "http://localhost:8083/client/v1/apps?q=findme" + n(v), Timestamp: at,
			Header: map[string]string{"Host": "localhost:8083"}}
	}
	for _, c := range []struct {
		what, path, recText, liveText string
		req                           func(int64, time.Time) *models.HTTPReq
		from                          string
		word                          int
	}{
		{what: "a suite's name read back whole", path: "body.cmd.filter.name", req: post("/client/v1/apps/a1/test-suites", "[E2E] suite-"),
			recText: "[E2E] suite-" + n(rNanos), liveText: "[E2E] suite-" + n(fNanos), from: "body.name", word: 2},
		{what: "a version history's name read back whole", path: "body.docs.msgs.0.name", req: post("/client/v1/apps/a1/test-suites", "hist-"),
			recText: "hist-" + n(rNanos), liveText: "hist-" + n(fNanos), from: "body.name", word: 1},
		{what: "the app name a distinct reads as one word of a longer text", path: "body.cmd.query.app.name", req: post("/client/v1/apps", "[E2E] findme"),
			recText: "[E2E] findme" + n(rNanos) + "-x", liveText: "[E2E] findme" + n(fNanos) + "-x", from: "body.name", word: 1},
		{what: "a search token the request's query sent", path: "body.cmd.filter.name.$regex", req: search,
			recText: "findme" + n(rNanos), liveText: "findme" + n(fNanos), from: "url.query.q", word: 0},
		{what: "a repository inside the trust policy's subject", path: "body.docs.msgs.0.subject", req: trust,
			recText: "repo:keploy/repo-" + n(rNanos) + ":ref:refs/heads/main", liveText: "repo:keploy/repo-" + n(fNanos) + ":ref:refs/heads/main", from: "body.repository", word: 2},
	} {
		in := iCall(c.path, String, c.recText, c.liveText, rCall, fCall, nil, clientPlaced(t, "case-1", c.req(rNanos, rReq), c.req(fNanos, fReq), fReq), nil)
		in.Drifts = oClientDrifts()
		d := oWord(t, Decide(in), n(rNanos), n(fNanos))
		if d.Outcome != OutcomeValue || d.From.Mock != "test case case-1" || d.From.Path != c.from || !d.From.Client || d.Clock != nil {
			t.Errorf("%s, %q -> %q: %s (client test's request: %v, at %s); want a Value of test case case-1's %s, a client test's, no clock", c.what, c.recText, c.liveText, oDescribe([]Decision{d}), d.From.Client, d.FromAt, c.from)
			continue
		}
		if d.FromAt.Word != c.word {
			t.Errorf("%s: bound at %s; want word %d of %s", c.what, d.FromAt, c.word, c.from)
		}
	}
}
