package record

import (
	"context"
	"net"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// kindIntegration is a registered integration whose AsyncParser takes the
// mocks of one kind only, as a real parser takes its own protocol's calls.
type kindIntegration struct {
	kind  models.Kind
	types []string
}

func (kindIntegration) MatchType(context.Context, []byte) bool { return false }
func (kindIntegration) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return nil
}
func (kindIntegration) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return nil
}
func (k kindIntegration) MatchesLane(m *models.Mock, _ models.AsyncLane) bool {
	return m != nil && m.Kind == k.kind
}
func (kindIntegration) MatchRequestShape(_, _ *models.Mock, _ models.AsyncLane) (bool, string) {
	return true, ""
}
func (kindIntegration) EmptyResponse(models.AsyncLane) ([]byte, error) { return nil, nil }
func (k kindIntegration) ResponseValueKey(m *models.Mock, _ models.AsyncLane) string {
	return string(k.kind) + m.Spec.Metadata["value"]
}

// laneTypedIntegration is a kindIntegration that serves lanes of other types.
type laneTypedIntegration struct{ kindIntegration }

func (l laneTypedIntegration) LaneTypes() []string { return l.types }

// An "http" lane's calls may be recorded over HTTP/2 as well as HTTP/1.1: the
// recorder stamps a call of either protocol, through the parser that serves
// the lane's type for that protocol (the HTTP/2 parser declares it serves
// "http" lanes), and diffs a poll's values with the same parser.
func TestTheRecorderStampsALaneCallOfEveryProtocolThatServesTheLane(t *testing.T) {
	const h1Type, h2Type = "zz-lanetype-h1", "zz-lanetype-h2"
	integrations.Register(h1Type, &integrations.Parsers{Initializer: func(*zap.Logger) integrations.Integrations {
		return kindIntegration{kind: models.HTTP}
	}})
	integrations.Register(h2Type, &integrations.Parsers{Initializer: func(*zap.Logger) integrations.Integrations {
		return laneTypedIntegration{kindIntegration{kind: models.HTTP2, types: []string{h1Type}}}
	}})
	t.Cleanup(func() {
		delete(integrations.Registered, h1Type)
		delete(integrations.Registered, h2Type)
	})

	lanes := []models.AsyncLane{{Name: "watch", Type: h1Type + "Poll"}}
	r := NewAsyncRecorder(zap.NewNop(), lanes, ResolveAsyncParsers(zap.NewNop(), lanes))
	at := time.Unix(100, 0)
	for _, c := range []struct {
		kind  models.Kind
		value string
	}{{models.HTTP2, "v1"}, {models.HTTP, "v1"}} {
		m := &models.Mock{Kind: c.kind, Spec: models.MockSpec{Metadata: map[string]string{"value": c.value}, ResTimestampMock: at}}
		info := &MockContext{Mock: m}
		if err := r.BeforeMockInsert(context.Background(), info); err != nil {
			t.Fatal(err)
		}
		if m.Spec.Async == nil || m.Spec.Async.Lane != "watch" || !m.Spec.Async.Poll {
			t.Fatalf("the lane's %s call was not stamped as the lane's poll: %+v", c.kind, m.Spec.Async)
		}
	}
}
