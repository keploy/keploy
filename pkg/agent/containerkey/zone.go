package containerkey

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// zoneAPI is the part of Docker's API containerZone reads a container's
// files through.
type zoneAPI interface {
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerStatPath(ctx context.Context, containerID, path string) (container.PathStat, error)
	CopyFromContainer(ctx context.Context, containerID, srcPath string) (io.ReadCloser, container.PathStat, error)
}

// Zone is the zone an app container's processes write a wall clock of no
// zone in, as the keploy CLI read it through Docker's API (containerZone)
// for its docker-mode agent, which runs in a PID namespace of its own and
// reads no process's zone from /proc (learn.ProcZones): the name the zone's
// Loc carries ("" for a zone file of no name) and the bytes of the zone file
// the CLI read it from, none for UTC (learn.ZoneOfFile reads them); or Err,
// why the CLI could not read it.
type Zone struct {
	Name string `json:"name,omitempty"`
	Data []byte `json:"data,omitempty"`
	Err  string `json:"err,omitempty"`
}

// readZone is the Zone of container id, whose configuration Docker's
// inspect gave as cfg (containerZone).
func readZone(ctx context.Context, api zoneAPI, id string, cfg *container.Config) Zone {
	z, data, err := containerZone(ctx, api, id, cfg)
	if err != nil {
		return Zone{Err: err.Error()}
	}
	return Zone{Name: z.Loc.String(), Data: data}
}

// containerZone is the zone the processes of container id, of
// configuration cfg, write a wall clock of no zone in, with the bytes of the
// zone file it was read from (none for UTC): TZ from the container's
// environment (its Config.Env, the image's merged in), else its
// /etc/localtime, read from the container's own files (CopyFromContainer),
// as the agent reads a process's root (learn.ReadZoneOf): a link into a
// zoneinfo directory names the zone it links to, a copied file the zone
// /etc/timezone names where its bytes are that zone's file, and no
// /etc/localtime at all is UTC. It is the zone of a process started with
// the container's environment and files, which is no JVM's own: an error
// (the zone unknown, as before) where the container's environment or
// command sets a JVM's user.timezone, or, where TZ names none, its
// /etc/timezone names a zone other than its /etc/localtime (JDKs before
// JDK-8345213 read the first, the C library the second), since keploy
// cannot tell from a container's configuration whether its process is a
// JVM. An error too where keploy cannot read the zone (a TZ of a POSIX
// rule, a TZ naming no zone file in the container, a link to nothing, a
// container gone).
func containerZone(ctx context.Context, api zoneAPI, id string, cfg *container.Config) (learn.ProcZone, []byte, error) {
	if cfg == nil {
		return learn.ProcZone{}, nil, errors.New("docker reported no configuration of the container")
	}
	tz, set := envValue(cfg.Env, "TZ")
	if src, ok := javaZoneOption(cfg); ok {
		return learn.ProcZone{}, nil, fmt.Errorf("%s holds -Duser.timezone=, which sets the zone of a JVM given it, over TZ and /etc/localtime; keploy reads a JVM's zone only from its process, and cannot tell from the container's configuration whether its process is a JVM given it", src)
	}
	f := containerFiles{ctx: ctx, api: api, id: id}
	z, data, err := learn.ReadZoneOf(f, tz, set)
	if err != nil {
		return learn.ProcZone{}, nil, err
	}
	if set && strings.TrimPrefix(tz, ":") != "" {
		// A JVM reads a TZ that names a zone as the C library does.
		return z, data, nil
	}
	if set {
		// An empty TZ: the C library reads UTC, a JVM the zone of its
		// files.
		const empty = "the container's TZ is empty, which the C library reads as UTC, and a JVM as the zone of its /etc/localtime or /etc/timezone"
		const which = "; keploy cannot tell from the container's configuration whether its process is a JVM"
		fz, _, ferr := learn.ReadZoneOf(f, "", false)
		if ferr != nil {
			// A failed read is no zone, never UTC (a nil Loc prints UTC).
			return learn.ProcZone{}, nil, fmt.Errorf("%s, whose read failed (%w)%s", empty, ferr, which)
		}
		if !sameZoneName(fz.Loc.String(), "UTC") && !utcRules(fz.Loc) {
			return learn.ProcZone{}, nil, errors.New(empty + ", which is not UTC" + which)
		}
	}
	tzfile, ok, err := learn.TimezoneFileOf(f)
	if err != nil {
		return learn.ProcZone{}, nil, err
	}
	if !ok {
		return z, data, nil
	}
	if !sameZoneName(tzfile, z.Loc.String()) {
		return learn.ProcZone{}, nil, fmt.Errorf("the container's /etc/timezone names %q, which JDKs before JDK-8345213 read as a JVM's zone, and its /etc/localtime %q, which the C library reads; keploy cannot tell from the container's configuration which its process reads", tzfile, z.Loc.String())
	}
	return z, data, nil
}

