package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// deliverTableSvc takes the per-test table, the pool slices and the
// deliveries, refusing them with err when set.
type deliverTableSvc struct {
	poolSvc
	deliver map[string][]models.DeliveryCase
	set     bool
	err     error
}

func (s *deliverTableSvc) SetScopeDeliver(_ context.Context, d map[string][]models.DeliveryCase) error {
	s.deliver, s.set = d, true
	return s.err
}

// Under the switch the scope table carries each test's deliveries and
// installs them; an agent that cannot deliver (no such setter, or one with
// no deliverer) refuses a table with any, 501, before any test runs. Off
// the switch they are not read.
func TestScopeTableCarriesTheDeliveries(t *testing.T) {
	body := `{"mappings":{"o::TestA":["mock-3"]},"pool":{},"deliver":{"o::TestA":[{"case":"test-2","after":"mock-3","req":{"method":"POST","body":"{}"},"resp":{"status_code":200}}]}}`
	post := func(a *Agent, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	t.Setenv(provenswitch.Env, "1")
	svc := &deliverTableSvc{}
	rec := post(&Agent{logger: zap.NewNop(), svc: svc}, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, svc.deliver["o::TestA"], 1)
	require.Equal(t, models.Delivery{Case: "test-2", After: "mock-3"}, svc.deliver["o::TestA"][0].Delivery)

	svc = &deliverTableSvc{err: coreAgent.ErrNoWebhookDeliverer}
	rec = post(&Agent{logger: zap.NewNop(), svc: svc}, body)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	require.Contains(t, rec.Body.String(), "cannot deliver recorded webhooks")

	svc = &deliverTableSvc{err: errors.New("disk full")}
	rec = post(&Agent{logger: zap.NewNop(), svc: svc}, body)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	rec = post(&Agent{logger: zap.NewNop(), svc: &givenTableSvc{}}, body)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	require.Contains(t, rec.Body.String(), "cannot deliver recorded webhooks")
	rec = post(&Agent{logger: zap.NewNop(), svc: &givenTableSvc{}}, `{"mappings":{"o::TestA":["mock-3"]},"pool":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, "a table with no delivery needs no deliverer")

	t.Setenv(provenswitch.Env, "")
	svc = &deliverTableSvc{}
	require.Equal(t, http.StatusOK, post(&Agent{logger: zap.NewNop(), svc: svc}, body).Code)
	require.False(t, svc.set, "off the switch the deliveries are not read")
}
