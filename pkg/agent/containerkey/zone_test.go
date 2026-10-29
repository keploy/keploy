package containerkey

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/matcher/learn"
)

// zoneFile is one file of a fakeZoneAPI container: a link (link set), a
// regular file (data), or a directory (dir).
type zoneFile struct {
	data []byte
	link string
	dir  bool
}

// fakeZoneAPI is Docker's API over one container's files (files, by path),
// environment, entrypoint and command; gone says the container was removed.
type fakeZoneAPI struct {
	t          *testing.T
	env        []string
	entrypoint []string
	cmd        []string
	files      map[string]zoneFile
	gone       bool
	// vanish removes the container once it has been inspected; vanishAt,
	// once its file at that path has been statted twice.
	vanish   bool
	vanishAt string
	stats    map[string]int
	// swapped is the files an archive gives in place of the ones stat
	// reads: a file replaced between the two.
	swapped map[string]zoneFile
	// failAt is a path whose every read the daemon fails, with an error
	// that is no "not found".
	failAt   string
	inspects int
	copies   []string
}

func (f *fakeZoneAPI) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	if f.gone {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("No such container: app"))
	}
	if f.inspects++; f.vanish {
		f.gone = true
	}
	return container.InspectResponse{Config: &container.Config{Env: f.env, Entrypoint: f.entrypoint, Cmd: f.cmd}}, nil
}

// file is the file at p, a directory where p holds others.
func (f *fakeZoneAPI) file(p string) (zoneFile, error) {
	if f.gone {
		return zoneFile{}, errdefs.NotFound(errors.New("No such container: app"))
	}
	if p == f.failAt {
		return zoneFile{}, errors.New("the daemon is busy")
	}
	if z, ok := f.files[p]; ok {
		return z, nil
	}
	for k := range f.files {
		if p == "/" || strings.HasPrefix(k, p+"/") {
			return zoneFile{dir: true}, nil
		}
	}
	return zoneFile{}, errdefs.NotFound(errors.New("Could not find the file " + p + " in container app"))
}

func (z zoneFile) mode() fs.FileMode {
	switch {
	case z.link != "":
		return fs.ModeSymlink | 0o777
	case z.dir:
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (f *fakeZoneAPI) ContainerStatPath(_ context.Context, _, p string) (container.PathStat, error) {
	z, err := f.file(p)
	if f.stats == nil {
		f.stats = map[string]int{}
	}
	if f.stats[p]++; p == f.vanishAt && f.stats[p] == 2 {
		f.gone = true
	}
	if err != nil {
		return container.PathStat{}, err
	}
	return container.PathStat{Name: p, Mode: z.mode(), Size: int64(len(z.data))}, nil
}

func (f *fakeZoneAPI) CopyFromContainer(_ context.Context, _, p string) (io.ReadCloser, container.PathStat, error) {
	z, err := f.file(p)
	if err != nil {
		return nil, container.PathStat{}, err
	}
	if s, ok := f.swapped[p]; ok {
		z = s
	}
	if z.dir {
		f.t.Errorf("keploy asked Docker for an archive of the directory %s, which holds all its files", p)
	}
	f.copies = append(f.copies, p)
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: p[strings.LastIndex(p, "/")+1:], Mode: int64(z.mode().Perm())}
	switch {
	case z.link != "":
		hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, z.link
	case z.dir:
		hdr.Typeflag = tar.TypeDir
	default:
		hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(z.data))
	}
	if err := w.WriteHeader(hdr); err != nil {
		f.t.Fatal(err)
	}
	if _, err := w.Write(z.data); err != nil {
		f.t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		f.t.Fatal(err)
	}
	return io.NopCloser(&buf), container.PathStat{Name: p, Mode: z.mode()}, nil
}

func zoneData(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("/usr/share/zoneinfo/" + name)
	if err != nil {
		t.Skipf("this host has no zone file of %s: %v", name, err)
	}
	return b
}

