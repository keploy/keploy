package utils

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMain keeps to this test binary the descriptors it was handed open on
// exec (a CI runner's step hands it some): each is made close on exec before
// any test runs, so a child a test starts holds the descriptors the test
// gives it and no others (TestMarkExitCountsOpenFilesNotDescriptors counts
// its child's files), whichever tests run and in whatever order. Go opens
// its own close on exec, and a test gives a child one of its own through
// ExtraFiles, which is dup'd for the child; no test here passes on one the
// binary was handed. None is closed.
func TestMain(m *testing.M) {
	flag.Parse()
	var kept []os.DirEntry
	kept, errDescriptorsKept = keepDescriptorsToSelf()
	if testing.Verbose() && len(kept) > 0 {
		fmt.Fprintf(os.Stderr, "this test binary was handed descriptors %s open on exec: made close on exec\n", descriptorsOf("/proc/self/fd", kept))
	}
	os.Exit(m.Run())
}

// errDescriptorsKept is why TestMain could not keep the binary's descriptors
// to itself, if it could not: a row whose child holds more than it was given
// says so.
var errDescriptorsKept error

// keepDescriptorsToSelf makes each descriptor of this process above stderr
// that is open on exec close on exec, and is those it made so.
func keepDescriptorsToSelf() ([]os.DirEntry, error) {
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	var kept []os.DirEntry
	for _, e := range fds {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd <= 2 {
			continue
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		switch {
		case errors.Is(err, unix.EBADF): // closed since it was listed: the listing's own
		case err != nil:
			return kept, fmt.Errorf("descriptor %d's flags: %w", fd, err)
		case flags&unix.FD_CLOEXEC == 0:
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
				return kept, fmt.Errorf("descriptor %d made close on exec: %w", fd, err)
			}
			kept = append(kept, e)
		}
	}
	return kept, nil
}

// descriptorsOf is listed descriptors fds of fd directory dir with what each
// names, for a row to say what a process held.
func descriptorsOf(dir string, fds []os.DirEntry) string {
	var named []string
	for _, e := range fds {
		target, err := os.Readlink(filepath.Join(dir, e.Name()))
		if err != nil {
			target = "(" + err.Error() + ")"
		}
		named = append(named, e.Name()+" "+target)
	}
	return "[" + strings.Join(named, ", ") + "]"
}
