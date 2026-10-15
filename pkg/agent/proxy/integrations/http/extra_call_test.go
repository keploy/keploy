package http

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// answeredDb is a mockMemDb whose open test has answered the mocks answered
// (exact serving): they are in none of its reads.
type answeredDb struct {
	*mockMemDb
	answered []*models.Mock
}

func (a answeredDb) ConsumedTestMocks() []*models.Mock { return a.answered }

const answeredHost = "127.0.0.1:18093"

// getQuery is a live GET of path?query to answeredHost.
func getQuery(path, query string) *req {
	r := getTo(path, answeredHost)
	r.url.RawQuery = query
	return r
}

// missOf matches input against db, which must miss, and returns the match's
// diagnosis and answeredDiag's.
func missOf(t *testing.T, input *req, db answeredDb) (matched, widened *matchDiag) {
	t.Helper()
	ctx := context.Background()
	h := newHTTP()
	ok, _, diag, err := h.match(ctx, input, db, nil, nil, nil, true, false, false, true, true)
	if err != nil || ok {
		t.Fatalf("match: ok %v, err %v; want a miss", ok, err)
	}
	return diag, h.answeredDiag(ctx, input, db, diag, nil, nil, nil)
}

// Under exact serving a test's mock is in no read once it answered, so a
// call the test makes after it found no recording to be diagnosed against:
// the miss named nothing that differs ("no_mocks"; enterprise lane 2's
// replay-miss on keploy v3.6.127). answeredDiag compares the call with the
// test's answered mocks too, by the strict comparison's rule for the closest
// recording, and the miss names what differs from it, as it did before
// exact serving (keploy S1). They are compared, never served.
func TestAMissIsDiagnosedAgainstTheTestsAnsweredMocks(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	stock := onHost("stock", "/stock/book", answeredHost)
	db := answeredDb{mockMemDb: &mockMemDb{}, answered: []*models.Mock{stock}}

	matched, diag := missOf(t, getTo("/reserve/book", answeredHost), db)
	if matched.phase != models.MatchPhaseNoMocks || matched.closest != nil {
		t.Fatalf("the match itself saw the answered mock: phase %q, closest %v", matched.phase, matched.closest)
	}
	if diag.closest != stock || !reflect.DeepEqual(differsOf(diag.differs), []string{"path"}) {
		t.Fatalf("closest %v differing in %v; want stock, differing in its path", diag.closest, differsOf(diag.differs))
	}
	if diag.phase != models.MatchPhaseSchema || diag.candidates != 1 || len(diag.pool) != 1 {
		t.Fatalf("phase %q, %d candidates, pool %d; want %q, 1, 1", diag.phase, diag.candidates, len(diag.pool), models.MatchPhaseSchema)
	}
	request := &http.Request{Method: "GET", URL: &url.URL{Path: "/reserve/book"}, Host: answeredHost, Header: http.Header{"Accept": {"application/json"}}}
	r := newHTTP().buildHTTPMismatchReport(request, nil, db, nil, nil, nil, diag)
	if r == nil || r.ClosestMock != "stock" || !reflect.DeepEqual(r.Differs, []string{"path"}) || r.MatchPhase != models.MatchPhaseSchema {
		t.Fatalf("report %+v; want stock closest, its path differing, phase %q", r, models.MatchPhaseSchema)
	}
}

// An answered mock of the call's route (method, destination, path) is closer
// than a recording of another route the pool still holds; on a tie the
// pool's stays closest, and the diagnosis is the match's own.
func TestTheAnsweredMocksCompeteWithThePoolsClosest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	price := onHost("price", "/price/book", answeredHost)
	reserved := onHost("reserved", "/reserve/book?n=1", answeredHost)
	_, diag := missOf(t, getQuery("/reserve/book", "n=2"), answeredDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{price}}, answered: []*models.Mock{reserved}})
	if diag.closest != reserved || diag.phase != models.MatchPhaseStrict {
		t.Fatalf("closest %v, phase %q; want the answered mock of the call's route, %q", diag.closest, diag.phase, models.MatchPhaseStrict)
	}
	if len(diag.schemaMatched) != 1 || diag.schemaMatched[0] != reserved || diag.candidates != 2 {
		t.Fatalf("schema-matched %v of %d candidates; want reserved of 2", diag.schemaMatched, diag.candidates)
	}

	stock := onHost("stock", "/stock/book", answeredHost)
	matched, diag := missOf(t, getTo("/reserve/book", answeredHost), answeredDb{mockMemDb: &mockMemDb{mocks: []*models.Mock{price}}, answered: []*models.Mock{stock}})
	if diag != matched || diag.closest != price {
		t.Fatalf("on a tie the diagnosis changed: closest %v; want the pool's price, as the match found it", diag.closest)
	}
}

// An exact repeat of an answered call is an extra call (extraCall), not a
// call to diagnose: the answered mock it repeats is no closest recording, it
// is the call itself. A miss whose recording fit but whose variables did not
// hold reports that. Without the switch nothing changes.
func TestTheAnsweredMocksLeaveOtherDiagnosesAlone(t *testing.T) {
	stock := onHost("stock", "/stock/book", answeredHost)
	db := answeredDb{mockMemDb: &mockMemDb{}, answered: []*models.Mock{stock}}

	t.Setenv(provenswitch.Env, "1")
	if matched, diag := missOf(t, getTo("/stock/book", answeredHost), db); diag != matched {
		t.Fatalf("an exact repeat was diagnosed against the call it repeats: closest %v", diag.closest)
	}
	// An answered mock that declares variable places is not compared
	// strictly, as in the live pool.
	withVar := onHost("stock-var", "/stock/book", answeredHost)
	withVar.Spec.Variables = models.MockVariables{"item": bindV("book", "url.path[1]")}
	if matched, diag := missOf(t, getTo("/reserve/book", answeredHost), answeredDb{mockMemDb: &mockMemDb{}, answered: []*models.Mock{withVar}}); diag != matched {
		t.Fatalf("an answered mock with variables was compared strictly: closest %v", diag.closest)
	}
	variable := &matchDiag{phase: models.MatchPhaseVariable, variable: &varProblem{}}
	if got := newHTTP().answeredDiag(context.Background(), getTo("/reserve/book", answeredHost), db, variable, nil, nil, nil); got != variable {
		t.Fatalf("a variable miss was diagnosed again: %+v", got)
	}

	t.Setenv(provenswitch.Env, "")
	if matched, diag := missOf(t, getTo("/reserve/book", answeredHost), db); diag != matched {
		t.Fatalf("without the switch the diagnosis changed: %+v", diag)
	}
}
