package generic

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// answered is a store whose open test's answered mocks are mocks (exact
// serving, under the proven switch).
type answered struct {
	integrations.MockMemDb
	mocks []*models.Mock
}

func (a answered) ConsumedTestMocks() []*models.Mock { return a.mocks }

func exchange(name string, req ...string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.GENERIC}
	for _, r := range req {
		m.Spec.GenericRequests = append(m.Spec.GenericRequests, models.Payload{Message: []models.OutputBinary{{Type: models.String, Data: r}}})
	}
	return m
}

// An exchange the open test's own mocks already answered, as often as it
// was recorded, is one exchange more than recorded; any other unanswered
// exchange is not, and nothing is one without exact serving.
//
// The exchange repeats the test's copy of it recorded last, whatever order
// they answered in or are named in: a mark on it leaves the earlier copies
// required.
func TestAnExchangeMoreThanRecordedIsAnExtraCall(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	early, late := exchange("mock-9", "PING"), exchange("mock-2", "PING")
	early.Spec.ReqTimestampMock, late.Spec.ReqTimestampMock = at, at.Add(time.Second)
	for _, order := range [][]*models.Mock{{late, early}, {early, late}} {
		db := answered{mocks: append(order, exchange("mock-3", "GET k"))}
		if n, repeats := extraCall([][]byte{[]byte("PING")}, db); n != 2 || repeats != late {
			t.Fatalf("PING was recorded twice for the test, mock-2 last, answered %s first: got %d, repeating %v", order[0].Name, n, repeats)
		}
	}
	db := answered{mocks: []*models.Mock{late, early, exchange("mock-3", "GET k")}}
	if n, repeats := extraCall([][]byte{[]byte("SET k v")}, db); n != 0 || repeats != nil {
		t.Fatalf("an exchange the test never recorded is an extra call: %d, repeating %v", n, repeats)
	}
	if n, repeats := extraCall([][]byte{[]byte("PING")}, struct{ integrations.MockMemDb }{}); n != 0 || repeats != nil {
		t.Fatalf("without exact serving: %d, repeating %v", n, repeats)
	}
}

// answeredStore is a pingStore whose open test has answered the exchanges
// answered (exact serving): they are in none of its reads.
type answeredStore struct {
	pingStore
	answered []*models.Mock
}

func (s answeredStore) ConsumedTestMocks() []*models.Mock { return s.answered }

// missReport is the report of the miss decodeGeneric returns for req.
func missReport(t *testing.T, store integrations.MockMemDb, req string) *models.MockMismatchReport {
	t.Helper()
	app, proxySide := net.Pipe()
	defer app.Close()
	err := decodeGeneric(context.Background(), zap.NewNop(), []byte(req), proxySide, nil, store, models.OutgoingOptions{})
	var r interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &r) || r.MismatchReport() == nil {
		t.Fatalf("%q: no miss with a report: %v", req, err)
	}
	return r.MismatchReport()
}

// Under exact serving an exchange the test recorded is in no read once it
// answered, so a changed exchange the test sends after it found nothing to
// be compared with ("no_mocks"). It is compared with the test's answered
// exchanges too, after the pool's, never answered by them. An exact repeat
// stays an extra call, compared as before.
func TestAChangedExchangeIsComparedWithTheTestsAnsweredOnes(t *testing.T) {
	store := answeredStore{answered: []*models.Mock{exchange("mock-1", "SET k v")}}
	r := missReport(t, store, "SET k w")
	if r.MissKind == models.MissExtraCall {
		t.Fatalf("a changed exchange was filed as an extra call")
	}
	if r.ClosestMock != "mock-1" || r.MatchPhase != models.MatchPhaseExhausted || r.CandidateCount != 1 {
		t.Fatalf("closest %q, phase %q, %d candidates; want mock-1, %q, 1", r.ClosestMock, r.MatchPhase, r.CandidateCount, models.MatchPhaseExhausted)
	}

	// The pool's exchanges are compared too: one of two requests is not
	// comparable with a one-request exchange, and the answered one is
	// closest of the two.
	pooled := exchange("mock-0", "MULTI", "EXEC")
	r = missReport(t, answeredStore{pingStore: pingStore{mocks: []*models.Mock{pooled}}, answered: store.answered}, "SET k w")
	if r.ClosestMock != "mock-1" || r.CandidateCount != 2 {
		t.Fatalf("closest %q of %d candidates; want the answered mock-1 of 2", r.ClosestMock, r.CandidateCount)
	}

	r = missReport(t, store, "SET k v")
	if r.MissKind != models.MissExtraCall || r.Recorded != 1 {
		t.Fatalf("an exact repeat: kind %q, recorded %d; want an extra call recorded once", r.MissKind, r.Recorded)
	}
	if r.ClosestMock != "mock-1" || r.MatchPhase != models.MatchPhaseNoMocks {
		t.Fatalf("an extra call is named as the recording it repeats and compared with nothing else: closest %q, phase %q; want mock-1, %q", r.ClosestMock, r.MatchPhase, models.MatchPhaseNoMocks)
	}

	// Another exchange in view, however like the call, is not named: its
	// difference would read as noise to declare, which would answer the
	// call with that exchange's answer.
	near := exchange("mock-0", "SET k x")
	r = missReport(t, answeredStore{pingStore: pingStore{mocks: []*models.Mock{near}}, answered: store.answered}, "SET k v")
	if r.MissKind != models.MissExtraCall || r.ClosestMock != "mock-1" || len(r.FieldDiffs)+len(r.Differs) != 0 ||
		!strings.Contains(r.Diff, "repeats mock-1") || !strings.Contains(r.NextSteps, "repeats mock-1") || strings.Contains(r.Diff+r.NextSteps, "mock-0") {
		t.Fatalf("an extra call with mock-0 in view: %+v; want it named as mock-1, the recording it repeats, and mock-0 nowhere", r)
	}
}