// utcNames are the names of UTC's zone files and of Go's UTC.
var utcNames = map[string]bool{"UTC": true, "Etc/UTC": true, "UCT": true, "Etc/UCT": true, "Universal": true, "Etc/Universal": true, "Zulu": true, "Etc/Zulu": true}

// sameZoneName reports whether a and b name one zone: one name, or two of
// UTC's.
func sameZoneName(a, b string) bool {
	return a == b || utcNames[a] && utcNames[b]
}

// utcRules reports whether loc reads every instant as UTC does: one offset
// of zero at every time (a copied UTC file of no name, as busybox ships
// /etc/localtime), whatever it is named.
func utcRules(loc *time.Location) bool {
	if loc == nil {
		return false
	}
	// Walk loc's offsets from the epoch back to its first and on to its
	// last, at most zoneWalkCap of them each way: a zone whose offsets
	// change for good (a rule of summer time) has one not zero long before.
	for _, back := range []bool{true, false} {
		t := time.Unix(0, 0)
		for n := 0; ; n++ {
			lt := t.In(loc)
			if _, off := lt.Zone(); off != 0 || n == zoneWalkCap {
				return false
			}
			start, end := lt.ZoneBounds()
			if back && start.IsZero() || !back && end.IsZero() {
				break
			}
			if back {
				t = start.Add(-time.Nanosecond)
			} else {
				t = end
			}
		}
	}
	return true
}

// zoneWalkCap bounds utcRules' walk of a zone's offsets each way.
const zoneWalkCap = 4096

// envValue is the value of name in env, the first, as getenv reads it, and
// whether env sets it.
func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

// javaZoneOption names where the container's configuration sets a JVM's
// user.timezone, if it does: a variable of its environment (one a JVM
// reads options from, JAVA_TOOL_OPTIONS and the like, or one a script
// hands a java command line, JAVA_OPTS, CATALINA_OPTS and the like), or its
// command (a java command line, or a shell's that runs one). keploy reads
// the container's configuration, not its process, so it cannot tell which
// variable a script hands java: any that holds the option leaves the zone
// unknown.
func javaZoneOption(c *container.Config) (string, bool) {
	for _, kv := range c.Env {
		if name, v, ok := strings.Cut(kv, "="); ok && strings.Contains(v, "-Duser.timezone=") {
			return "the container's " + name, true
		}
	}
	for _, arg := range append(append([]string{}, c.Entrypoint...), c.Cmd...) {
		if strings.Contains(arg, "-Duser.timezone=") {
			return "the container's command", true
		}
	}
	return "", false
}

// containerFiles is container id's files read through Docker's API
// (learn.ZoneFiles), each call under ctx.
type containerFiles struct {
	ctx context.Context
	api zoneAPI
	id  string
}

// pathError is err, wrapping fs.ErrNotExist where Docker says the path, or
// the container, is not there (Readable tells which).
func pathError(p string, err error) error {
	if errdefs.IsNotFound(err) {
		return fmt.Errorf("%s: %w (%v)", p, fs.ErrNotExist, err)
	}
	return fmt.Errorf("keploy cannot read %s in the container: %w", p, err)
}

func (c containerFiles) Lstat(p string) (fs.FileMode, error) {
	st, err := c.api.ContainerStatPath(c.ctx, c.id, p)
	if err != nil {
		return 0, pathError(p, err)
	}
	return st.Mode, nil
}

