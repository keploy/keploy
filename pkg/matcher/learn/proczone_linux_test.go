package learn

import (
	"bufio"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// A zone file that is no regular file (a FIFO, which would hold a read
// until something writes it) is an error, never read: the agent reads a
// process's zone on its connection's path.
func TestAZoneFileThatIsNoRegularFileIsNotRead(t *testing.T) {
	proc := t.TempDir()
	fakeProc(t, proc, "700", "500", nil, map[string][]byte{"etc/hostname": []byte("app\n")}, nil)
	if err := syscall.Mkfifo(filepath.Join(proc, "700", "root", "etc", "localtime"), 0o644); err != nil {
		t.Skipf("no FIFO: %v", err)
	}
	if z, err := ReadProcZone(proc, 700); err == nil || z.Loc != nil || !isLasting(err) {
		t.Fatalf("a FIFO /etc/localtime: %v %v (lasting %v); want an error and no zone, kept for the process's life", z.Loc, err, isLasting(err))
	}
	if _, err := os.Stat(filepath.Join(proc, "700", "root", "etc", "localtime")); err != nil {
		t.Fatal(err)
	}
}

// heldFlags is the open flags of keploy's own descriptor fd
// (/proc/self/fdinfo).
func heldFlags(t *testing.T, fd int) int {
	t.Helper()
	b, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "flags:"); ok {
			f, err := strconv.ParseInt(strings.TrimSpace(v), 8, 64)
			if err != nil {
				t.Fatal(err)
			}
			return int(f)
		}
	}
	t.Fatalf("no flags in fdinfo of %d: %q", fd, b)
	return 0
}

