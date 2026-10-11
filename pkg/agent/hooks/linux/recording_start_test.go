//go:build linux

package linux

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// fakeNamespace is a PID namespace as recordingStart sees it: its inode, its
// IPC namespace's, when its PID 1 started, and its /dev/shm.
type fakeNamespace struct {
	pidNS, ipcNS, init uint64
	shm                string
}

// load is an agent loading its hooks in ns at boottime now: the
// recording_start_time it gives the tracepoint.
func (ns fakeNamespace) load(t *testing.T, now uint64) uint64 {
	t.Helper()
	return recordingStart(zap.NewNop(), ns.source(func() (uint64, error) { return now, nil }))
}

// source is ns read with the given clock.
func (ns fakeNamespace) source(now func() (uint64, error)) recordingStartSource {
	return recordingStartSource{
		now:       now,
		pidNS:     func() (uint64, error) { return ns.pidNS, nil },
		ipcNS:     func() (uint64, error) { return ns.ipcNS, nil },
		initStart: func() (uint64, error) { return ns.init, nil },
		shmDir:    ns.shm,
	}
}

// onlyTheRecord fails t unless ns's /dev/shm holds the one record, a regular
// file with start, and nothing else: no temporary file left behind.
func (ns fakeNamespace) onlyTheRecord(t *testing.T, start uint64) {
	t.Helper()
	entries, err := os.ReadDir(ns.shm)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != filepath.Base(ns.record()) {
		t.Fatalf("/dev/shm holds %q, want only %q", names, filepath.Base(ns.record()))
	}
	r, err := readRecordingStartFile(ns.record())
	if err != nil {
		t.Fatalf("the record: %v", err)
	}
	if r != (recordingStartRecord{pidNS: ns.pidNS, init: ns.init, start: start}) {
		t.Fatalf("the record says %+v, want start %d", r, start)
	}
}

func (ns fakeNamespace) record() string {
	return filepath.Join(ns.shm, recordingStartFile+strconv.FormatUint(ns.pidNS, 10))
}

// excluded is the sys_enter_socket rule: a process that started before
// recording_start_time is left alone, and only one that did not is the
// application.
func excluded(processStart, recordingStartTime uint64) bool {
	return recordingStartTime > 0 && processStart < recordingStartTime
}

func newPod(t *testing.T) fakeNamespace {
	return fakeNamespace{pidNS: 0xF0000123, ipcNS: 0xF0000456, init: 7, shm: t.TempDir()}
}

// A pod's sidecar agent is restarted, as kubelet restarts a container, while
// the application it was recording runs on in the pod's PID namespace. The
// restarted agent must go on recording the application, and go on leaving
// alone what was there before keploy. With each agent stamping its own load
// time the application, older than the restarted agent, was excluded from
// then on.
func TestARestartedAgentKeepsRecordingTheApplication(t *testing.T) {
	pod := newPod(t)
	const (
		sidecarStart = 100 // an Istio proxy, started before keploy
		firstLoad    = 1000
		appStart     = 1500 // started once the first agent was ready
		restartLoad  = 9000
		laterWorker  = 9500 // a process the application starts after the restart
	)
	first := pod.load(t, firstLoad)
	if first != firstLoad {
		t.Fatalf("the first agent in the namespace: recording_start_time %d, want its load time %d", first, firstLoad)
	}
	pod.onlyTheRecord(t, firstLoad)
	if excluded(appStart, first) || !excluded(sidecarStart, first) {
		t.Fatalf("under the first agent: application excluded %v, sidecar excluded %v; want false, true",
			excluded(appStart, first), excluded(sidecarStart, first))
	}

	for i, load := range []uint64{restartLoad, restartLoad + 1000} {
		got := pod.load(t, load)
		if excluded(appStart, got) {
			t.Errorf("restart %d: recording_start_time %d excludes the application, which started at %d and runs on", i+1, got, appStart)
		}
		if excluded(laterWorker, got) {
			t.Errorf("restart %d: recording_start_time %d excludes a process the application started at %d", i+1, got, laterWorker)
		}
		if !excluded(sidecarStart, got) {
			t.Errorf("restart %d: recording_start_time %d no longer excludes the sidecar that started at %d, before keploy", i+1, got, sidecarStart)
		}
		if got != firstLoad {
			t.Errorf("restart %d: recording_start_time %d, want the first agent's %d", i+1, got, firstLoad)
		}
		pod.onlyTheRecord(t, firstLoad)
	}
}

