package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// silentVolumes is a daemon that never answers a volume's removal: it
// returns only once the question's context has ended.
type silentVolumes struct{ fakeVolumes }

func (s *silentVolumes) VolumeRemove(ctx context.Context, _ string, _ bool) error {
	<-ctx.Done()
	return ctx.Err()
}

// A run's volume the daemon did not remove before keploy's bound on its
// answer ended is said at WARN, by name, with the command that removes it:
// nothing else removes it until a later docker-mode run. One the engine
// refused because a container still mounts it stays at DEBUG: a later run's
// sweep takes it once that container is gone.
func TestRemoveRunVolumesSaysAVolumeTheBoundLeft(t *testing.T) {
	withRunVolumes(t, socketsVolume)
	const agent = "keploy-v3-0a1b2c3d4e5f6071"
	silentDaemon := errors.New("no answer from the daemon")
	for _, tc := range []struct {
		name   string
		api    VolumeRemover
		ended  bool
		warned []string
	}{
		{name: "the daemon never answered within the bound", api: &silentVolumes{}, ended: true,
			warned: []string{"keploy-tls-certs-0a1b2c3d4e5f6071", "keploy-sockets-vol-0a1b2c3d4e5f6071"}},
		{name: "a container still mounts it", api: &fakeVolumes{mounted: map[string]bool{"keploy-sockets-vol-0a1b2c3d4e5f6071": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.DebugLevel)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			if tc.ended {
				cancel(silentDaemon)
			}
			RemoveRunVolumes(ctx, zap.New(core), tc.api, agent)
			var warned []string
			for _, e := range logs.All() {
				if e.Level != zapcore.WarnLevel {
					continue
				}
				name, _ := e.ContextMap()["volume"].(string)
				if want := "volume rm " + name; !strings.HasSuffix(e.Message, want) {
					t.Errorf("WARN %q does not end with the command that removes it (%q)", e.Message, want)
				}
				if cause, _ := e.ContextMap()["cause"].(string); cause != silentDaemon.Error() {
					t.Errorf("WARN for %s gives cause %q, want %q", name, cause, silentDaemon)
				}
				warned = append(warned, name)
			}
			if strings.Join(warned, ",") != strings.Join(tc.warned, ",") {
				t.Fatalf("said at WARN %v, want %v", warned, tc.warned)
			}
		})
	}
}
