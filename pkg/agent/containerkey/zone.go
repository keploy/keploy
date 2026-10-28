package containerkey

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

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

// javaOptionEnvs are the environment variables a JVM reads options from
// (learn.javaZoneID reads them of a process).
var javaOptionEnvs = []string{"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "OPENJ9_JAVA_OPTIONS", "IBM_JAVA_OPTIONS"}

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
		return learn.ProcZone{}, nil, fmt.Errorf("%s sets a JVM's user.timezone, which a JVM reads over TZ and /etc/localtime; keploy reads a JVM's zone only from its process, and cannot tell from the container's configuration whether its process is a JVM", src)
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
		if fz, _, ferr := learn.ReadZoneOf(f, "", false); ferr != nil || !sameZoneName(fz.Loc.String(), "UTC") {
			return learn.ProcZone{}, nil, errors.New("the container's TZ is empty, which the C library reads as UTC, and a JVM as the zone of its /etc/localtime or /etc/timezone, which is not UTC or keploy cannot read it; keploy cannot tell from the container's configuration whether its process is a JVM")
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
// user.timezone, if it does: a JVM option variable of its environment, or
// its command (a java command line, or a shell's that runs one).
func javaZoneOption(c *container.Config) (string, bool) {
	for _, name := range javaOptionEnvs {
		if v, ok := envValue(c.Env, name); ok && strings.Contains(v, "-Duser.timezone=") {
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

// first is the first entry of the archive Docker gives of the file at p
// (its own, never a link's target), and the archive's reader, which the
// caller closes.
func (c containerFiles) first(p string) (*tar.Header, *tar.Reader, io.Closer, error) {
	rc, _, err := c.api.CopyFromContainer(c.ctx, c.id, p)
	if err != nil {
		return nil, nil, nil, pathError(p, err)
	}
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		_ = rc.Close()
		return nil, nil, nil, fmt.Errorf("keploy cannot read %s in the container: %w", p, err)
	}
	return hdr, tr, rc, nil
}

func (c containerFiles) Readlink(p string) (string, error) {
	hdr, _, rc, err := c.first(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	if hdr.Typeflag != tar.TypeSymlink {
		return "", fmt.Errorf("%s in the container is no link", p)
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
	hdr, tr, rc, err := c.first(p)
	if err != nil {
		return nil, err
	}
	if hdr.Typeflag != tar.TypeReg {
		_ = rc.Close()
		return nil, learn.NotRegular(hdr.FileInfo().Mode())
	}
	return struct {
		io.Reader
		io.Closer
	}{tr, rc}, nil
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
