package docker

import (
	"context"
	"testing"

	"github.com/docker/docker/api/types/volume"
	"go.uber.org/zap"
)

// fakeVolumes is the engine's volume API: it lists what it holds, refuses to
// remove one a container mounts, and records each removal asked for.
type fakeVolumes struct {
	held    []string
	mounted map[string]bool
	removed []string
	forced  bool
	filter  string
}

func (f *fakeVolumes) VolumeList(_ context.Context, o volume.ListOptions) (volume.ListResponse, error) {
	f.filter = o.Filters.Get("name")[0]
	var out []*volume.Volume
	for _, n := range f.held {
		out = append(out, &volume.Volume{Name: n})
	}
	return volume.ListResponse{Volumes: out}, nil
}

func (f *fakeVolumes) VolumeRemove(_ context.Context, name string, force bool) error {
	f.forced = f.forced || force
	if f.mounted[name] {
		return errInUse(name)
	}
	f.removed = append(f.removed, name)
	return nil
}

type errInUse string

func (e errInUse) Error() string { return "remove " + string(e) + ": volume is in use" }

// Each docker-run run's agent writes its CA to a volume of its own, named
// from its agent container's id. One name for every run let a second run's
// agent overwrite the first's CA while the first ran.
func TestTLSVolumeForIsTheRunsOwn(t *testing.T) {
	a, b := TLSVolumeFor("keploy-v3-0a1b2c3d4e5f6071"), TLSVolumeFor("keploy-v3-8899aabbccddeeff")
	if a != "keploy-tls-certs-0a1b2c3d4e5f6071" || b != "keploy-tls-certs-8899aabbccddeeff" {
		t.Fatalf("got %q and %q, want keploy-tls-certs-<the agent container's id>", a, b)
	}
}

// The sweep takes the CA volumes earlier docker-run runs left and nothing
// else: not one a container still mounts (a live run's), not a compose
// project's <project>_keploy-tls-certs, not an older keploy's bare
// keploy-tls-certs, and not a name that only contains the prefix. Never
// forced: a forced remove is what would take a live run's.
func TestSweepTLSVolumesTakesOnlyEarlierRunsUnmountedVolumes(t *testing.T) {
	f := &fakeVolumes{
		held: []string{
			"keploy-tls-certs-0badc0de0badc0de", // a killed run's
			"keploy-tls-certs-1111222233334444", // a live run's
			"shop_keploy-tls-certs",
			"keploy-tls-certs",
			"x-keploy-tls-certs-0badc0de",
		},
		mounted: map[string]bool{"keploy-tls-certs-1111222233334444": true},
	}
	SweepTLSVolumes(context.Background(), zap.NewNop(), f)
	if f.filter != "keploy-tls-certs-" {
		t.Errorf("listed by %q, want the run volumes' prefix", f.filter)
	}
	if len(f.removed) != 1 || f.removed[0] != "keploy-tls-certs-0badc0de0badc0de" {
		t.Errorf("removed %v, want only the killed run's volume", f.removed)
	}
	if f.forced {
		t.Error("a removal was forced")
	}
}

// At a run's end its volume goes, unforced.
func TestRemoveTLSVolumeRemovesTheRunsVolumeUnforced(t *testing.T) {
	f := &fakeVolumes{}
	RemoveTLSVolume(context.Background(), zap.NewNop(), f, "keploy-tls-certs-0a1b2c3d4e5f6071")
	if len(f.removed) != 1 || f.removed[0] != "keploy-tls-certs-0a1b2c3d4e5f6071" || f.forced {
		t.Fatalf("removed %v (forced %v), want the run's volume, unforced", f.removed, f.forced)
	}
}
