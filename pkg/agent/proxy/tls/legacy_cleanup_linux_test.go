//go:build linux

package tls

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// The boot sweep runs as root, synchronously, inside every keploy record and
// test (SetupCAForApp -> SweepLegacyCA -> removeLegacyTempFiles), and it scans
// the shared temp dir, where any local user can create an entry named ca.crt*.
// These rows hold it to reading only a regular file there.

// openWatch reports which entries of one directory were opened or read. The
// kernel queues an inotify event inside the open or read call itself, so once
// the sweep has returned, every open it made is already queued: draining the
// queue then needs no clock.
type openWatch struct{ fd int }

func watchOpens(t *testing.T, dir string) *openWatch {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		t.Fatalf("inotify_init1: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_OPEN|unix.IN_ACCESS); err != nil {
		t.Fatalf("inotify_add_watch %s: %v", dir, err)
	}
	return &openWatch{fd: fd}
}

// opened drains the queued events and returns the names of the entries that
// were opened or read (the directory's own open, by the sweep's ReadDir, has
// no name and is left out).
func (w *openWatch) opened(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	buf := make([]byte, 64<<10)
	for {
		n, err := unix.Read(w.fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			return names
		}
		if err != nil {
			t.Fatalf("read inotify events: %v", err)
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
			if ev.Mask&unix.IN_Q_OVERFLOW != 0 {
				t.Fatal("inotify queue overflowed: the open check lost events")
			}
			start := off + unix.SizeofInotifyEvent
			end := start + int(ev.Len)
			if name := string(bytes.TrimRight(buf[start:end], "\x00")); name != "" {
				names[name] = true
			}
			off = end
		}
	}
}

// sweepCatchingFIFOOpens runs sweep and returns the FIFOs some reader opened
// while it ran. With the FIFO's writer missing, a blocking open there would
// have stuck the sweep (and every keploy setup on the host) forever. It needs
// no clock: a blocking writer open of each FIFO returns only once a reader
// has opened it (blocking or not), and the sweep cannot return while it is
// stuck in a blocking open. So, for each event in turn, either the sweep
// returns, or a writer open completes; a completed writer is closed with
// nothing written, a stuck read then sees EOF, and the sweep moves on. A
// non-blocking open that returned at once may still lose that race to the
// sweep's return; the rows' inotify check catches it. Once the sweep has
// returned, afterSweep runs (so a row can drain its open watch first), and
// then the writers still waiting are released by a non-blocking reader open
// the row makes itself.
func sweepCatchingFIFOOpens(t *testing.T, fifos []string, sweep, afterSweep func()) (openedFIFOs []string) {
	t.Helper()
	type writer struct {
		path string
		f    *os.File
		err  error
	}
	opened := make(chan writer, len(fifos))
	for _, p := range fifos {
		go func(p string) {
			f, err := os.OpenFile(p, os.O_WRONLY, 0)
			opened <- writer{path: p, f: f, err: err}
		}(p)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sweep()
	}()

	waiting := map[string]bool{}
	for _, p := range fifos {
		waiting[p] = true
	}
	settle := func(w writer) {
		delete(waiting, w.path)
		if w.err != nil {
			t.Errorf("writer open of %s: %v", w.path, w.err)
			return
		}
		_ = w.f.Close()
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		case w := <-opened:
			openedFIFOs = append(openedFIFOs, w.path)
			settle(w)
		}
	}
	afterSweep()
	for len(waiting) > 0 {
		var readers []*os.File
		for p := range waiting {
			r, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatalf("release the writer of %s: %v", p, err)
			}
			readers = append(readers, r)
		}
		for range readers {
			settle(<-opened)
		}
		for _, r := range readers {
			_ = r.Close()
		}
	}
	return openedFIFOs
}

func observedDebugLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}

// skipLineNames reports whether a debug line names path as an entry the sweep
// skipped.
func skipLineNames(logs *observer.ObservedLogs, path string) bool {
	for _, e := range logs.All() {
		if !strings.Contains(e.Message, "skipped") {
			continue
		}
		if p, ok := e.ContextMap()["path"].(string); ok && p == path {
			return true
		}
	}
	return false
}

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o666); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}

// TestLegacyTempSweepNeverOpensAFIFO is the hang: any local user's
// `mkfifo /tmp/ca.crt` made the root boot sweep block in open(2) forever, and
// with it every keploy record and test on the host. The sweep must skip the
// FIFO (and a socket and a directory under the same prefix) without opening
// it, name it in a debug line, and still remove a real leaked copy.
func TestLegacyTempSweepNeverOpensAFIFO(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(ca.der)

	fifo := filepath.Join(tmp, "ca.crt")
	mkfifo(t, fifo)
	sock := filepath.Join(tmp, "ca.crt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	dir := filepath.Join(tmp, "ca.crt.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	leaked := filepath.Join(tmp, "ca.crt.leaked")
	if err := os.WriteFile(leaked, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	logger, logs := observedDebugLogger()
	watch := watchOpens(t, tmp)
	var opened map[string]bool
	fifoOpens := sweepCatchingFIFOOpens(t, []string{fifo}, func() {
		removeLegacyTempFiles(logger, fp, false)
	}, func() { opened = watch.opened(t) })

	if len(fifoOpens) > 0 {
		t.Errorf("the sweep opened the FIFO %v: a blocking open there, with no writer, hangs keploy setup forever", fifoOpens)
	}
	// An open of the socket fails (ENXIO) and queues no event: its skip is
	// shown by its debug line below.
	for _, name := range []string{"ca.crt", "ca.crt.d"} {
		if opened[name] {
			t.Errorf("the sweep opened %s, which is not a regular file: it must look at the type first", name)
		}
	}
	for _, p := range []string{fifo, sock, dir} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s must be left in place: %v", p, err)
		}
		if !skipLineNames(logs, p) {
			t.Errorf("no debug line names the skipped entry %s", p)
		}
	}
	if _, err := os.Lstat(leaked); !os.IsNotExist(err) {
		t.Errorf("a real leaked copy of the CA must still be removed; lstat: %v", err)
	}
}

