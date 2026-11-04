package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// repeatTableSvc takes the per-test table, the pool slices and each test's
// repeated reads.
type repeatTableSvc struct {
	poolSvc
	reads map[string][]models.RepeatRead
	set   bool
}

func (s *repeatTableSvc) SetScopeRepeatReads(_ context.Context, r map[string][]models.RepeatRead) error {
	s.reads, s.set = r, true
	return nil
}

// Under the switch the scope table carries each test's repeated reads and
// installs them on every push (one without clears the last); an agent that
// cannot take them says so when there are any, and with the switch off they
// are not read.
func TestScopeTableCarriesEachTestsRepeatedReads(t *testing.T) {
	body := `{"mappings":{"o::TestA":["mock-3","mock-4"]},"pool":{},"repeatReads":{"o::TestA":[{"name":"onboarding","mocks":["mock-3","mock-4"]}]}}`
	post := func(a *Agent, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.HandleScopeTable(rec, httptest.NewRequest(http.MethodPost, "/agent/scope/table", strings.NewReader(body)))
		return rec
	}
	t.Setenv(provenswitch.Env, "1")
	svc := &repeatTableSvc{}
	a := &Agent{logger: zap.NewNop(), svc: svc}
	rec := post(a, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, map[string][]models.RepeatRead{"o::TestA": {{Name: "onboarding", Mocks: []string{"mock-3", "mock-4"}}}}, svc.reads)
	rec = post(a, `{"mappings":{"o::TestA":["mock-3"]},"pool":{}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, svc.set)
	require.Empty(t, svc.reads, "a push without repeated reads clears the last push's")

	rec = post(&Agent{logger: zap.NewNop(), svc: &poolSvc{}}, body)
	require.Equal(t, http.StatusNotImplemented, rec.Code)
	require.Contains(t, rec.Body.String(), "repeated read")
	require.Equal(t, http.StatusOK, post(&Agent{logger: zap.NewNop(), svc: &poolSvc{}}, `{"mappings":{},"pool":{}}`).Code)

	t.Setenv(provenswitch.Env, "")
	svc = &repeatTableSvc{}
	require.Equal(t, http.StatusOK, post(&Agent{logger: zap.NewNop(), svc: svc}, body).Code)
	require.False(t, svc.set, "off the switch the repeated reads are not read")
}
