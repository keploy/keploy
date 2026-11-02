package docker

import (
	"context"
	"reflect"
	"strings"
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
	filters []string
}

// VolumeList matches its name filter anywhere in a name, as the engine does.
func (f *fakeVolumes) VolumeList(_ context.Context, o volume.ListOptions) (volume.ListResponse, error) {
	filter := o.Filters.Get("name")[0]
	f.filters = append(f.filters, filter)
	var out []*volume.Volume
	for _, n := range f.held {
		if strings.Contains(n, filter) {
			out = append(out, &volume.Volume{Name: n})
		}
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
// withRunVolumes names an embedder's run volumes for one test.
func withRunVolumes(t *testing.T, vs ...RunVolume) {
	prev := DockerConfig.RunVolumes
	DockerConfig.RunVolumes = vs
	t.Cleanup(func() { DockerConfig.RunVolumes = prev })
}

var socketsVolume = RunVolume{Base: "keploy-sockets-vol", Target: "/tmp"}

// The sweep takes the volumes earlier runs left, the CA's and an embedder's,
// that no container mounts: not a live run's, not a compose project's
// <project>_<base>, not an older keploy's bare base, not a user's volume that
// only starts with a base, and not a name that only contains one. Never
// forced: a forced remove is what would take a live run's.
func TestSweepRunVolumesTakesOnlyEarlierRunsUnmountedVolumes(t *testing.T) {
	withRunVolumes(t, socketsVolume)
	f := &fakeVolumes{
		held: []string{
			"keploy-tls-certs-0badc0de0badc0de", // a killed run's
			"keploy-tls-certs-1111222233334444", // a live run's
			"shop_keploy-tls-certs",
			"keploy-tls-certs",
			"x-keploy-tls-certs-0badc0de0badc0de",
			"keploy-sockets-vol-0badc0de0badc0de", // a killed run's
			"keploy-sockets-vol-1111222233334444", // a live run's
			"keploy-sockets-vol",
			"shop_keploy-sockets-vol",
			"keploy-sockets-vol-data",              // the user's
			"keploy-sockets-vol-0badc0de",          // too short for a run's id
			"keploy-sockets-vol-0badc0de0badc0dee", // too long
			"keploy-sockets-vol-0BADC0DE0BADC0DE",  // not a run's id
			"keploy-sockets-vol-0badc0de0badc0dg",  // not hex
		},
		mounted: map[string]bool{"keploy-tls-certs-1111222233334444": true, "keploy-sockets-vol-1111222233334444": true},
	}
	SweepRunVolumes(context.Background(), zap.NewNop(), f)
	if want := []string{"keploy-tls-certs-", "keploy-sockets-vol-"}; !reflect.DeepEqual(f.filters, want) {
		t.Errorf("listed by %q, want each run volume's prefix %q", f.filters, want)
	}
	if want := []string{"keploy-tls-certs-0badc0de0badc0de", "keploy-sockets-vol-0badc0de0badc0de"}; !reflect.DeepEqual(f.removed, want) {
		t.Errorf("removed %v, want only the killed run's volumes %v", f.removed, want)
	}
	if f.forced {
		t.Error("a removal was forced")
	}
}

// A listing the engine refuses leaves that base's volumes, and the sweep
// goes on to the next base.
func TestSweepRunVolumesGoesOnPastAListingTheEngineRefuses(t *testing.T) {
	withRunVolumes(t, socketsVolume)
	f := &refusingList{fakeVolumes: fakeVolumes{held: []string{"keploy-sockets-vol-0badc0de0badc0de"}}, refuse: "keploy-tls-certs-"}
	SweepRunVolumes(context.Background(), zap.NewNop(), f)
	if want := []string{"keploy-sockets-vol-0badc0de0badc0de"}; !reflect.DeepEqual(f.removed, want) {
		t.Errorf("removed %v, want %v", f.removed, want)
	}
}

type refusingList struct {
	fakeVolumes
	refuse string
}

func (f *refusingList) VolumeList(ctx context.Context, o volume.ListOptions) (volume.ListResponse, error) {
	if o.Filters.Get("name")[0] == f.refuse {
		return volume.ListResponse{}, errInUse("list")
	}
	return f.fakeVolumes.VolumeList(ctx, o)
}

// At a run's end its own volumes go, the CA's and an embedder's, unforced.
func TestRemoveRunVolumesRemovesTheRunsVolumesUnforced(t *testing.T) {
	withRunVolumes(t, socketsVolume)
	f := &fakeVolumes{}
	RemoveRunVolumes(context.Background(), zap.NewNop(), f, "keploy-v3-0a1b2c3d4e5f6071")
	if want := []string{"keploy-tls-certs-0a1b2c3d4e5f6071", "keploy-sockets-vol-0a1b2c3d4e5f6071"}; !reflect.DeepEqual(f.removed, want) || f.forced {
		t.Fatalf("removed %v (forced %v), want %v, unforced", f.removed, f.forced, want)
	}
}

// An application's command is given the run's own embedder volume. One that
// mounts the volume by the bare name every run shared, as a script written
// for an older keploy does, has that mount renamed to the run's, and is told
// so; any other mount is the command's own and is left as it is.
func TestRunVolumesForAppGivesTheRunsOwnAndRenamesTheSharedName(t *testing.T) {
	const agent, own = "keploy-v3-0a1b2c3d4e5f6071", "keploy-sockets-vol-0a1b2c3d4e5f6071"
	withRunVolumes(t, socketsVolume)
	for _, c := range []struct{ cmd, out string }{
		{"docker run -v keploy-sockets-vol:/tmp app", "docker run -v " + own + ":/tmp app"},
		{"docker run --volume keploy-sockets-vol:/tmp:rw app", "docker run --volume " + own + ":/tmp:rw app"},
		{"docker run --volume=keploy-sockets-vol:/tmp app", "docker run --volume=" + own + ":/tmp app"},
		{"docker run -v 'keploy-sockets-vol:/tmp' app", "docker run -v '" + own + ":/tmp' app"},
		{"docker run --mount type=volume,source=keploy-sockets-vol,target=/tmp app", "docker run --mount type=volume,source=" + own + ",target=/tmp app"},
		{"docker run --mount=src=keploy-sockets-vol,dst=/tmp,readonly=false app", "docker run --mount=src=" + own + ",dst=/tmp,readonly=false app"},
		{"docker run --mount destination=/tmp,source=keploy-sockets-vol app", "docker run --mount destination=/tmp,source=" + own + " app"},
	} {
		out, flags, renamed := RunVolumesForApp(c.cmd, agent)
		if out != c.out || flags != "" || !reflect.DeepEqual(renamed, []string{"keploy-sockets-vol"}) {
			t.Errorf("%q: got %q, flags %q, renamed %v; want %q renamed, no flags", c.cmd, out, flags, renamed, c.out)
		}
	}
	for _, cmd := range []string{
		"docker run app",
		"docker run -v keploy-sockets-vol:/tmpdir app",
		"docker run -v keploy-sockets-vol2:/tmp app",
		"docker run -v /srv/keploy-sockets-vol:/tmp app",
		"docker run -v x-keploy-sockets-vol:/tmp app",
		"docker run -e K=x-v keploy-sockets-vol:/tmp", // an image of that name
		"docker run --mount type=volume,source=keploy-sockets-vol,target=/data app",
		"docker run --mount type=volume,source=cache,target=/tmp app",
		"docker run --mount type=volume,target=/tmp app",
		"docker run --mount type=volume,source=keploy-sockets-vol app",
	} {
		out, flags, renamed := RunVolumesForApp(cmd, agent)
		if out != cmd || flags != "-v "+own+":/tmp " || renamed != nil {
			t.Errorf("%q: got %q, flags %q, renamed %v; want it unchanged and the run's own mount in the flags", cmd, out, flags, renamed)
		}
	}
	// A volume at another target: its own target, both for the flag and for
	// the mount renamed.
	const embedOwn = "keploy-embed-vol-0a1b2c3d4e5f6071"
	withRunVolumes(t, RunVolume{Base: "keploy-embed-vol", Target: "/srv/keploy-embed"})
	if out, flags, renamed := RunVolumesForApp("docker run app", agent); out != "docker run app" || flags != "-v "+embedOwn+":/srv/keploy-embed " || renamed != nil {
		t.Errorf("a volume at /srv/keploy-embed: got %q, flags %q, renamed %v; want the run's own mount at /srv/keploy-embed in the flags", out, flags, renamed)
	}
	if out, flags, renamed := RunVolumesForApp("docker run -v keploy-embed-vol:/srv/keploy-embed app", agent); out != "docker run -v "+embedOwn+":/srv/keploy-embed app" || flags != "" || !reflect.DeepEqual(renamed, []string{"keploy-embed-vol"}) {
		t.Errorf("a volume at /srv/keploy-embed mounted bare: got %q, flags %q, renamed %v; want it renamed, no flags", out, flags, renamed)
	}
	if out, flags, renamed := RunVolumesForApp("docker run --mount type=volume,source=keploy-embed-vol,target=/srv/keploy-embed app", agent); out != "docker run --mount type=volume,source="+embedOwn+",target=/srv/keploy-embed app" || flags != "" || !reflect.DeepEqual(renamed, []string{"keploy-embed-vol"}) {
		t.Errorf("a volume at /srv/keploy-embed mounted bare with --mount: got %q, flags %q, renamed %v; want it renamed, no flags", out, flags, renamed)
	}
	if out, flags, renamed := RunVolumesForApp("docker run -v keploy-embed-vol:/tmp app", agent); out != "docker run -v keploy-embed-vol:/tmp app" || flags != "-v "+embedOwn+":/srv/keploy-embed " || renamed != nil {
		t.Errorf("a volume at /srv/keploy-embed mounted bare at /tmp: got %q, flags %q, renamed %v; want it left and the run's own mount in the flags", out, flags, renamed)
	}
	withRunVolumes(t)
	if out, flags, renamed := RunVolumesForApp("docker run -v keploy-sockets-vol:/tmp app", agent); out != "docker run -v keploy-sockets-vol:/tmp app" || flags != "" || renamed != nil {
		t.Errorf("with no run volumes named: got %q, %q, %v; want the command as it is", out, flags, renamed)
	}
}