// archived is the one file of the archive Docker gives of the file at p
// (CopyFromContainer: p's own entry, never a link's target): its header,
// and of a regular file its bytes, read into memory whole. The archive is
// the container's, so untrusted, and keploy takes only one of the shape
// Docker gives of one file: a single entry, of type typ (a regular file, or
// a link where Readlink asks: never a hard link, a directory or a device in
// a regular file's place), named p's last name ("../localtime", an absolute
// name, or another name is another file's), and a regular file of at most
// learn.ZoneFileCap bytes, refused before any is read. No entry's name, nor
// a link's target, is ever a path on this host: learn resolves a target in
// the container's own view and reads it through Docker again.
func (c containerFiles) archived(p string, typ byte) (hdr *tar.Header, data []byte, err error) {
	rc, _, err := c.api.CopyFromContainer(c.ctx, c.id, p)
	if err != nil {
		return nil, nil, pathError(p, err)
	}
	defer func() {
		if cerr := rc.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("keploy cannot close Docker's archive of %s in the container: %w", p, cerr))
		}
		if err != nil {
			hdr, data = nil, nil
		}
	}()
	tr := tar.NewReader(rc)
	if hdr, err = tr.Next(); err != nil {
		return nil, nil, fmt.Errorf("keploy cannot read %s in the container: %w", p, err)
	}
	if base := path.Base(p); path.Clean(hdr.Name) != base {
		return nil, nil, fmt.Errorf("the archive Docker gives of %s in the container holds %q, not %q: keploy reads no other file", p, hdr.Name, base)
	}
	if hdr.Typeflag != typ {
		switch {
		case typ == tar.TypeSymlink:
			return nil, nil, fmt.Errorf("%s in the container is no link", p)
		case hdr.Typeflag == tar.TypeLink:
			return nil, nil, fmt.Errorf("the archive Docker gives of %s in the container holds a hard link to %q in its place: %w", p, hdr.Linkname, learn.NotRegular(fs.ModeIrregular))
		}
		return nil, nil, learn.NotRegular(hdr.FileInfo().Mode())
	}
	if hdr.Size > learn.ZoneFileCap {
		return nil, nil, fmt.Errorf("%s in the container is %d bytes, more than the %d keploy reads of a zone file, an /etc/timezone or an argfile", p, hdr.Size, learn.ZoneFileCap)
	}
	if data, err = io.ReadAll(tr); err != nil {
		return nil, nil, fmt.Errorf("keploy cannot read %s in the container: %w", p, err)
	}
	if next, err := tr.Next(); err == nil {
		return nil, nil, fmt.Errorf("the archive Docker gives of %s in the container holds %q too: keploy reads an archive of one file alone", p, next.Name)
	} else if err != io.EOF {
		return nil, nil, fmt.Errorf("keploy cannot read %s in the container: %w", p, err)
	}
	return hdr, data, nil
}

func (c containerFiles) Readlink(p string) (string, error) {
	hdr, _, err := c.archived(p, tar.TypeSymlink)
	if err != nil {
		return "", err
	}
	return hdr.Linkname, nil
}

func (c containerFiles) OpenRegular(p string) (io.ReadCloser, error) {
	// Its type first, so that no other file (a directory, whose archive
	// would hold all it holds) is read.
	mode, err := c.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !mode.IsRegular() {
		return nil, learn.NotRegular(mode)
	}
	_, data, err := c.archived(p, tar.TypeReg)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c containerFiles) Readable() error {
	_, err := c.api.ContainerInspect(c.ctx, c.id)
	return err
}

// ZoneMount is nil: keploy reads no container's mount table. The daemon
// reads the container's files through its mounts, so a host's
// /etc/localtime bind-mounted over an image's link to its zone
// (-v /etc/localtime:/etc/localtime:ro lands on the file the link names)
// reads as the mounted file's bytes there, which learn checks against the
// rules the keploy CLI's own zone data gives the zone the path names
// (learn's loadZone, and utcZone for an empty TZ or TZ=UTC): the CLI shares
// no root with the container, so its zone data is never that file.
func (c containerFiles) ZoneMount(p, zn string) (*models.ZoneMount, error) {
	return nil, nil
}
