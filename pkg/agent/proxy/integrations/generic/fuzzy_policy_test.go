package generic

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type genericMemDb struct {
	mocks []*models.Mock
}

func (m *genericMemDb) GetUnFilteredMocks() ([]*models.Mock, error) { return m.mocks, nil }
func (m *genericMemDb) GetFilteredMocks() ([]*models.Mock, error)   { return nil, nil }
func (m *genericMemDb) UpdateUnFilteredMock(_ *models.Mock, _ *models.Mock) bool {
	return true
}
func (m *genericMemDb) DeleteFilteredMock(_ models.Mock) bool             { return false }
func (m *genericMemDb) DeleteUnFilteredMock(_ models.Mock) bool           { return false }
func (m *genericMemDb) DeleteStartupMock(_ models.Mock) bool              { return false }
func (m *genericMemDb) GetMySQLCounts() (total, config, data int)         { return 0, 0, 0 }
func (m *genericMemDb) MarkMockAsUsed(_ models.Mock) bool                 { return false }
func (m *genericMemDb) SetCurrentTestWindow(_, _ time.Time)               {}
func (m *genericMemDb) IsTestWindowActive() bool                          { return false }
func (m *genericMemDb) GetFilteredMocksInWindow() ([]*models.Mock, error) { return nil, nil }
func (m *genericMemDb) GetPerTestMocksInWindow() ([]*models.Mock, error)  { return nil, nil }
func (m *genericMemDb) GetSessionMocks() ([]*models.Mock, error)          { return m.mocks, nil }
func (m *genericMemDb) GetStartupMocks() ([]*models.Mock, error)          { return nil, nil }
func (m *genericMemDb) GetStartupMocksByKind(_ models.Kind) ([]*models.Mock, error) {
	return nil, nil
}
func (m *genericMemDb) GetSessionScopedMocks() ([]*models.Mock, error)      { return m.mocks, nil }
func (m *genericMemDb) HasFirstTestFired() bool                             { return false }
func (m *genericMemDb) FirstTestWindowStart() time.Time                     { return time.Time{} }
func (m *genericMemDb) WindowSnapshot() models.WindowSnapshot               { return models.WindowSnapshot{} }
func (m *genericMemDb) CurrentTestWindow() (time.Time, time.Time)           { return time.Time{}, time.Time{} }
func (m *genericMemDb) GetConnectionMocks(_ string) ([]*models.Mock, error) { return nil, nil }
func (m *genericMemDb) SessionMockHitCounts() map[string]uint64             { return nil }

func genericMock(name, reqData string) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: "Generic",
		Spec: models.MockSpec{
			GenericRequests: []models.Payload{
				{Message: []models.OutputBinary{{Type: "String", Data: reqData}}},
			},
			GenericResponses: []models.Payload{
				{Message: []models.OutputBinary{{Type: "String", Data: "resp"}}},
			},
		},
	}
}

// A similar-but-not-equal buffer must not be served under fuzzyMatch=off, and
// must be served (legacy Jaccard fallback) under on/warn.
//
// Note: the Jaccard fallback decodes mock data as base64 (binary recordings);
// the recorded payload is stored encoded, exactly as the recorder does for
// non-ASCII wire data.
func TestGenericFuzzyMatch_PolicyGate(t *testing.T) {
	logger := zap.NewNop()
	// Long shared prefix → Jaccard similarity above the 0.4 session threshold.
	recorded := "SELECT id,name,email FROM users WHERE tenant='acme' AND created_at > '2026-01-01'"
	live := "SELECT id,name,email FROM users WHERE tenant='acme' AND created_at > '2026-06-12'"

	db := &genericMemDb{mocks: []*models.Mock{genericMock("mock-1", util.EncodeBase64([]byte(recorded)))}}

	matched, _, err := fuzzyMatch(context.Background(), logger, [][]byte{[]byte(live)}, db, models.FuzzyMatchOff)
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("fuzzyMatch=off must not serve a similarity guess")
	}

	matched, _, err = fuzzyMatch(context.Background(), logger, [][]byte{[]byte(live)}, db, models.FuzzyMatchWarn)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("fuzzyMatch=warn should keep the legacy similarity fallback")
	}
}

// Exact matches are deterministic and must be served under every policy.
func TestGenericFuzzyMatch_ExactUnaffectedByPolicy(t *testing.T) {
	logger := zap.NewNop()
	data := "PING"
	db := &genericMemDb{mocks: []*models.Mock{genericMock("mock-1", data)}}

	matched, _, err := fuzzyMatch(context.Background(), logger, [][]byte{[]byte(data)}, db, models.FuzzyMatchOff)
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("exact match must be served even under fuzzyMatch=off")
	}
}
