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
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// zoneFile is one file of a fakeZoneAPI container: a link (link set), a
// regular file (data), or a directory (dir).
type zoneFile struct {
	data []byte
	link string
	dir  bool
	// hardlink is the path of the file this one is a hard link of: its
	// own archive gives its bytes, a directory's archive an entry naming
	// that file (tar.TypeLink), as Docker's does.
	hardlink string
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
	// raw is the archive the daemon gives of a path in place of one of
	// its file (a container's archive is the container's to shape), and
	// rawRead how many of its bytes keploy read.
	raw     map[string][]byte
	rawRead map[string]*int
	// mounts is what Docker's inspect lists of the container's mounts,
	// and dirCopies how many archives of its zoneinfo directory keploy
	// asked for.
	mounts    []container.MountPoint
	dirCopies int
}

// countedReader counts in *n the bytes read from r.
type countedReader struct {
	r io.Reader
	n *int
}

func (c countedReader) Read(b []byte) (int, error) {
	k, err := c.r.Read(b)
	*c.n += k
	return k, err
}

func (f *fakeZoneAPI) ContainerInspect(context.Context, string) (container.InspectResponse, error) {
	if f.gone {
		return container.InspectResponse{}, errdefs.NotFound(errors.New("No such container: app"))
	}
	if f.inspects++; f.vanish {
		f.gone = true
	}
	return container.InspectResponse{Config: &container.Config{Env: f.env, Entrypoint: f.entrypoint, Cmd: f.cmd}, Mounts: f.mounts}, nil
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

// dirArchive is the archive Docker gives of the directory dir: its own
// entry, then each file and link below it, named from dir's last name.
func (f *fakeZoneAPI) dirArchive(dir string) io.Reader {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	base := path.Base(dir)
	if err := w.WriteHeader(&tar.Header{Name: base + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for p := range f.files {
		if strings.HasPrefix(p, dir+"/") {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	for _, p := range names {
		z := f.files[p]
		hdr := &tar.Header{Name: base + "/" + strings.TrimPrefix(p, dir+"/"), Mode: int64(z.mode().Perm())}
		switch {
		case z.hardlink != "":
			hdr.Typeflag, hdr.Linkname = tar.TypeLink, base+"/"+strings.TrimPrefix(z.hardlink, dir+"/")
			if err := w.WriteHeader(hdr); err != nil {
				f.t.Fatal(err)
			}
			continue
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
	}
	if err := w.Close(); err != nil {
		f.t.Fatal(err)
	}
	return &buf
}

func (f *fakeZoneAPI) CopyFromContainer(_ context.Context, _, p string) (io.ReadCloser, container.PathStat, error) {
	z, err := f.file(p)
	if err != nil {
		return nil, container.PathStat{}, err
	}
	if s, ok := f.swapped[p]; ok {
		z = s
	}
	if z.dir && p == "/usr/share/zoneinfo" {
		// The zoneinfo directory alone, read whole for a copy of
		// another zone's file (SameZoneFiles).
		f.dirCopies++
		return io.NopCloser(f.dirArchive(p)), container.PathStat{Name: p, Mode: z.mode()}, nil
	}
	if z.dir {
		f.t.Errorf("keploy asked Docker for an archive of the directory %s, which holds all its files", p)
	}
	f.copies = append(f.copies, p)
	if r, ok := f.raw[p]; ok {
		if f.rawRead == nil {
			f.rawRead = map[string]*int{}
		}
		n := new(int)
		f.rawRead[p] = n
		return io.NopCloser(countedReader{bytes.NewReader(r), n}), container.PathStat{Name: p, Mode: z.mode()}, nil
	}
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
	agent, err := learn.ZoneOfFile(took.Name, learn.ZoneFile{Data: took.Data, Source: took.Source})
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

// archiveEntry is one entry of an archive a row hands keploy as Docker's.
type archiveEntry struct {
	name, link string
	typ        byte
	data       []byte
}

// archiveOf is an archive of entries, as a container's daemon could give it.
func archiveOf(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := w.WriteHeader(&tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Size: int64(len(e.data)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestAContainersArchiveIsReadAsTheOneFileAsked: Docker's archive of a
// container's file is the container's to shape, so keploy reads only one
// of the shape Docker gives of that one file, into memory: a single entry
// of the type asked (a regular file; a link, for its target), named the
// file's last name, a regular file of at most learn.ZoneFileCap bytes,
// refused before any is read. Every other is refused, naming why: an entry
// named to leave the archive ("../localtime") or absolutely, a link or a
// hard link in a regular file's place, an oversized file, a second entry.
// No entry's name is a path on this host: the directory the rows run in
// holds nothing new after each. A link's target is the container's, given
// as it is ("../usr/share/zoneinfo/...", which learn resolves in the
// container's own view).
func TestAContainersArchiveIsReadAsTheOneFileAsked(t *testing.T) {
	kolkata := zoneData(t, "Asia/Kolkata")
	top := t.TempDir()
	cwd := top + "/a/b"
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	const lt = "/etc/localtime"
	big := make([]byte, learn.ZoneFileCap+1)
	rows := []struct {
		name    string
		link    bool // the row reads lt's link (Readlink), else lt's bytes
		entries []archiveEntry
		want    string // the error's text; "" for a read
	}{
		{name: "a regular file of its name", entries: []archiveEntry{{name: "localtime", typ: tar.TypeReg, data: kolkata}}},
		{name: "a regular file of its name, cleaned", entries: []archiveEntry{{name: "./localtime", typ: tar.TypeReg, data: kolkata}}},
		{name: "an entry named to leave the archive", entries: []archiveEntry{{name: "../localtime", typ: tar.TypeReg, data: kolkata}}, want: `holds "../localtime", not "localtime"`},
		{name: "an entry of an absolute name", entries: []archiveEntry{{name: "/etc/localtime", typ: tar.TypeReg, data: kolkata}}, want: `holds "/etc/localtime", not "localtime"`},
		{name: "another file's entry", entries: []archiveEntry{{name: "shadow", typ: tar.TypeReg, data: kolkata}}, want: `holds "shadow", not "localtime"`},
		{name: "a link in a regular file's place", entries: []archiveEntry{{name: "localtime", typ: tar.TypeSymlink, link: "/etc/shadow"}}, want: "no regular file (L"},
		{name: "a hard link in a regular file's place", entries: []archiveEntry{{name: "localtime", typ: tar.TypeLink, link: "../../etc/passwd"}}, want: `holds a hard link to "../../etc/passwd" in its place`},
		{name: "a file over the cap", entries: []archiveEntry{{name: "localtime", typ: tar.TypeReg, data: big}}, want: fmt.Sprintf("is %d bytes, more than the %d keploy reads", len(big), learn.ZoneFileCap)},
		{name: "a second entry", entries: []archiveEntry{{name: "localtime", typ: tar.TypeReg, data: kolkata}, {name: "../evil", typ: tar.TypeReg, data: []byte("x")}}, want: `holds "../evil" too`},
		{name: "a link of its name", link: true, entries: []archiveEntry{{name: "localtime", typ: tar.TypeSymlink, link: "../usr/share/zoneinfo/Asia/Kolkata"}}},
		{name: "a link named to leave the archive", link: true, entries: []archiveEntry{{name: "../localtime", typ: tar.TypeSymlink, link: "/usr/share/zoneinfo/Asia/Kolkata"}}, want: `holds "../localtime", not "localtime"`},
		{name: "a link and a second entry", link: true, entries: []archiveEntry{{name: "localtime", typ: tar.TypeSymlink, link: "/usr/share/zoneinfo/Asia/Kolkata"}, {name: "../evil", typ: tar.TypeSymlink, link: "/"}}, want: `holds "../evil" too`},
		{name: "a regular file where a link is read", link: true, entries: []archiveEntry{{name: "localtime", typ: tar.TypeReg, data: kolkata}}, want: "is no link"},
		{name: "a hard link where a link is read", link: true, entries: []archiveEntry{{name: "localtime", typ: tar.TypeLink, link: "/etc/shadow"}}, want: "is no link"},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := zoneFile{data: kolkata}
			if r.link {
				f = zoneFile{link: "/usr/share/zoneinfo/Asia/Kolkata"}
			}
			api := &fakeZoneAPI{t: t, files: map[string]zoneFile{lt: f}, raw: map[string][]byte{lt: archiveOf(t, r.entries...)}}
			c := containerFiles{ctx: t.Context(), api: api, id: "app"}
			var got string
			var err error
			if r.link {
				got, err = c.Readlink(lt)
			} else {
				var rc io.ReadCloser
				if rc, err = c.OpenRegular(lt); err == nil {
					var b []byte
					b, err = io.ReadAll(rc)
					if cerr := rc.Close(); err == nil {
						err = cerr
					}
					got = string(b)
				}
			}
			switch {
			case r.want == "" && err != nil:
				t.Fatalf("read %s: %v; want it read", lt, err)
			case r.want == "" && r.link && got != r.entries[0].link:
				t.Fatalf("read %s's target as %q; want %q, as the link holds it", lt, got, r.entries[0].link)
			case r.want == "" && !r.link && got != string(kolkata):
				t.Fatalf("read %d bytes of %s; want Kolkata's %d", len(got), lt, len(kolkata))
			case r.want != "" && (err == nil || !strings.Contains(err.Error(), r.want)):
				t.Fatalf("read %s as %d bytes or %q (%v); want refused, saying %q", lt, len(got), got, err, r.want)
			}
			if r.want != "" && len(r.entries[0].data) > learn.ZoneFileCap {
				if n := *api.rawRead[lt]; n > learn.ZoneFileCap {
					t.Fatalf("keploy read %d bytes of an archive of a file over the cap; want it refused by its header, before its bytes", n)
				}
			}
			var left []string
			if err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if p != top && p != top+"/a" && p != cwd {
					left = append(left, p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(left) != 0 {
				t.Fatalf("reading the archive wrote %v on the host", left)
			}
		})
	}

	// Through the zone read: a zone file whose archive holds a second
	// entry leaves the zone unknown, saying why.
	api := &fakeZoneAPI{t: t, files: map[string]zoneFile{"/bin/sh": {data: []byte("x")}, lt: {link: "/usr/share/zoneinfo/Asia/Kolkata"}, "/usr/share/zoneinfo/Asia/Kolkata": {data: kolkata}},
		raw: map[string][]byte{"/usr/share/zoneinfo/Asia/Kolkata": archiveOf(t, archiveEntry{name: "Kolkata", typ: tar.TypeReg, data: kolkata}, archiveEntry{name: "../../../../tmp/x", typ: tar.TypeReg, data: []byte("x")})}}
	z, err := zoneOfApp(t, api)
	if err == nil || !strings.Contains(err.Error(), `holds "../../../../tmp/x" too`) {
		t.Fatalf("a zone file whose archive holds a second entry read as %v (%v); want unknown, saying why", z.Loc, err)
	}
}

// learnZone is learn's testdata zone file of name of IANA's tzdata release
// rel, copied from Go's zoneinfo.zip of that release (Go 1.17.13's for
// 2021a, Go 1.23.12's for 2024a, Go 1.27.3's for 2026c). Africa/Casablanca
// of 2024a and of 2026c read UTC+01:00 and UTC+00:00 from 2026-09-20 on.
func learnZone(t *testing.T, rel, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "matcher", "learn", "testdata", "zoneinfo-"+rel, filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A container's zone file is its zone's where a release of IANA's zone
// data reads its offsets so, whatever release the keploy CLI's own zone
// data is, and so is the agent's reading of the bytes the CLI hands it: an
// image's own Africa/Casablanca of the other release, linked or named by
// TZ; one in a host's zoneinfo directory mounted over the image's; the
// host's /etc/localtime, the other release's Africa/Casablanca, over the
// image's own; the host's /etc/localtime, the CLI's own Europe/Berlin, over
// the image's; a host's Asia/Calcutta (Asia/Kolkata's link) over the
// image's link to Asia/Kolkata; tzdata 2026c's WET, a hard link of its
// Europe/Lisbon, read by a CLI of 2024a's data. Another zone's file at a
// zone's path is unknown however it got there and whatever its bytes are of
// (the CLI reads the file's content, never the container's mounts): a
// Dockerfile's COPY of a file of Asia/Kolkata (Go's slim one, the image's
// own fat one elsewhere) or docker cp writing through the image's link to
// Europe/Berlin; tzdata 2024a's America/Phoenix over America/Denver; a copy
// of the image's own Africa/Casablanca, or of the other release's, written
// through its link to Europe/Berlin; Europe/Berlin made a hard link of the
// image's Asia/Kolkata; a host's file of another zone mounted over the link,
// from a zoneinfo directory or from a path that names no zone (the host's
// /etc/localtime); TZ=Europe/Berlin naming such a file. The refusal names
// the zone a release reads the file's offsets as, and never the host's
// path a mount reads it from. Each runs in a child process whose own zone
// data ($ZONEINFO) holds learn's testdata files of one release.
func TestAContainersZoneFileIsItsZonesWhereAReleaseReadsItSo(t *testing.T) {
	const run = "TestAContainersZoneFileIsItsZonesWhereAReleaseReadsItSo"
	own := os.Getenv("KEPLOY_TEST_ZONE_RELEASE")
	if own == "" {
		for _, rel := range []string{"2026c", "2024a"} {
			dir, err := filepath.Abs(filepath.Join("..", "..", "matcher", "learn", "testdata", "zoneinfo-"+rel))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+run+"$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), "KEPLOY_TEST_ZONE_RELEASE="+rel, "ZONEINFO="+dir)
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil || !bytes.Contains(out, []byte("--- PASS: "+run)) {
				t.Errorf("the CLI's own zone data of tzdata %s: %v\n%s", rel, err, out)
			}
		}
		return
	}
	rel := map[string]string{"2026c": "2024a", "2024a": "2026c"}[own]
	other, ours := learnZone(t, rel, "Africa/Casablanca"), learnZone(t, own, "Africa/Casablanca")
	berlin, fatKolkata, slimKolkata := zoneData(t, "Europe/Berlin"), zoneData(t, "Asia/Kolkata"), learnZone(t, "2026c", "Asia/Kolkata")
	laterBerlin := laterZone(t, berlin, time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), 3*3600)
	if bytes.Equal(fatKolkata, slimKolkata) {
		t.Fatal("precondition: the host's Asia/Kolkata is Go's own slim file")
	}
	offsets := map[string]int{"2024a": 3600, "2026c": 0}
	at := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	if loc, err := time.LoadLocation("Africa/Casablanca"); err != nil {
		t.Fatal(err)
	} else if _, off := at.In(loc).Zone(); off != offsets[own] {
		t.Fatalf("precondition: the CLI's own Africa/Casablanca reads %d s east of UTC on %s; want tzdata %s's", off, at.Format(time.DateOnly), own)
	}
	const zi = "/usr/share/zoneinfo/"
	casa := map[string]zoneFile{"/etc/localtime": {link: zi + "Africa/Casablanca"}, zi + "Africa/Casablanca": {data: other}}
	linked := func(name string, data []byte, more ...string) map[string]zoneFile {
		files := map[string]zoneFile{"/etc/localtime": {link: zi + name}, zi + name: {data: data}}
		for i := 0; i+1 < len(more); i += 2 {
			files[zi+more[i]] = zoneFile{data: []byte(more[i+1])}
		}
		return files
	}
	bind := func(src, dst string) []container.MountPoint {
		return []container.MountPoint{{Type: "bind", Source: src, Destination: dst}, {Type: "volume", Source: "/var/lib/docker/volumes/data/_data", Destination: "/data"}}
	}
	hardKolkata := map[string]zoneFile{"/etc/localtime": {link: zi + "Europe/Berlin"}, zi + "Asia/Kolkata": {data: fatKolkata}, zi + "Europe/Berlin": {data: fatKolkata, hardlink: zi + "Asia/Kolkata"}}
	kolkataIs := "one gives them Asia/Calcutta, Asia/Kolkata: it is that zone's file"
	rows := []struct {
		name   string
		env    []string
		files  map[string]zoneFile
		mounts []container.MountPoint
		want   string
		offset int
		err    string
	}{
		{name: "an image's own Africa/Casablanca of the other release", files: casa, want: "Africa/Casablanca", offset: offsets[rel]},
		{name: "TZ=Africa/Casablanca, the image's own of the other release", env: []string{"TZ=Africa/Casablanca"}, files: casa, want: "Africa/Casablanca", offset: offsets[rel]},
		{name: "a host's zoneinfo directory mounted over the image's, of the other release", files: casa, mounts: bind("/usr/share/zoneinfo", "/usr/share/zoneinfo"), want: "Africa/Casablanca", offset: offsets[rel]},
		{name: "the host's /etc/localtime, the other release's Africa/Casablanca, over the image's own", files: casa, mounts: bind("/etc/localtime", "/etc/localtime"), want: "Africa/Casablanca", offset: offsets[rel]},
		{name: "the host's /etc/localtime, the CLI's own Europe/Berlin, over the image's", files: linked("Europe/Berlin", berlin), mounts: bind("/etc/localtime", "/etc/localtime"), want: "Europe/Berlin", offset: 3600},
		{name: "a host's Asia/Calcutta over the image's link to Asia/Kolkata", files: linked("Asia/Kolkata", fatKolkata), mounts: bind("/home/alice/zoneinfo/Asia/Calcutta", "/etc/localtime"), want: "Asia/Kolkata", offset: 19800},
		{name: "a COPY of Go's slim Asia/Kolkata through the image's link to Europe/Berlin, no other zone file in the image", files: linked("Europe/Berlin", slimKolkata), err: kolkataIs},
		{name: "a COPY of Go's slim Asia/Kolkata through the image's link to Europe/Berlin, the image's own Asia/Kolkata fat", files: linked("Europe/Berlin", slimKolkata, "Asia/Kolkata", string(fatKolkata)), err: kolkataIs},
		{name: "TZ=Europe/Berlin, its file Go's slim Asia/Kolkata", env: []string{"TZ=Europe/Berlin"}, files: linked("Europe/Berlin", slimKolkata), err: kolkataIs},
		{name: "Europe/Berlin a hard link of the image's Asia/Kolkata", files: hardKolkata, err: kolkataIs},
		{name: "tzdata 2024a's America/Phoenix over the image's link to America/Denver", files: linked("America/Denver", learnZone(t, "2024a", "America/Phoenix")),
			err: `first differ on 1970-04-26T09:00:00Z, where it reads UTC-07:00 and "America/Denver" UTC-06:00`},
		{name: "a copy of the image's own Africa/Casablanca of the other release through its link to Europe/Berlin", files: linked("Europe/Berlin", other, "Africa/Casablanca", string(other)),
			err: "one gives them Africa/Casablanca: it is that zone's file"},
		{name: "a copy of the CLI's own release's Africa/Casablanca through the image's link to Europe/Berlin", files: linked("Europe/Berlin", ours), err: "one gives them Africa/Casablanca: it is that zone's file"},
		{name: "a host's Africa/Casablanca of the other release over the image's link to Europe/Berlin", files: linked("Europe/Berlin", other), mounts: bind("/home/alice/zoneinfo/Africa/Casablanca", "/etc/localtime"),
			err: "one gives them Africa/Casablanca: it is that zone's file"},
		{name: "the host's /etc/localtime, Asia/Kolkata's, over the image's link to Europe/Berlin", files: linked("Europe/Berlin", fatKolkata), mounts: bind("/home/alice/localtime", "/etc/localtime"), err: kolkataIs},
		// An image of a release later than keploy knows (2099a), its own
		// tzdata.zi beside its files: its Europe/Berlin at UTC+03:00 from
		// 2027 is Berlin's; where 2099a parts Europe/Oslo from Berlin so, a
		// COPY of the image's Oslo through its link to Berlin is Oslo's
		// file; with no tzdata.zi, nothing tells the file's release.
		{name: "an image of a later release, its Europe/Berlin UTC+03:00 from 2027, its tzdata.zi saying so",
			files: linked("Europe/Berlin", laterBerlin, "tzdata.zi", string(laterSource(t, "1 DE CE%sT 1980\n1 E CE%sT\n", "1 DE CE%sT 1980\n1 E CE%sT 2027\n3 - +03\n"))), want: "Europe/Berlin", offset: 3600},
		{name: "a COPY of an image's Europe/Oslo of a later release, which parts it from Berlin, through its link to Europe/Berlin",
			files: linked("Europe/Berlin", laterBerlin, "Europe/Oslo", string(laterBerlin), "tzdata.zi", string(laterSource(t, "L Europe/Berlin Europe/Oslo\n", "Z Europe/Oslo 1 - CET 1980\n1 E CE%sT 2027\n3 - +03\n"))),
			err:   "nor does the root's own zone data (tzdata.zi, release 2099a), and one gives them Europe/Oslo: it is that zone's file"},
		{name: "an image of a later release, its Europe/Berlin UTC+03:00 from 2027, no tzdata.zi", files: linked("Europe/Berlin", laterBerlin),
			err: "nor does a tzdata.zi of the root's own zone data, which it holds none of"},
	}
	if own == "2024a" {
		// tzdata 2026c's WET, which 2024b made a link to Europe/Lisbon,
		// installed as tzdata installs a link: a hard link of Lisbon's, one
		// file, which Docker's archive of the directory gives as an entry
		// naming Lisbon's. At a path naming WET it is WET.
		wet := learnZone(t, "2026c", "WET")
		rows = append(rows, struct {
			name   string
			env    []string
			files  map[string]zoneFile
			mounts []container.MountPoint
			want   string
			offset int
			err    string
		}{name: "tzdata 2026c's WET, a hard link of its Europe/Lisbon", files: map[string]zoneFile{"/etc/localtime": {link: zi + "WET"}, zi + "Europe/Lisbon": {data: wet}, zi + "WET": {data: wet, hardlink: zi + "Europe/Lisbon"}}, want: "WET", offset: 0})
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			api := &fakeZoneAPI{t: t, env: r.env, files: r.files, mounts: r.mounts}
			z, err := zoneOfApp(t, api)
			switch {
			case r.err != "":
				if err == nil || !strings.Contains(err.Error(), r.err) {
					t.Fatalf("tzdata %s, the CLI's %s: %v %v; want unknown, saying %q", rel, own, z.Loc, err, r.err)
				}
				if strings.Contains(err.Error(), "/home/alice") {
					t.Fatalf("tzdata %s, the CLI's %s: %v; want no host path in it", rel, own, err)
				}
			case err != nil:
				t.Fatalf("tzdata %s, the CLI's %s: %v; want %s", rel, own, err, r.want)
			default:
				if _, off := at.In(z.Loc).Zone(); z.Loc.String() != r.want || off != r.offset {
					t.Fatalf("tzdata %s, the CLI's %s: %q at %d s on %s; want %s at %d", rel, own, z.Loc, off, at.Format(time.DateOnly), r.want, r.offset)
				}
			}
		})
	}
}

// laterSource is the zone source (tzdata.zi) of a release later than every
// one keploy knows, "2099a": the newest release's tzdata.zi (models'
// testdata), each edit (an old text, then its new one) made once.
func laterSource(t *testing.T, edits ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "models", "testdata", "tzdata.zi"))
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "# version "+models.ZoneReleasesLast+"\n", "# version 2099a\n", 1)
	for i := 0; i+1 < len(edits); i += 2 {
		if strings.Count(s, edits[i]) != 1 {
			t.Fatalf("precondition: tzdata %s's tzdata.zi holds %q %d times; want once", models.ZoneReleasesLast, edits[i], strings.Count(s, edits[i]))
		}
		s = strings.Replace(s, edits[i], edits[i+1], 1)
	}
	return []byte(s)
}

// laterZone is a zone file (version 1) of data's offsets from 1970 up to
// from, and off (seconds east of UTC) from there on: a file of a later
// release that changes the zone at from.
func laterZone(t *testing.T, data []byte, from time.Time, off int) []byte {
	t.Helper()
	loc, err := time.LoadLocationFromTZData("", data)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	_, first := start.In(loc).Zone()
	offs, at := []int{first}, []time.Time(nil)
	for u := start; ; {
		end := models.ZoneEnd(u.In(loc))
		if end.IsZero() || !end.Before(from) {
			break
		}
		u = end.UTC()
		if _, o := u.In(loc).Zone(); o != offs[len(offs)-1] {
			offs, at = append(offs, o), append(at, u)
		}
	}
	offs, at = append(offs, off), append(at, from)
	var b []byte
	put := func(v int32) { b = append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v)) }
	b = append(b, "TZif"...)
	b = append(b, make([]byte, 16)...)
	for _, n := range []int{0, 0, 0, len(at), len(offs), 2} {
		put(int32(n))
	}
	for _, tt := range at {
		put(int32(tt.Unix()))
	}
	for i := range at {
		b = append(b, byte(i+1))
	}
	for _, o := range offs {
		put(int32(o))
		b = append(b, 0, 0)
	}
	return append(b, "Z\x00"...)
}
