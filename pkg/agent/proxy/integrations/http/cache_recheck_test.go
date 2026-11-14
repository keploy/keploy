package http

import (
	"errors"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// The serving rows are in package proxy (cache_recheck_test.go there); these
// pin what a store or a recording keploy kept apart tells a re-check.

// perTestFails is a store whose per-test read fails.
type perTestFails struct{ integrations.MockMemDb }

func (perTestFails) GetPerTestMocksInWindow() ([]*models.Mock, error) {
	return nil, errors.New("the per-test tier is gone")
}

// A store whose per-test read fails is read as holding a recorded
// conditional fetch: the request is then matched as it came.
func TestAFailedPerTestReadKeepsAConditionalGetAsItCame(t *testing.T) {
	in := &req{method: "GET", url: &url.URL{Path: "/v3/.deploy_status_henson.json"}}
	if !conditionalOwed(in, perTestFails{}) {
		t.Fatal("a failed per-test read was read as holding no recorded conditional fetch")
	}
}

// A recording whose answer keploy kept apart is read back to see what it
// answered: a re-check names it by the read-back answer's validators, and
// one keploy cannot read back names nothing.
func TestARecheckReadsAnAnswerKeptApartBack(t *testing.T) {
	rc := cacheRecheck{values: map[string]string{"If-None-Match": `"5f1c-9a"`}, validators: []string{"If-None-Match"}}
	mk := func(load func() (*models.HTTPResp, []models.MongoResponse, error)) *models.Mock {
		m := &models.Mock{Name: "h-1", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://js.stripe.com/v3/.deploy_status_henson.json"}}}
		m.SetResponseHydrator(load)
		return m
	}
	kept := mk(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Etag": `"5f1c-9a"`}}, nil, nil
	})
	if !rc.names(kept) {
		t.Fatal("an answer kept apart, read back with the validators the re-check carries, was not named")
	}
	lost := mk(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return nil, nil, errors.New("spill file gone")
	})
	if rc.names(lost) {
		t.Fatal("an answer keploy cannot read back was named by a re-check")
	}
}
