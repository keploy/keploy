package proxy

import (
	"context"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// IncomingOptions tells a capture path other than this manager's own what the
// recorder started ingress with: nothing before Start, then each Start's
// options (a reconnecting recorder's replace the earlier ones). An enterprise
// observe-only capture filters with them; without this it could not see them
// and recorded what the recording's filters exclude.
func TestIncomingOptionsAreWhatTheRecorderStartedIngressWith(t *testing.T) {
	pm := New(zap.NewNop(), nil, &config.Config{})
	pm.startOnce.Do(func() {}) // no watcher goroutines: only the options matter here

	if got := pm.IncomingOptions(); !reflect.DeepEqual(got, models.IncomingOptions{}) {
		t.Fatalf("before Start: %+v, want zero options", got)
	}
	first := models.IncomingOptions{Filters: []models.Filter{{BypassRule: models.BypassRule{Path: "/gf-blocked"}}}}
	pm.Start(context.Background(), first)
	if got := pm.IncomingOptions(); !reflect.DeepEqual(got, first) {
		t.Fatalf("after Start: %+v, want %+v", got, first)
	}
	second := models.IncomingOptions{Filters: []models.Filter{{BypassRule: models.BypassRule{Path: "/health"}}}}
	pm.Start(context.Background(), second)
	if got := pm.IncomingOptions(); !reflect.DeepEqual(got, second) {
		t.Fatalf("after a second Start: %+v, want the newer %+v", got, second)
	}
}