// The record is only a previous agent's of the same PID namespace. A record
// left by a namespace that is gone, whose inode the kernel has given to a new
// one, has another init, and must not be taken for the new namespace's: the
// new one's processes all started after it, and a stale, earlier start would
// stop excluding what was there before the new namespace's first agent.
func TestARecordingStartIsKeptPerPIDNamespace(t *testing.T) {
	gone := newPod(t)
	if got := gone.load(t, 1000); got != 1000 {
		t.Fatalf("first agent of the old namespace: %d, want 1000", got)
	}

	reused := gone
	reused.init = 50000 // a new namespace with the same inode: another PID 1
	if got := reused.load(t, 60000); got != 60000 {
		t.Fatalf("first agent of a new namespace with a reused inode: recording_start_time %d, want its own load time 60000", got)
	}
	if got := reused.load(t, 70000); got != 60000 {
		t.Fatalf("restarted agent of the new namespace: recording_start_time %d, want its first agent's 60000", got)
	}

	other := gone
	other.pidNS = 0xF0000999 // another PID namespace sharing this (non-host) /dev/shm
	other.init = 7
	if got := other.load(t, 80000); got != 80000 {
		t.Fatalf("first agent of another namespace: recording_start_time %d, want its own load time 80000", got)
	}
	if got := reused.load(t, 90000); got != 60000 {
		t.Fatalf("another namespace's agent changed this one's recording start: %d, want 60000", got)
	}
}

// Where the namespace outlives any one recording, nothing is kept, and each
// agent's load time is used as it always was.
func TestNoRecordingStartIsKeptWhereTheNamespaceOutlivesTheRecording(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*fakeNamespace)
	}{
		{"host PID namespace", func(ns *fakeNamespace) { ns.pidNS = procPIDInitInode }},
		{"host IPC namespace", func(ns *fakeNamespace) { ns.ipcNS = procIPCInitInode }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := newPod(t)
			tc.edit(&ns)
			if got := ns.load(t, 1000); got != 1000 {
				t.Fatalf("first load: %d, want 1000", got)
			}
			if got := ns.load(t, 2000); got != 2000 {
				t.Fatalf("second load: %d, want its own load time 2000", got)
			}
			if entries, _ := os.ReadDir(ns.shm); len(entries) != 0 {
				t.Fatalf("kept %d files in /dev/shm, want none", len(entries))
			}
		})
	}
}