// sameFile reports whether keploy's descriptor fd holds the file at p.
func sameFile(t *testing.T, fd int, p string) bool {
	t.Helper()
	var a, b syscall.Stat_t
	if err := syscall.Fstat(fd, &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(p, &b); err != nil {
		t.Fatal(err)
	}
	return a.Dev == b.Dev && a.Ino == b.Ino
}

// An application's TZ naming a device or a FIFO is refused without keploy,
// which runs as root, ever opening it for reading: a device's driver's
// open would run (a watchdog arms, a tape rewinds at its close), a FIFO
// would gain a reader. keploy holds such a file by an O_PATH descriptor
// alone while it reads its type, so a writer's open of the FIFO, which
// needs a reader, finds none (ENXIO).
func TestAZoneFileThatIsNoRegularFileIsNeverOpenedForReading(t *testing.T) {
	proc := t.TempDir()
	fakeProc(t, proc, "701", "500", []string{"TZ=/run/zone"}, map[string][]byte{"etc/hostname": []byte("app\n")}, nil)
	fifo := filepath.Join(proc, "701", "root", "run", "zone")
	if err := os.MkdirAll(filepath.Dir(fifo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("no FIFO: %v", err)
	}
	// The process's root is the host's, so TZ=/dev/null names the host's
	// own character device.
	fakeProc(t, proc, "702", "500", []string{"TZ=/dev/null"}, nil, nil)
	if err := os.Symlink("/", filepath.Join(proc, "702", "root")); err != nil {
		t.Fatal(err)
	}
	held := map[string]int{}
	heldZoneFile = func(fd int) {
		for _, p := range []string{fifo, "/dev/null"} {
			if !sameFile(t, fd, p) {
				continue
			}
			held[p]++
			if fl := heldFlags(t, fd); fl&unix.O_PATH == 0 {
				t.Errorf("keploy held %s by a descriptor of flags %o; want O_PATH", p, fl)
			}
			if p != fifo {
				continue
			}
			w, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err == nil {
				_ = syscall.Close(w)
			}
			if !errors.Is(err, syscall.ENXIO) {
				t.Errorf("a writer's open of the FIFO TZ names, while keploy held it: %v; want ENXIO, no reader", err)
			}
		}
	}
	t.Cleanup(func() { heldZoneFile = nil })
	for _, row := range []struct {
		pid  uint32
		p    string
		mode fs.FileMode
	}{{701, fifo, fs.ModeNamedPipe}, {702, "/dev/null", fs.ModeDevice | fs.ModeCharDevice}} {
		z, err := ReadProcZone(proc, row.pid)
		var nr errNotRegular
		if err == nil || z.Loc != nil || !isLasting(err) || !errors.As(err, &nr) || nr.mode.Type() != row.mode {
			t.Errorf("TZ naming %s: %v %v (lasting %v); want refused as no regular file (%v), kept for the process's life", row.p, z.Loc, err, isLasting(err), row.mode)
		}
		if held[row.p] != 1 {
			t.Errorf("TZ naming %s: keploy held it %d times; want once", row.p, held[row.p])
		}
	}
}

// A path that resolves to the process's root (TZ=/, an /etc/timezone
// linked to /) names the root, which keploy follows (/proc/<pid>/root is a
// link): a directory, as before, never "a link swapped in".
func TestAZonePathThatIsTheProcesssRootIsItsRootDirectory(t *testing.T) {
	proc := t.TempDir()
	fakeProc(t, proc, "703", "500", []string{"TZ=/"}, nil, nil)
	if err := os.Symlink("/", filepath.Join(proc, "703", "root")); err != nil {
		t.Fatal(err)
	}
	var nr errNotRegular
	if z, err := ReadProcZone(proc, 703); err == nil || z.Loc != nil || !isLasting(err) || !errors.As(err, &nr) || !nr.mode.IsDir() {
		t.Errorf("TZ=/: %v %v (lasting %v); want refused as no regular file (a directory), kept for the process's life", z.Loc, err, isLasting(err))
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "img", "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(dir, "img", "etc", "timezone")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	if err := os.Symlink(filepath.Join(dir, "img"), root); err != nil {
		t.Fatal(err)
	}
	if line, ok, err := javaTimezoneLine(root); line != "" || ok || err != nil {
		t.Errorf("an /etc/timezone linked to the root: %q %v %v; want no line, as a directory gives", line, ok, err)
	}
}

// keploy reads the file whose type it read, never one that took its name
// since: it reads it through the descriptor it read the type through. A
// link at the path's last name, which resolveUnder followed before, is one
// swapped in since: an error read again at the next call, never followed.
func TestAZoneFileIsReadAsTheFileItsTypeWasReadOf(t *testing.T) {
	dir := t.TempDir()
	name, other := filepath.Join(dir, "localtime"), filepath.Join(dir, "other")
	if err := os.WriteFile(name, []byte("the file checked"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("a file put in its place"), 0o644); err != nil {
		t.Fatal(err)
	}
	swaps := 0
	heldZoneFile = func(fd int) {
		if heldFlags(t, fd)&unix.O_PATH == 0 {
			return
		}
		swaps++
		if err := os.Rename(other, name); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { heldZoneFile = nil })
	if b, err := readRegular(dir, "/localtime"); err != nil || string(b) != "the file checked" || swaps != 1 {
		t.Errorf("a file replaced after its type was read: %q %v (%d swaps); want the file checked", b, err, swaps)
	}
	heldZoneFile = nil
	link := filepath.Join(dir, "link")
	if err := os.Symlink(name, link); err != nil {
		t.Fatal(err)
	}
	if b, err := readRegular(dir, "/link"); err == nil || isLasting(err) || !strings.Contains(err.Error(), link+" became a link while keploy read it") {
		t.Errorf("a link at the last name: %q %v (lasting %v); want an error read again, the link never followed", b, err, isLasting(err))
	}
	// A file keploy cannot open for reading is named in the error by its
	// own path, not by the descriptor's.
	t.Run("unreadable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads a file of no permissions")
		}
		if err := os.Chmod(name, 0); err != nil {
			t.Fatal(err)
		}
		var pe *fs.PathError
		if b, err := readRegular(dir, "/localtime"); !errors.Is(err, fs.ErrPermission) || !errors.As(err, &pe) || pe.Path != name {
			t.Errorf("a file keploy cannot read: %q %v; want a permission error naming %s", b, err, name)
		}
	})
}
