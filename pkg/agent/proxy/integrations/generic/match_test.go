package generic

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
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
func TestAnExchangeMoreThanRecordedIsAnExtraCall(t *testing.T) {
	db := answered{mocks: []*models.Mock{exchange("mock-1", "PING"), exchange("mock-2", "PING"), exchange("mock-3", "GET k")}}
	if n := extraCall([][]byte{[]byte("PING")}, db); n != 2 {
		t.Fatalf("PING was recorded twice for the test, got %d", n)
	}
	if n := extraCall([][]byte{[]byte("SET k v")}, db); n != 0 {
		t.Fatalf("an exchange the test never recorded is an extra call: %d", n)
	}
	if n := extraCall([][]byte{[]byte("PING")}, struct{ integrations.MockMemDb }{}); n != 0 {
		t.Fatalf("without exact serving: %d", n)
	}
}