// TestLegacyTempSweepNeverFollowsALinkOutOfTheTempDir: a ca.crt* symlink in
// the temp dir is skipped as a link. Its target, outside the temp dir, is
// never opened or read, even when it is the very CA the sweep removes or a
// FIFO that would block it, and the link itself is left in place.
func TestLegacyTempSweepNeverFollowsALinkOutOfTheTempDir(t *testing.T) {
	tmp := t.TempDir()
	outside := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(ca.der)

	target := filepath.Join(outside, "target.pem")
	if err := os.WriteFile(target, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	outsideFIFO := filepath.Join(outside, "pipe")
	mkfifo(t, outsideFIFO)
	link := filepath.Join(tmp, "ca.crt")
	fifoLink := filepath.Join(tmp, "ca.crt.pipe")
	for l, to := range map[string]string{link: target, fifoLink: outsideFIFO} {
		if err := os.Symlink(to, l); err != nil {
			t.Fatal(err)
		}
	}

	logger, logs := observedDebugLogger()
	watch := watchOpens(t, outside)
	var opened map[string]bool
	fifoOpens := sweepCatchingFIFOOpens(t, []string{outsideFIFO}, func() {
		removeLegacyTempFiles(logger, fp, false)
	}, func() { opened = watch.opened(t) })

	if len(fifoOpens) > 0 {
		t.Errorf("the sweep followed a link and opened the FIFO %v", fifoOpens)
	}
	for name := range opened {
		t.Errorf("the sweep opened %s outside the temp dir through a link", filepath.Join(outside, name))
	}
	for _, l := range []string{link, fifoLink} {
		if fi, err := os.Lstat(l); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("the link %s must be left in place (lstat: %v)", l, err)
		}
		if !skipLineNames(logs, l) {
			t.Errorf("no debug line names the skipped link %s", l)
		}
	}
	if data, err := os.ReadFile(target); err != nil || !bytes.Equal(data, ca.certPEM) {
		t.Errorf("the link's target must be left untouched (err: %v)", err)
	}
}

// TestOpenedTempEntryIsCheckedAgain covers the open half, readOpenedRegular,
// which holds the rule for an entry swapped in between the sweep's Lstat and
// its open: the open never follows a link and never blocks, and what it
// opened is read only if it is a regular file.
func TestOpenedTempEntryIsCheckedAgain(t *testing.T) {
	tmp := t.TempDir()
	outside := t.TempDir()
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}

	// Non-blocking: a FIFO swapped in after the Lstat cannot block the open.
	regular := filepath.Join(tmp, "ca.crt")
	if err := os.WriteFile(regular, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openNoFollowNonBlocking(regular)
	if err != nil {
		t.Fatalf("open a regular file: %v", err)
	}
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFL, 0)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_NONBLOCK == 0 {
		t.Error("the sweep's open is blocking: a FIFO swapped in after the Lstat would block it with no writer")
	}

	// No-follow: a link swapped in after the Lstat is not opened through.
	target := filepath.Join(outside, "target.pem")
	if err := os.WriteFile(target, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "ca.crt.link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	watch := watchOpens(t, outside)
	data, err := readOpenedRegular(link, maxLegacyTempCertBytes)
	if !errors.Is(err, syscall.ELOOP) {
		t.Errorf("reading through a link must fail with ELOOP, got %d bytes, err %v", len(data), err)
	}
	if opened := watch.opened(t); opened["target.pem"] {
		t.Error("the open followed the link to its target outside the temp dir")
	}

	// Regular after the open: a device is opened (non-blocking) but not read.
	if data, err := readOpenedRegular(os.DevNull, maxLegacyTempCertBytes); !errors.Is(err, errTempEntryNotRegular) {
		t.Errorf("a device must be refused after the open, got %d bytes, err %v", len(data), err)
	}
}

// TestTempEntryReadStopsAtTheCap: the cap bounds what is read, not only what
// is kept, so a huge ca.crt* file cannot make the root sweep read it all into
// memory. rchar in /proc/self/io counts the bytes this process passed to
// read(2); this row is not parallel, so nothing else reads megabytes meanwhile.
func TestTempEntryReadStopsAtTheCap(t *testing.T) {
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	const size = 32 << 20
	big := filepath.Join(t.TempDir(), "ca.crt")
	content := append(append([]byte{}, ca.certPEM...), bytes.Repeat([]byte("x"), size)...)
	if err := os.WriteFile(big, content, 0o644); err != nil {
		t.Fatal(err)
	}
	before := readCharsSoFar(t)
	data, err := readOpenedRegular(big, maxLegacyTempCertBytes)
	read := readCharsSoFar(t) - before
	if err == nil {
		t.Errorf("a file over the cap must be refused, got %d bytes", len(data))
	}
	if read > 4*maxLegacyTempCertBytes {
		t.Errorf("the sweep read %d bytes of a %d-byte file: the read must stop at the cap (%d)",
			read, len(content), maxLegacyTempCertBytes)
	}
}

func readCharsSoFar(t *testing.T) int64 {
	t.Helper()
	b, err := os.ReadFile("/proc/self/io")
	if err != nil {
		t.Fatalf("read /proc/self/io: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "rchar: "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				t.Fatalf("parse rchar %q: %v", v, err)
			}
			return n
		}
	}
	t.Fatal("no rchar line in /proc/self/io")
	return 0
}
