package agent

import (
	"context"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type proxySpy struct {
	filtered   []*models.Mock
	unfiltered []*models.Mock
}

func (p *proxySpy) StartProxy(context.Context, coreAgent.ProxyOptions) error { return nil }

func (p *proxySpy) Record(context.Context, chan<- *models.Mock, models.OutgoingOptions) error {
	return nil
}

func (p *proxySpy) Mock(context.Context, models.OutgoingOptions) error { return nil }

func (p *proxySpy) SetMocks(_ context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error {
	p.filtered = filtered
	p.unfiltered = unFiltered
	return nil
}

func (p *proxySpy) GetConsumedMocks(context.Context) ([]models.MockState, error) { return nil, nil }

func (p *proxySpy) GetMockErrors(context.Context) ([]models.UnmatchedCall, error) { return nil, nil }

func (p *proxySpy) MakeClientDeRegisterd(context.Context) error { return nil }

func (p *proxySpy) GetErrorChannel() <-chan error { return nil }

func (p *proxySpy) SetGracefulShutdown(context.Context) error { return nil }

func (p *proxySpy) Mapping(context.Context, chan models.TestMockMapping) {}

func (p *proxySpy) GetDestInfo() coreAgent.DestInfo { return nil }

func (p *proxySpy) GetIntegrations() map[integrations.IntegrationType]integrations.Integrations {
	return nil
}

func (p *proxySpy) GetSession() *coreAgent.Session { return nil }

func (p *proxySpy) SetAuxiliaryHook(coreAgent.AuxiliaryProxyHook) {}

func TestUpdateMockParamsFiltersDeletedMocksFromUnfilteredSet(t *testing.T) {
	ctx := context.Background()
	proxy := &proxySpy{}
	agent := New(zap.NewNop(), nil, proxy, nil, nil, nil)

	filtered := []*models.Mock{{Name: "filtered-keep"}, {Name: "filtered-delete"}}
	unfiltered := []*models.Mock{{Name: "unfiltered-keep"}, {Name: "unfiltered-delete"}}

	if err := agent.StoreMocks(ctx, filtered, unfiltered); err != nil {
		t.Fatalf("store mocks: %v", err)
	}

	err := agent.UpdateMockParams(ctx, models.MockFilterParams{
		TotalConsumedMocks: map[string]models.MockState{
			"filtered-delete":   {Usage: models.Deleted},
			"unfiltered-delete": {Usage: models.Deleted},
		},
	})
	if err != nil {
		t.Fatalf("update mock params: %v", err)
	}

	if len(proxy.filtered) != 1 || proxy.filtered[0].Name != "filtered-keep" {
		t.Fatalf("filtered mocks = %#v, want only filtered-keep", mockNames(proxy.filtered))
	}

	if len(proxy.unfiltered) != 1 || proxy.unfiltered[0].Name != "unfiltered-keep" {
		t.Fatalf("unfiltered mocks = %#v, want only unfiltered-keep", mockNames(proxy.unfiltered))
	}
}

func TestUpdateMockParamsPropagatesConsumedStateToUnfilteredSet(t *testing.T) {
	ctx := context.Background()
	proxy := &proxySpy{}
	agent := New(zap.NewNop(), nil, proxy, nil, nil, nil)

	filtered := []*models.Mock{{Name: "filtered"}}
	unfiltered := []*models.Mock{{Name: "mysql-a"}, {Name: "mysql-b"}}

	if err := agent.StoreMocks(ctx, filtered, unfiltered); err != nil {
		t.Fatalf("store mocks: %v", err)
	}

	err := agent.UpdateMockParams(ctx, models.MockFilterParams{
		TotalConsumedMocks: map[string]models.MockState{
			"mysql-a": {
				Usage:      models.Updated,
				IsFiltered: true,
				SortOrder:  99,
			},
		},
	})
	if err != nil {
		t.Fatalf("update mock params: %v", err)
	}

	if len(proxy.unfiltered) != 2 {
		t.Fatalf("unfiltered mock count = %d, want 2", len(proxy.unfiltered))
	}

	updated := proxy.unfiltered[0]
	if updated.Name != "mysql-a" {
		t.Fatalf("first unfiltered mock = %q, want mysql-a", updated.Name)
	}
	if !updated.TestModeInfo.IsFiltered {
		t.Fatalf("mysql-a IsFiltered = false, want true")
	}
	if updated.TestModeInfo.SortOrder != 99 {
		t.Fatalf("mysql-a SortOrder = %d, want 99", updated.TestModeInfo.SortOrder)
	}
}

func mockNames(mocks []*models.Mock) []string {
	names := make([]string, 0, len(mocks))
	for _, mock := range mocks {
		if mock == nil {
			names = append(names, "<nil>")
			continue
		}
		names = append(names, mock.Name)
	}
	return names
}