// A record that is not one this code wrote, or that says the recording
// started after now, is replaced by this agent's. /dev/shm is shared with
// every container of the pod, so what is at the record's path may be
// anything: it is read only if it is a regular file of this agent's user,
// never through a symbolic link, never by waiting on a FIFO, and never
// beyond the size of a record, and is replaced, not written through.
func TestADamagedRecordingStartIsReplaced(t *testing.T) {
	valid := recordingStartRecord{pidNS: 0xF0000123, init: 7, start: 500}.String()
	write := func(content string) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	elsewhere := filepath.Join(t.TempDir(), "valid-record")
	if err := os.WriteFile(elsewhere, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(*testing.T, string){
		"garbage":        write("not a record\n"),
		"empty":          write(""),
		"trailing bytes": write(valid + "x"),
		"in the future":  write(recordingStartRecord{pidNS: 0xF0000123, init: 7, start: 5000}.String()),
		"zero":           write(recordingStartRecord{pidNS: 0xF0000123, init: 7, start: 0}.String()),
		"oversized":      write(valid + strings.Repeat(" ", 1<<20)),
		// A valid record, read through a link, would be taken for this
		// namespace's: the link is not followed, and is replaced.
		"symbolic link to a valid record": func(t *testing.T, path string) {
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Fatal(err)
			}
		},
		// Opened as a file, it waits for a writer that never comes, and the
		// hooks never finish loading.
		"FIFO": func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		// One that holds a valid record, and whose writer is gone, so it
		// reads as the record and then end of file: not a regular file, so
		// not a record either.
		"FIFO holding a valid record": func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			// A reader keeps the FIFO, and what is written to it, alive
			// once the writer has closed.
			r, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.Close(r) })
			w, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = w.WriteString(valid)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatal(err)
			}
		},
		// The file's name is this namespace's, its content another's.
		"another namespace's record": write(recordingStartRecord{pidNS: 0xF0000999, init: 7, start: 500}.String()),
		"another init's record":      write(recordingStartRecord{pidNS: 0xF0000123, init: 8, start: 500}.String()),
		"owned by another user": func(t *testing.T, path string) {
			if os.Geteuid() != 0 {
				t.Skip("needs root to give the file to another user")
			}
			write(valid)(t, path)
			if err := os.Chown(path, 65534, 65534); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ns := newPod(t)
			plant(t, ns.record())
			got := make(chan uint64, 1)
			go func() { got <- ns.load(t, 1000) }()
			select {
			case v := <-got:
				if v != 1000 {
					t.Fatalf("recording_start_time %d, want this agent's load time 1000", v)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the load did not return: it is blocked on what is at the record's path")
			}
			ns.onlyTheRecord(t, 1000)
			if got := ns.load(t, 2000); got != 1000 {
				t.Fatalf("after the replacement a restart got %d, want 1000", got)
			}
			if b, err := os.ReadFile(elsewhere); err != nil || string(b) != valid {
				t.Fatalf("the replacement wrote through a link: %s is now %q (%v)", elsewhere, b, err)
			}
		})
	}
}

// Agents of one namespace loading at once must all end up with the one value
// the record keeps, leaving the record and nothing else: at the same instant,
// and with a clock that moves on while they load, as CLOCK_BOOTTIME does. A
// record another agent kept since this one first read the clock is not in the
// future and must be taken, not replaced.
func TestAgentsLoadingAtOnceAgreeOnTheRecordingStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clock func() func() (uint64, error)
	}{
		{"same instant", func() func() (uint64, error) {
			return func() (uint64, error) { return 1000, nil }
		}},
		{"moving clock", func() func() (uint64, error) {
			var c atomic.Uint64
			c.Store(1000)
			return func() (uint64, error) { return c.Add(1), nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for round := 0; round < 100; round++ {
				ns := newPod(t)
				src := ns.source(tc.clock())
				got := make([]uint64, 8)
				var wg sync.WaitGroup
				for i := range got {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						got[i] = recordingStart(zap.NewNop(), src)
					}(i)
				}
				wg.Wait()
				for i, v := range got {
					if v != got[0] {
						t.Fatalf("round %d: agents got %v; agent %d disagrees with agent 0", round, got, i)
					}
				}
				ns.onlyTheRecord(t, got[0])
			}
		})
	}
}

