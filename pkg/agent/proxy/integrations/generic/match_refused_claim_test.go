package generic

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// refusingStore hands out its recordings and refuses every claim of them, as
// a store does that files a recording where its claim does not look.
type refusingStore struct {
	integrations.MockMemDb
	mocks   []*models.Mock
	claimed []*models.Mock
}

func (s *refusingStore) GetSessionMocks() ([]*models.Mock, error) { return s.mocks, nil }

func (s *refusingStore) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	s.claimed = append(s.claimed, old)
	return false
}

// An exchange whose recording the store hands out but will not let a claim
// take ends in a miss: the claim that failed leaves the recording out of the
// exchange's later passes. Before, the match re-read the same recording and
// failed the same claim forever, at full CPU, and the exchange was never
// answered. One row per claim: a per-test recording and a session one. Each
// waits for the match to return; a hang ends in go test's own -timeout, which
// names the row.
func TestAGenericClaimTheStoreRefusesEndsTheExchange(t *testing.T) {
	for _, filtered := range []bool{true, false} {
		name := "session"
		if filtered {
			name = "per-test"
		}
		t.Run(name, func(t *testing.T) {
			m := exchange("refused", "PING")
			m.TestModeInfo.IsFiltered = filtered
			m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "PONG"}}}}
			db := &refusingStore{mocks: []*models.Mock{m}}
			ok, resp, err := fuzzyMatch(context.Background(), zap.NewNop(), [][]byte{[]byte("PING")}, db)
			if err != nil || ok || resp != nil {
				t.Fatalf("match: ok %v, response %v, err %v; want a miss", ok, resp, err)
			}
			if len(db.claimed) != 1 || db.claimed[0] != m {
				t.Fatalf("claims %d (%v); want the one claim of the recording, refused once", len(db.claimed), db.claimed)
			}
		})
	}
}

// refilingStore hands out its recordings and, at the first claim, files a
// new copy of the claimed one (same name, new pointer) in its place and
// refuses the claim of the old one, as when another connection's serve filed
// it again first. The copy's claim goes through.
type refilingStore struct {
	integrations.MockMemDb
	mocks   []*models.Mock
	refiled bool
}

func (s *refilingStore) GetSessionMocks() ([]*models.Mock, error) { return s.mocks, nil }

func (s *refilingStore) UpdateUnFilteredMock(old, _ *models.Mock) bool {
	if !s.refiled {
		s.refiled = true
		cp := *old
		for i, m := range s.mocks {
			if m == old {
				s.mocks[i] = &cp
			}
		}
		return false
	}
	return true
}

// A failed claim leaves out the copy it failed on, by pointer, never the
// recording by name: the new copy another connection's serve filed answers
// the exchange, as before a refused claim was left out. A per-test recording
// and a session one.
func TestAGenericRefiledCopyOfARefusedRecordingAnswersTheExchange(t *testing.T) {
	for _, filtered := range []bool{true, false} {
		name := "session"
		if filtered {
			name = "per-test"
		}
		t.Run(name, func(t *testing.T) {
			m := exchange("rec", "PING")
			m.TestModeInfo.IsFiltered = filtered
			m.Spec.GenericResponses = []models.Payload{{Message: []models.OutputBinary{{Type: models.String, Data: "PONG"}}}}
			db := &refilingStore{mocks: []*models.Mock{m}}
			ok, resp, err := fuzzyMatch(context.Background(), zap.NewNop(), [][]byte{[]byte("PING")}, db)
			if err != nil || !ok || len(resp) != 1 || !db.refiled {
				t.Fatalf("match: ok %v, response %v, err %v, refiled %v; want the new copy's PONG", ok, resp, err, db.refiled)
			}
		})
	}
}
