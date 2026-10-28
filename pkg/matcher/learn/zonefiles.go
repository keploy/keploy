package learn

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// ZoneFiles is a process's root's files as keploy reads its zone from them
// (readZone): a directory keploy reaches itself (dirFiles: a process's
// /proc/<pid>/root, or "/" for keploy's own), or a container's that the
// keploy CLI reads through Docker's API for a docker-mode agent, which runs
// in a PID namespace of its own and reads no process's zone (ProcZones). Each
// p is an absolute path in the root's own view.
type ZoneFiles interface {
	// Lstat is the mode of the file at p, a link at p's last name not
	// followed; p's earlier names name no link (resolveIn resolved them). An
	// error wrapping fs.ErrNotExist where p names nothing.
	Lstat(p string) (fs.FileMode, error)
	// Readlink is the target of the link at p, as the link holds it.
	Readlink(p string) (string, error)
	// OpenRegular opens the regular file at p, whose names name no link,
	// for reading, and an error of a file that is no regular one
	// (errNotRegular), never opening it.
	OpenRegular(p string) (io.ReadCloser, error)
	// Readable is nil where the root can still be read, and why not
	// otherwise: an absence read in one that cannot (an exited process's,
	// a removed container's) is no absence (localtimeAbsentIn, utcZone).
	Readable() error
	// ZoneMount is the mount the zone file at p (whose names name no link),
	// which names the zone zn by its path, is read from another zone's file
	// through, nil where it is read through none such (mountedZone): a
	// host's /etc/localtime bind-mounted over an image's link to its zone.
	// An error, not lasting, where keploy cannot tell.
	ZoneMount(p, zn string) (*models.ZoneMount, error)
}

// dirFiles is the files under the directory root, which keploy reads
// itself (under), through the mounts mounts (zoneMountOf). Its zero mounts
// read no zone file's mount: only a reader of no zone's name holds them
// (readZoneFile, readUnder).
type dirFiles struct {
	root   string
	mounts zoneMounts
}

func (d dirFiles) Lstat(p string) (fs.FileMode, error) {
	fi, err := os.Lstat(under(d.root, p))
	if err != nil {
		return 0, err
	}
	return fi.Mode(), nil
}

func (d dirFiles) Readlink(p string) (string, error) {
	return os.Readlink(under(d.root, p))
}

func (d dirFiles) OpenRegular(p string) (io.ReadCloser, error) {
	f, err := openRegular(d.root, p)
	if err != nil {
		// Never a nil *os.File in a non-nil io.ReadCloser.
		return nil, err
	}
	return f, nil
}

func (d dirFiles) Readable() error {
	_, err := os.Stat(d.root)
	return err
}

func (d dirFiles) ZoneMount(p, zn string) (*models.ZoneMount, error) {
	return zoneMountOf(d.mounts, d.root, p, zn)
}

// NotRegular is the error ZoneFiles.OpenRegular gives of a file of mode,
// which is no regular file.
func NotRegular(mode fs.FileMode) error {
	return errNotRegular{mode: mode}
}

// ReadZoneOf is the zone a process whose root's files f gives writes its
// wall clock in, as the C library and Go read it, as ReadProcZone reads one
// that is no JVM: TZ (tz, where set), else f's /etc/localtime, a link into a
// zoneinfo directory named by the zone it names, a copied file by the zone
// f's /etc/timezone names where its bytes are that zone's file, else
// unnamed; UTC where f holds no /etc/localtime at all. An error where keploy
// cannot read it: a TZ of a POSIX rule, a TZ naming a zone f holds no file
// of, a link to nothing.
func ReadZoneOf(f ZoneFiles, tz string, set bool) (ProcZone, error) {
	loc, err := readZoneIn(f, tz, set, false)
	if err != nil {
		return ProcZone{}, err
	}
	return ProcZone{Loc: loc, Name: models.ZoneNameOf(loc, time.Now())}, nil
}

// TimezoneFileOf is the zone f's /etc/timezone names, its first line
// trimmed, which a JVM of a JDK before JDK-8345213 reads as its zone where
// TZ names none (javaPlatformID), over its /etc/localtime; false where f
// holds none, or it is empty or a directory (docker makes one of a bind of
// a host file that is not there). An error where keploy cannot read it.
func TimezoneFileOf(f ZoneFiles) (string, bool, error) {
	b, err := readUnderIn(f, "/etc/timezone")
	var nr errNotRegular
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.As(err, &nr) && nr.mode.IsDir():
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("keploy cannot read /etc/timezone, which a JVM may read its zone from: %w", err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = javaStrip(strings.TrimSpace(line))
	return line, line != "", nil
}