// A valid record kept by another agent of the namespace between this agent's
// look and its own write is taken, and left as it is.
func TestKeepTakesARecordKeptSinceTheAgentLooked(t *testing.T) {
	ns := newPod(t)
	theirs := recordingStartRecord{pidNS: ns.pidNS, init: ns.init, start: 900}
	if err := os.WriteFile(ns.record(), []byte(theirs.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	src := ns.source(func() (uint64, error) { return 1000, nil })
	got, err := src.keep(ns.record(), recordingStartRecord{pidNS: ns.pidNS, init: ns.init, start: 1000})
	if err != nil || got != 900 {
		t.Fatalf("keep = %d, %v; want the other agent's 900", got, err)
	}
	ns.onlyTheRecord(t, 900)
}

// Without anywhere to keep the record the agent still records, from its own
// load time; without a clock the exclusion is off, as before.
func TestRecordingStartWithoutSharedMemoryOrClock(t *testing.T) {
	ns := newPod(t)
	ns.shm = filepath.Join(ns.shm, "missing")
	if got := ns.load(t, 1000); got != 1000 {
		t.Fatalf("no /dev/shm: recording_start_time %d, want the load time 1000", got)
	}

	got := recordingStart(zap.NewNop(), recordingStartSource{
		now: func() (uint64, error) { return 0, errors.New("no CLOCK_BOOTTIME") },
	})
	if got != 0 {
		t.Fatalf("no clock: recording_start_time %d, want 0 (exclusion off)", got)
	}

	for name, src := range map[string]recordingStartSource{
		"PID namespace": {pidNS: func() (uint64, error) { return 0, errors.New("no /proc") }},
		"IPC namespace": {pidNS: func() (uint64, error) { return 0xF0000123, nil }, ipcNS: func() (uint64, error) { return 0, errors.New("no /proc") }},
		"PID 1": {pidNS: func() (uint64, error) { return 0xF0000123, nil }, ipcNS: func() (uint64, error) { return 0xF0000456, nil },
			initStart: func() (uint64, error) { return 0, errors.New("hidepid") }},
	} {
		src.now = func() (uint64, error) { return 1000, nil }
		src.shmDir = t.TempDir()
		if got := recordingStart(zap.NewNop(), src); got != 1000 {
			t.Errorf("unreadable %s: recording_start_time %d, want the load time 1000", name, got)
		}
	}
}

func TestProcStartTicks(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		stat string
		want uint64
		ok   bool
	}{
		"plain": {"1 (pause) S 0 1 1 0 -1 4194560 92 0 0 0 0 0 0 0 20 0 1 0 4242 1003520 1 18446744073709551615\n", 4242, true},
		"command name with spaces and parentheses": {"77 (a) b) (c d) S 1 77 77 0 -1 4194560 92 0 0 0 0 0 0 0 20 0 1 0 31337 1003520 1\n", 31337, true},
		"truncated":       {"1 (pause) S 0 1 1 0 -1\n", 0, false},
		"no command name": {"1 pause S 0 1 1 0 -1 4194560 92 0 0 0 0 0 0 0 20 0 1 0 4242\n", 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte(tc.stat), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := procStartTicks(p)
			if tc.ok && (err != nil || got != tc.want) {
				t.Fatalf("procStartTicks = %d, %v; want %d", got, err, tc.want)
			}
			if !tc.ok && err == nil {
				t.Fatalf("procStartTicks = %d, want an error", got)
			}
		})
	}
}

// The real sources read this process's own namespaces and its PID 1.
func TestHostRecordingStartReadsThisProcess(t *testing.T) {
	src := hostRecordingStart()
	pidNS, err := src.pidNS()
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := GetSelfInodeNumber()
	if err != nil {
		t.Fatal(err)
	}
	if pidNS != want {
		t.Fatalf("PID namespace inode %d, want %d (/proc/self/ns/pid)", pidNS, want)
	}
	if _, err := src.ipcNS(); err != nil {
		t.Fatal(err)
	}
	self, err := procStartTicks("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	init, err := src.initStart()
	if err != nil {
		t.Skipf("PID 1 is not readable here: %v", err)
	}
	if init > self {
		t.Fatalf("PID 1 started at tick %d, after this process (%d)", init, self)
	}
	now, err := src.now()
	if err != nil || now == 0 {
		t.Fatalf("CLOCK_BOOTTIME: %d, %v", now, err)
	}
	if now/1e7 < self { // ticks are 10ms (USER_HZ 100)
		t.Fatalf("CLOCK_BOOTTIME %dns is before this process started (tick %d)", now, self)
	}
}
