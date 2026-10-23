//go:build linux

package utils

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// commandEnviron is the environment keploy gives a command it starts, before
// what the caller adds: keploy's own, with what changed in it before keploy's
// code ran as keploy was started with it.
//
// Something changes it then where a file capability starts keploy (a keploy
// binary given CAP_SYS_ADMIN with setcap, the remedy utils/appns names for a
// keploy run as a user): the kernel starts keploy in secure mode, and glibc's
// loader (keploy's release binary is linked dynamically, for cgo) removes from
// keploy's environment every variable on its list of those unsafe for a
// privileged program: LD_PRELOAD, LD_LIBRARY_PATH, TMPDIR, LOCALDOMAIN,
// RES_OPTIONS and the rest (MALLOC_* too, from glibc 2.39). Go's runtime may
// also set GOTRACEBACK=none (where it sees secure mode: not where the loader
// removed a variable, as the runtime then misses the auxiliary vector that
// says so). The command is no such program: it runs as keploy's user, and exec
// gives it none of keploy's capabilities, so it gets what it gets from a
// keploy without the file capability. A variable keploy itself has set or
// unset since stays as keploy has it, but for one the loader removed and
// keploy then unset, which looks the same as one keploy never touched, and
// comes back (no code of keploy's unsets one). keploy's own environment stays
// as the loader and the runtime left it: the variables go to the command
// alone. Where keploy could not read the environment it was started with, it
// says so once, at INFO.
func commandEnviron(logger *zap.Logger) []string {
	if launch.err != nil {
		launchSaid.Do(func() {
			logger.Info("keploy holds capabilities its user lacks and could not read the environment it was started with, "+
				"so the commands it starts get keploy's own: where a file capability started keploy, glibc's loader removed "+
				"LD_PRELOAD, LD_LIBRARY_PATH, TMPDIR and the like from it, and those commands get them no more",
				zap.Error(launch.err))
		})
	}
	return withInitialOf(os.Environ(), launch.start, launch.initial)
}

// launch is keploy's environment as it was started: initial, as the kernel
// started keploy with it, and start, as keploy's code first sees it
// (os.Environ). Both are nil unless keploy holds capabilities its user lacks
// (fileCapable); err says why keploy, which does, could not read them. They are read at this
// package's initialization, before keploy's own code can change the
// environment.
var launch = readLaunch()

// launchSaid makes the INFO line about launch.err once per keploy process.
var launchSaid sync.Once

type launchEnviron struct {
	start, initial []string
	err            error
}

func readLaunch() launchEnviron {
	ruid, euid, suid := unix.Getresuid()
	rgid, egid, sgid := unix.Getresgid()
	permitted, err := permittedCaps()
	if err != nil || !fileCapable(ruid, euid, suid, rgid, egid, sgid, permitted, false) {
		return launchEnviron{}
	}
	ambient, err := ambientCaps()
	if err != nil {
		// Not restored: whether exec would hand keploy's capabilities on is
		// unknown. Said once, as a failed read is.
		return launchEnviron{err: fmt.Errorf("read keploy's ambient capabilities: %w", err)}
	}
	if !fileCapable(ruid, euid, suid, rgid, egid, sgid, permitted, ambient) {
		return launchEnviron{}
	}
	initial, err := initialEnviron()
	if err != nil {
		return launchEnviron{err: err}
	}
	return launchEnviron{start: os.Environ(), initial: initial}
}

// fileCapable reports whether a process with these ids, this permitted
// capability set and ambient capabilities or none holds capabilities its user
// would not have, as a file capability gives a process that is not root, and
// none it would hand on: the one case in which keploy gives its commands back
// what the loader removed, as exec then gives them none of keploy's
// capabilities. Not a keploy with ambient capabilities, which its commands
// keep (and which an LSM's secure exec may have meant to scrub), nor a
// set-user-ID or set-group-ID keploy, whose commands keep its other user or
// group: their commands get keploy's environment as the loader left it.
func fileCapable(ruid, euid, suid, rgid, egid, sgid int, permitted uint64, ambient bool) bool {
	sameIDs := ruid == euid && ruid == suid && rgid == egid && rgid == sgid
	return sameIDs && ruid != 0 && permitted != 0 && !ambient
}