// TestAContainersZoneIsReadFromItsEnvironmentAndFilesThroughDocker: the CLI
// reads an app container's zone as the agent reads a process's (TZ, else
// /etc/localtime, linked or copied), and reads none where the container's
// configuration leaves it unknown (a JVM's option, an /etc/timezone of
// another zone, a link to nothing, a container gone).
func TestAContainersZoneIsReadFromItsEnvironmentAndFilesThroughDocker(t *testing.T) {
	kolkata, berlin := zoneData(t, "Asia/Kolkata"), zoneData(t, "Europe/Berlin")
	zi := "/usr/share/zoneinfo/"
	both := func(extra map[string]zoneFile) map[string]zoneFile {
		m := map[string]zoneFile{zi + "Asia/Kolkata": {data: kolkata}, zi + "Europe/Berlin": {data: berlin}, "/bin/sh": {data: []byte("x")}, "/etc/hostname": {data: []byte("app\n")}}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	at := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	rows := []struct {
		name       string
		env        []string
		entrypoint []string
		cmd        []string
		files      map[string]zoneFile
		gone       bool
		vanish     bool
		vanishAt   string
		swapped    map[string]zoneFile
		failAt     string
		want       string // the zone's name; "" with offset set for an unnamed one
		offset     int    // seconds east of UTC at `at`
		err        string // a part of the error, where the zone is unknown
	}{
		{name: "TZ in the environment wins over /etc/localtime", env: []string{"PATH=/bin", "TZ=Asia/Kolkata"}, files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}}), want: "Asia/Kolkata", offset: 19800},
		{name: "the first TZ, as getenv reads it", env: []string{"TZ=Europe/Berlin", "TZ=Asia/Kolkata"}, files: both(nil), want: "Europe/Berlin", offset: 7200},
		{name: "an /etc/localtime linking into the zoneinfo directory names its zone", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}}), want: "Europe/Berlin", offset: 7200},
		{name: "a relative /etc/localtime link, read in the container's view", files: both(map[string]zoneFile{"/etc/localtime": {link: "../usr/share/zoneinfo/Asia/Kolkata"}}), want: "Asia/Kolkata", offset: 19800},
		{name: "an /etc/localtime link through a linked directory", files: both(map[string]zoneFile{"/etc/localtime": {link: "/zi/Asia/Kolkata"}, "/zi": {link: zi[:len(zi)-1]}}), want: "Asia/Kolkata", offset: 19800},
		{name: "a copied /etc/localtime is named by the /etc/timezone whose zone file it matches", files: both(map[string]zoneFile{"/etc/localtime": {data: kolkata}, "/etc/timezone": {data: []byte("Asia/Kolkata\n")}}), want: "Asia/Kolkata", offset: 19800},
		{name: "a copied /etc/localtime with no /etc/timezone is its rules, unnamed", files: both(map[string]zoneFile{"/etc/localtime": {data: kolkata}}), offset: 19800},
		{name: "no /etc/localtime at all (an Alpine or distroless image) is UTC", files: both(nil), want: "UTC"},
		{name: "a TZ naming a zone the container holds no file of is unknown", env: []string{"TZ=Asia/Tokyo"}, files: both(nil), err: "names no zone file"},
		{name: "a TZ of a POSIX rule is unknown", env: []string{"TZ=IST-5:30"}, files: both(nil), err: "IST-5:30"},
		{name: "an /etc/localtime linking to nothing is unknown", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Asia/Tokyo"}}), err: "file does not exist"},
		{name: "a host's /etc/localtime bind-mounted over a link to Etc/UTC (the daemon reads the mounted bytes at the file the link names) is unknown", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Etc/UTC"}, zi + "Etc/UTC": {data: kolkata}, "/etc/timezone": {data: []byte("Etc/UTC\n")}}), err: `the zone file named "Etc/UTC" (by TZ, the /etc/localtime link or /etc/timezone) does not hold "Etc/UTC"'s rules`},
		{name: "TZ=UTC over that mount, whose file Debian's glibc reads for it, is unknown", env: []string{"TZ=UTC"}, files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Etc/UTC"}, zi + "Etc/UTC": {data: kolkata}, zi + "UTC": {link: "Etc/UTC"}}), err: "read the zone file /usr/share/zoneinfo/UTC, which holds other rules than UTC's"},
		{name: "a JVM option variable setting user.timezone is unknown", env: []string{"JAVA_TOOL_OPTIONS=-Xmx1g -Duser.timezone=Asia/Kolkata"}, files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Asia/Kolkata"}}), err: "JAVA_TOOL_OPTIONS"},
		{name: "JDK_JAVA_OPTIONS setting user.timezone is unknown", env: []string{"TZ=Asia/Kolkata", "JDK_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, files: both(nil), err: "the container's JDK_JAVA_OPTIONS holds -Duser.timezone="},
		{name: "_JAVA_OPTIONS setting user.timezone is unknown", env: []string{"_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, files: both(nil), err: "the container's _JAVA_OPTIONS holds"},
		{name: "OPENJ9_JAVA_OPTIONS setting user.timezone is unknown", env: []string{"OPENJ9_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, files: both(nil), err: "the container's OPENJ9_JAVA_OPTIONS holds"},
		{name: "IBM_JAVA_OPTIONS setting user.timezone is unknown", env: []string{"IBM_JAVA_OPTIONS=-Duser.timezone=Europe/Berlin"}, files: both(nil), err: "the container's IBM_JAVA_OPTIONS holds"},
		{name: "JAVA_OPTS, which a script hands java, setting user.timezone is unknown", env: []string{"TZ=Asia/Kolkata", "JAVA_OPTS=-Xmx512m -Duser.timezone=Europe/Berlin"}, cmd: []string{"sh", "-c", "exec java $JAVA_OPTS -jar /app.jar"}, files: both(nil), err: "the container's JAVA_OPTS holds"},
		{name: "CATALINA_OPTS setting user.timezone is unknown", env: []string{"CATALINA_OPTS=-Duser.timezone=Europe/Berlin"}, files: both(nil), err: "the container's CATALINA_OPTS holds"},
		{name: "a variable of any name holding user.timezone (JVM_ARGS) is unknown", env: []string{"TZ=Asia/Kolkata", "JVM_ARGS=-Xmx1g -Duser.timezone=Europe/Berlin"}, files: both(nil), err: "the container's JVM_ARGS holds"},
		{name: "a variable holding no user.timezone option is not a JVM's zone", env: []string{"TZ=Asia/Kolkata", "JAVA_OPTS=-Xmx512m -Duser.language=en"}, files: both(nil), want: "Asia/Kolkata", offset: 19800},
		{name: "a java command line setting user.timezone is unknown", cmd: []string{"java", "-Duser.timezone=Asia/Kolkata", "-jar", "app.jar"}, files: both(nil), err: "command"},
		{name: "an ENTRYPOINT java command line setting user.timezone is unknown", entrypoint: []string{"java", "-Duser.timezone=Asia/Kolkata", "-jar", "/app.jar"}, cmd: []string{"--server.port=8080"}, files: both(nil), err: "the container's command"},
		{name: "a shell-form CMD running java with user.timezone (stored as sh -c) is unknown", env: []string{"TZ=Asia/Kolkata"}, cmd: []string{"/bin/sh", "-c", "java -Duser.timezone=Europe/Berlin -jar /app.jar"}, files: both(nil), err: "the container's command"},
		{name: "a CMD java line setting user.timezone under an ENTRYPOINT (tini) is unknown", env: []string{"TZ=Asia/Kolkata"}, entrypoint: []string{"/usr/bin/tini", "--"}, cmd: []string{"java", "-Duser.timezone=Europe/Berlin", "-jar", "/app.jar"}, files: both(nil), err: "the container's command"},
		{name: "an /etc/timezone naming a zone other than /etc/localtime's is unknown", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}, "/etc/timezone": {data: []byte("Asia/Kolkata\n")}}), err: "/etc/timezone"},
		{name: "an /etc/timezone naming /etc/localtime's zone", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}, "/etc/timezone": {data: []byte("Europe/Berlin\n")}}), want: "Europe/Berlin", offset: 7200},
		{name: "an /etc/timezone of Etc/UTC with no /etc/localtime is UTC", files: both(map[string]zoneFile{"/etc/timezone": {data: []byte("Etc/UTC\n")}}), want: "UTC"},
		{name: "an /etc/timezone that is a directory (a bind of a missing host file) is none", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}, "/etc/timezone/x": {data: []byte("y")}}), want: "Europe/Berlin", offset: 7200},
		{name: "an empty TZ with files of a zone other than UTC is unknown", env: []string{"TZ="}, files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}}), err: "TZ is empty"},
		{name: "an empty TZ with no /etc/localtime is UTC", env: []string{"TZ="}, files: both(nil), want: "UTC"},
		{name: "an empty TZ over a copied UTC /etc/localtime of no name (busybox) is UTC", env: []string{"TZ="}, files: both(map[string]zoneFile{"/etc/localtime": {data: zoneData(t, "UTC")}}), want: "UTC"},
		{name: "an empty TZ over a copied /etc/localtime of no name, not UTC's, is unknown", env: []string{"TZ="}, files: both(map[string]zoneFile{"/etc/localtime": {data: berlin}}), err: "TZ is empty"},
		{name: "an empty TZ whose /etc/localtime the daemon fails to read is unknown, never UTC", env: []string{"TZ="}, files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}}), failAt: "/etc/localtime", err: "whose read failed (keploy cannot read /etc/localtime in the container: the daemon is busy)"},
		{name: "an empty TZ over a copied zone of no name, at UTC only now (Africa/Abidjan's), is unknown", env: []string{"TZ="}, files: both(map[string]zoneFile{"/etc/localtime": {data: zoneData(t, "Africa/Abidjan")}}), err: "TZ is empty"},
		{name: "a daemon that fails only the read of /etc/localtime leaves the zone unknown, never UTC", files: both(nil), failAt: "/etc/localtime", err: "the daemon is busy"},
		{name: "a daemon that fails only the read of a linked /etc/localtime leaves the zone unknown", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}}), failAt: "/etc/localtime", err: "the daemon is busy"},
		{name: "a container removed as keploy reads it is unknown, never a missing /etc/localtime", vanish: true, files: both(nil), err: "does not exist"},
		{name: "a container removed as keploy reads its missing /etc/localtime is unknown, never UTC", vanishAt: "/etc/localtime", files: both(nil), err: "does not exist"},
		{name: "an /etc/localtime link replaced by a file as keploy reads it is unknown", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}}), swapped: map[string]zoneFile{"/etc/localtime": {data: kolkata}}, err: "is no link"},
		{name: "an /etc/timezone replaced by a link as keploy reads it is unknown, never none", files: both(map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}, "/etc/timezone": {data: []byte("Asia/Kolkata\n")}}), swapped: map[string]zoneFile{"/etc/timezone": {link: "/x"}}, err: "/etc/timezone"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			api := &fakeZoneAPI{t: t, env: r.env, entrypoint: r.entrypoint, cmd: r.cmd, files: r.files, gone: r.gone, vanish: r.vanish, vanishAt: r.vanishAt, swapped: r.swapped, failAt: r.failAt}
			z, err := zoneOfApp(t, api)
			if r.err != "" {
				if err == nil {
					t.Fatalf("containerZone read %q (%v), want unknown with an error saying %q", z.Name, z.Loc, r.err)
				}
				if !strings.Contains(err.Error(), r.err) {
					t.Fatalf("containerZone's error %q does not say %q", err, r.err)
				}
				if z.Loc != nil {
					t.Fatalf("containerZone gave zone %v with its error", z.Loc)
				}
				return
			}
			if err != nil {
				t.Fatalf("containerZone: %v", err)
			}
			if z.Loc == nil {
				t.Fatal("containerZone gave no zone and no error")
			}
			if got := z.Loc.String(); got != r.want {
				t.Fatalf("containerZone read zone %q, want %q", got, r.want)
			}
			if _, off := at.In(z.Loc).Zone(); off != r.offset {
				t.Fatalf("containerZone's zone is %d s east of UTC on %s, want %d", off, at.Format(time.DateOnly), r.offset)
			}
		})
	}
}

