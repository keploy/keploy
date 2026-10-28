package replayer

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// againDb is a store under exact serving whose open test's marked
// background recordings answered in the order given.
type againDb struct {
	fakeMockDb
	answered []*models.Mock
}

func (a *againDb) ExactServing() bool                 { return true }
func (a *againDb) AnsweredBackground() []*models.Mock { return a.answered }

// Two marked recordings of one timer's statement, answered in the reverse
// of their recorded order (two connections at once): the statement run again
// is answered with the first recorded, whichever answered first, so the
// answer does not depend on how the connections raced.
func TestTwoMarkedRecordingsOfACommandAnswerAgainWithTheFirstRecorded(t *testing.T) {
	const sql = "UPDATE leases SET owner = NULL WHERE expires_at < 1761555000"
	mk := func(name string, at time.Time) *models.Mock {
		m := &models.Mock{Name: name, Kind: models.MySQL}
		m.Spec.ReqTimestampMock = at
		m.Spec.MySQLRequests = []mysql.Request{comQueryReq(sql)}
		m.Spec.MySQLResponses = []mysql.Response{{Payload: name}}
		return m
	}
	at := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	first, second := mk("bg-1", at), mk("bg-2", at.Add(time.Second))
	db := &againDb{answered: []*models.Mock{second, first}}
	if x := backgroundAgain(zap.NewNop(), comQueryReq(sql), db, newDecodeCtx()); x == nil || x.Name != "bg-1" {
		t.Fatalf("answered again with %v; want bg-1, the first recorded", x)
	}
	if db.answered[0] != second {
		t.Fatal("backgroundAgain reordered the store's list: it must only read it")
	}
}
