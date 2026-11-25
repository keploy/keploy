//go:build linux

package linux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// defaultBPFInstanceLockDir lives on bpffs (/sys/fs/bpf), which every
	// Keploy agent container shares via its host bind mount (unlike /run, which
	// is container-private). It stays outside /sys/fs/bpf/keploy/ (owned by
	// enterprise per-instance pin dirs) and outside CleanOrphanedBPFPins's
	// /sys/fs/bpf/keploy_* glob patterns.
	defaultBPFInstanceLockDir = "/sys/fs/bpf/keploy-instances"
	// bpffs rejects '.' in directory/object names (EPERM in kernel/bpf/inode.c),
	// so use a hyphen suffix rather than ".lock".
	instanceLockSuffix = "-lock"
)

// staleLockReapAge keeps a freshly created lock directory safe from stale
// cleanup while its owner is between mkdir and flock(LOCK_EX). Variable (not
// const) so unit tests can set it to 0 to stress concurrent acquire vs reap.
var staleLockReapAge = 5 * time.Second

// defaultInstanceLockDir returns the shared bpffs directory used for
// cross-PID-namespace flock instance locks, or "" when /sys/fs/bpf is not
// present. It has no side effects (never creates directories and never falls
// back to world-writable /tmp).
func defaultInstanceLockDir() string {
	fi, err := os.Stat("/sys/fs/bpf")
	if err != nil || !fi.IsDir() {
		return ""
	}
	return defaultBPFInstanceLockDir
}

// instanceLock holds an exclusive kernel flock(2) on a lock directory inside a
// host-shared filesystem (bpffs in production). Directories are used rather
// than regular files so the same mechanism works identically on bpffs (which
// permits mkdir/rmdir/flock on directories but not regular file creation) and
// on standard filesystems in unit tests.
//
// Because flock(2) is enforced by the kernel across container PID namespaces
// and automatically released when the owning process exits or is SIGKILLed,
// sibling agents can reliably detect live instances even when /proc cannot see
// across PID namespaces.
type instanceLock struct {
	mu       sync.Mutex
	id       string
	path     string
	fd       int
	released bool
}

// ID returns the unique instance identifier for this lock.
func (l *instanceLock) ID() string {
	if l == nil {
		return ""
	}
	return l.id
}

// Release removes the lock directory (if its inode still matches the open fd)
// and releases the kernel flock. Safe to call multiple times.
func (l *instanceLock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	var firstErr error
	if l.fd >= 0 {
		if sameInode(l.fd, l.path) {
			if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				firstErr = err
			}
		}
		if err := unix.Close(l.fd); err != nil && firstErr == nil {
			firstErr = err
		}
		l.fd = -1
	}
	return firstErr
}

// acquireInstanceLock creates and exclusively locks a per-instance lock
// directory in lockDir. When id is empty, a fresh random 16-hex identifier is
// generated.
//
// Race resistance:
//   - If a sibling liveness probe momentarily holds LOCK_SH on the newly
//     created directory between mkdir and flock, taking blocking LOCK_EX waits
//     the microseconds until the probe closes its fd instead of failing with
//     EWOULDBLOCK. (When the caller supplies an explicit non-empty id, non-
//     blocking LOCK_EX|LOCK_NB is used so conflicting explicit claims fail fast.)
//   - After acquiring LOCK_EX, fstat(fd) is compared with lstat(path); if the
//     directory was removed concurrently, the fd is closed and the acquire
//     retries so a live owner never holds a lock on an unlinked directory.
func acquireInstanceLock(lockDir, id string) (*instanceLock, error) {
	if lockDir == "" {
		return nil, errors.New("instance lock dir is empty")
	}
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return nil, fmt.Errorf("create instance lock dir %s: %w", lockDir, err)
	}

	explicitID := id != ""
	const maxAttempts = 16
	for attempt := 0; attempt < maxAttempts; attempt++ {
		curID := id
		if curID == "" {
			var err error
			curID, err = randomInstanceID()
			if err != nil {
				return nil, fmt.Errorf("generate instance id: %w", err)
			}
		}
		lockPath := filepath.Join(lockDir, curID+instanceLockSuffix)
		if err := os.Mkdir(lockPath, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				if !explicitID {
					continue
				}
			} else {
				return nil, fmt.Errorf("create instance lock %s: %w", lockPath, err)
			}
		}

		fd, err := unix.Open(lockPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return nil, fmt.Errorf("open instance lock %s: %w", lockPath, err)
		}

		flockHow := unix.LOCK_EX
		if explicitID {
			flockHow |= unix.LOCK_NB
		}
		if err := flockRetryEINTR(fd, flockHow); err != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("flock instance lock %s: %w", lockPath, err)
		}

		// Verify the directory at lockPath is still the exact inode we locked.
		if !sameInode(fd, lockPath) {
			_ = unix.Close(fd)
			continue
		}

		return &instanceLock{
			id:   curID,
			path: lockPath,
			fd:   fd,
		}, nil
	}
	return nil, fmt.Errorf("failed to acquire instance lock in %s after retries", lockDir)
}

// hasOtherLiveInstanceLocks reports whether any other live Keploy instance
// holds an exclusive flock in lockDir (ignoring selfID when non-empty).
//
// Probing takes a shared non-blocking flock (LOCK_SH|LOCK_NB) read-only so
// multiple concurrent probes never interfere with each other and a freshly
// started owner waiting on LOCK_EX succeeds immediately once the probe closes.
// Dead lock directories older than staleLockReapAge are opportunistically
// removed under LOCK_EX|LOCK_NB with an inode match check.
func hasOtherLiveInstanceLocks(lockDir, selfID string) bool {
	if lockDir == "" {
		return false
	}
	entries, err := os.ReadDir(lockDir)
	if err != nil {
		return false
	}

	now := time.Now()
	liveFound := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, instanceLockSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, instanceLockSuffix)
		if id == "" || (selfID != "" && id == selfID) {
			continue
		}
		lockPath := filepath.Join(lockDir, name)
		fd, err := unix.Open(lockPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}

		err = unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			_ = unix.Close(fd)
			liveFound = true
			break
		}
		if err != nil {
			_ = unix.Close(fd)
			continue
		}

		// Unlocked: check whether this is an old stale lock directory safe to reap.
		var st unix.Stat_t
		shouldReap := unix.Fstat(fd, &st) == nil && now.Sub(time.Unix(st.Mtim.Sec, st.Mtim.Nsec)) >= staleLockReapAge
		if shouldReap {
			if unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) == nil && sameInode(fd, lockPath) {
				_ = os.Remove(lockPath)
			}
		}
		_ = unix.Close(fd)
	}
	return liveFound
}

func flockRetryEINTR(fd, how int) error {
	for {
		err := unix.Flock(fd, how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func sameInode(fd int, path string) bool {
	var fdSt, pathSt unix.Stat_t
	if unix.Fstat(fd, &fdSt) != nil || unix.Lstat(path, &pathSt) != nil {
		return false
	}
	return fdSt.Dev == pathSt.Dev && fdSt.Ino == pathSt.Ino
}

func randomInstanceID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