// zoneOfApp is the zone of the fake's container as the agent takes it: the
// index's read inspects the container, reads its Zone (readZone) and hands
// it on, and the agent reads that (learn.ZoneOfFile). The zone read is
// containerZone's own, whose bytes give the agent the same zone.
func zoneOfApp(t *testing.T, api zoneAPI) (learn.ProcZone, error) {
	t.Helper()
	ctx := context.Background()
	got, err := api.ContainerInspect(ctx, "app")
	if err != nil {
		return learn.ProcZone{}, err
	}
	// containerZone's own result is the row's: the read of the Zone after
	// it only checks the two agree (it fails where the first read gave a
	// zone and a fake's container is gone by the second, which a row must
	// not take for the unknown zone it wants).
	z, _, zerr := containerZone(ctx, api, "app", got.Config)
	handed := readZone(ctx, api, "app", got.Config)
	if zerr != nil {
		if z.Loc != nil {
			t.Fatalf("containerZone gave zone %v with its error %v", z.Loc, zerr)
		}
		if handed.Err == "" || handed.Data != nil || handed.Name != "" {
			t.Fatalf("containerZone failed (%v), but its Zone is %+v", zerr, handed)
		}
		return learn.ProcZone{}, zerr
	}
	if handed.Err != "" {
		t.Fatalf("containerZone read %v, but its Zone says %s", z.Loc, handed.Err)
	}
	b, err := json.Marshal(handed)
	if err != nil {
		return learn.ProcZone{}, err
	}
	var took Zone
	if err := json.Unmarshal(b, &took); err != nil {
		return learn.ProcZone{}, err
	}
	agent, err := learn.ZoneOfFile(took.Name, took.Data)
	if err != nil {
		return learn.ProcZone{}, fmt.Errorf("the agent cannot take the zone %v containerZone read: %w", z.Loc, err)
	}
	if agent.Loc.String() != z.Loc.String() || agent.Name != z.Name {
		return learn.ProcZone{}, fmt.Errorf("the agent took zone %q (%q), containerZone read %q (%q)", agent.Loc, agent.Name, z.Loc, z.Name)
	}
	for _, at := range []time.Time{time.Date(1990, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)} {
		_, a := at.In(agent.Loc).Zone()
		if _, c := at.In(z.Loc).Zone(); a != c {
			return learn.ProcZone{}, fmt.Errorf("the agent's zone is %d s east of UTC on %s, the container's %d", a, at.Format(time.DateOnly), c)
		}
	}
	return agent, nil
}

