package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// relayingProxy is an outcomeProxy that counts the connections it relayed
// live (coreAgent.RelayedLiveReporter).
type relayingProxy struct {
	outcomeProxy
	relayed models.RelayedLives
}

func (p *relayingProxy) RelayedLive() models.RelayedLives { return p.relayed }

// The agent answers its proxy's count; a proxy that does not count is
// "unknown" (ErrRelayedLiveUnsupported), never "none".
func TestRelayedLiveIsTheProxysCountOrUnknown(t *testing.T) {
	rows := models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "10.0.0.5:6379", Connections: 2}}
	got, err := (&Agent{Proxy: &relayingProxy{relayed: rows}}).RelayedLive(context.Background())
	if err != nil || !reflect.DeepEqual(got, rows) {
		t.Fatalf("RelayedLive = %+v, %v; want %+v", got, err, rows)
	}
	for name, p := range map[string]coreAgent.Proxy{"a proxy that does not count": &outcomeProxy{}, "no proxy": nil} {
		if _, err := (&Agent{Proxy: p}).RelayedLive(context.Background()); !errors.Is(err, models.ErrRelayedLiveUnsupported) {
			t.Errorf("%s: err %v, want ErrRelayedLiveUnsupported", name, err)
		}
	}
}

// Under compose the account the stopping agent leaves carries what it
// relayed live, an empty list for none; an agent whose proxy does not count
// leaves the field out, which the CLI reads as "unknown".
func TestWriteStopOutcomeLeavesWhatTheProxyRelayed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proxy coreAgent.Proxy
		want  *models.RelayedLives
	}{
		{"Redis relayed", &relayingProxy{relayed: models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "redis:6379", Connections: 3}}},
			&models.RelayedLives{{Reason: models.RelayedRedisEnterpriseOnly, Destination: "redis:6379", Connections: 3}}},
		{"none relayed", &relayingProxy{relayed: models.RelayedLives{}}, &models.RelayedLives{}},
		{"a proxy that does not count", &outcomeProxy{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "outcome.json")
			if err := (&Agent{Proxy: tc.proxy, logger: zap.NewNop()}).writeStopOutcome(context.Background(), path); err != nil {
				t.Fatalf("writeStopOutcome: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got models.MockOutcome
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Relayed, tc.want) {
				t.Fatalf("left relayed %+v, want %+v\n%s", got.Relayed, tc.want, raw)
			}
		})
	}
}