// ambientCaps reports whether the calling thread has any ambient capability.
func ambientCaps() (bool, error) {
	for c := 0; c < 64; c++ {
		set, err := unix.PrctlRetInt(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_IS_SET, uintptr(c), 0, 0)
		if errors.Is(err, unix.EINVAL) {
			return false, nil // past the kernel's last capability
		}
		if err != nil {
			return false, err
		}
		if set == 1 {
			return true, nil
		}
	}
	return false, nil
}

// permittedCaps is the calling thread's permitted capability set.
func permittedCaps() (uint64, error) {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return 0, err
	}
	return uint64(data[0].Permitted) | uint64(data[1].Permitted)<<32, nil
}

// initialEnviron is the environment the kernel started this process with:
// the strings it put in the process's memory at exec, between the env_start
// and env_end /proc/self/stat says, which nothing of keploy's writes (the
// loader's removals only reorder the pointers to them). They are the bytes
// /proc/self/environ holds, which the kernel makes root's (0400) in a process
// a file capability starts, read here with process_vm_readv instead, which
// asks for no file's permission: a process may always read its own memory.
func initialEnviron() ([]string, error) {
	stat, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return nil, err
	}
	start, end, err := environBounds(stat)
	if err != nil {
		return nil, err
	}
	if end == start {
		return []string{}, nil
	}
	buf := make([]byte, end-start)
	local := []unix.Iovec{{Base: &buf[0]}}
	local[0].SetLen(len(buf))
	remote := []unix.RemoteIovec{{Base: uintptr(start), Len: len(buf)}}
	n, err := unix.ProcessVMReadv(os.Getpid(), local, remote, 0)
	if err != nil {
		return nil, fmt.Errorf("read the environment's strings: %w", err)
	}
	if n != len(buf) {
		return nil, fmt.Errorf("read %d of the environment's %d bytes", n, len(buf))
	}
	return strings.Split(string(bytes.TrimRight(buf, "\x00")), "\x00"), nil
}

// environBounds is env_start and env_end, fields 50 and 51 of a
// /proc/PID/stat, whose second field (the command's name, in parentheses)
// may hold blanks and parentheses of its own.
func environBounds(stat []byte) (start, end uint64, err error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, 0, fmt.Errorf("no command name in /proc/self/stat")
	}
	fields := strings.Fields(string(stat[i+1:])) // field 3 on
	const envStart, envEnd = 50, 51
	if len(fields) < envEnd-2 {
		return 0, 0, fmt.Errorf("/proc/self/stat has %d fields, not env_start and env_end", len(fields)+2)
	}
	if start, err = strconv.ParseUint(fields[envStart-3], 10, 64); err == nil {
		end, err = strconv.ParseUint(fields[envEnd-3], 10, 64)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("/proc/self/stat's env_start and env_end: %w", err)
	}
	if start == 0 || end < start {
		return 0, 0, fmt.Errorf("/proc/self/stat shows no environment (env_start %d, env_end %d)", start, end)
	}
	return start, end, nil
}

// withInitialOf is env with each variable whose value (or absence) start
// does not have from initial as initial has it, where env still has it as
// start does: what was changed before keploy's code ran, and keploy has not
// changed since. Its other entries keep their order; those taken from initial
// come after them, in initial's. With no start, it is env.
func withInitialOf(env, start, initial []string) []string {
	if start == nil {
		return env
	}
	startVars, initialVars, envVars := environMap(start), environMap(initial), environMap(env)
	undo := map[string]bool{}
	for _, vars := range []map[string]string{startVars, initialVars} {
		for name := range vars {
			s, inStart := startVars[name]
			i, inInitial := initialVars[name]
			e, inEnv := envVars[name]
			if (inStart != inInitial || s != i) && inEnv == inStart && e == s {
				undo[name] = true
			}
		}
	}
	if len(undo) == 0 {
		return env
	}
	out := make([]string, 0, len(env)+len(undo))
	for _, e := range env {
		if name, _, _ := strings.Cut(e, "="); !undo[name] {
			out = append(out, e)
		}
	}
	// The first of each, the one os.Getenv would have read.
	for _, e := range initial {
		if name, _, ok := strings.Cut(e, "="); ok && undo[name] {
			out = append(out, e)
			delete(undo, name)
		}
	}
	return out
}

// environMap is env's variables, each with its first value, as os.Getenv
// reads them; an entry without '=' names none.
func environMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		name, value, ok := strings.Cut(e, "=")
		if !ok || name == "" {
			continue
		}
		if _, seen := m[name]; !seen {
			m[name] = value
		}
	}
	return m
}