// TestAContainersZoneReadGivesUpOnAFileItCannotRead: a Docker error other
// than a missing path (a daemon that fails the read) leaves the zone
// unknown, never read as a missing /etc/localtime (UTC).
func TestAContainersZoneReadGivesUpOnAFileItCannotRead(t *testing.T) {
	api := &failingStatAPI{fakeZoneAPI{t: t, files: map[string]zoneFile{"/bin/sh": {data: []byte("x")}}}}
	z, err := zoneOfApp(t, api)
	if err == nil {
		t.Fatalf("containerZone read %v where Docker failed every read of the container's files, want unknown", z.Loc)
	}
	// Docker gave no configuration: no file is read, the zone unknown.
	files := &fakeZoneAPI{t: t, files: map[string]zoneFile{"/bin/sh": {data: []byte("x")}}}
	if z, _, err := containerZone(t.Context(), files, "app", nil); err == nil || z.Loc != nil || len(files.copies) != 0 || len(files.stats) != 0 {
		t.Fatalf("a container of no configuration read as %v (%v), its files read %v %v; want unknown, nothing read", z.Loc, err, files.stats, files.copies)
	}
}

type failingStatAPI struct{ fakeZoneAPI }

func (f *failingStatAPI) ContainerStatPath(context.Context, string, string) (container.PathStat, error) {
	return container.PathStat{}, errors.New("the daemon is busy")
}
