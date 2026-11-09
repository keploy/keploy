package utils

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// LinkOutError is the error for a recording's file that keploy does not open
// because the file, or a link on its way, is a symbolic link os.Root does not
// follow: one that leads out of the recording, or one into it by an absolute
// path. It says which link that is and where it leads, in place of os.Root's
// own words; errors.Is and errors.As still see os.Root's error through it.
type LinkOutError struct {
	// File is the recording's file, relative to the recording.
	File string
	// Recording is the recording's folder.
	Recording string
	// Link is where the link os.Root does not follow is, relative to the
	// recording, with every link before it followed: File itself, a folder
	// on its way, or a place a link on the way leads to.
	Link string
	// Target is where Link leads: its own target when that is absolute, else
	// the place it names from the recording's folder.
	Target string
	// Inside says Target is an absolute path into the recording.
	Inside bool
	// Err is os.Root's refusal.
	Err error
}

func (e *LinkOutError) Error() string {
	subject := fmt.Sprintf("the recording's file %q is a link", e.File)
	if e.Link != e.File {
		subject = fmt.Sprintf("the recording's file %q leads through %q, a link", e.File, e.Link)
	}
	if e.Inside {
		return fmt.Sprintf("%s by the absolute path %q: keploy follows a link in the recording %q only by a relative path that stays inside it; make the link relative", subject, e.Target, e.Recording)
	}
	return fmt.Sprintf("%s that leads out of the recording %q, to %q: keploy uses a recording's files only inside it; replace the link with what it leads to", subject, e.Recording, e.Target)
}

func (e *LinkOutError) Unwrap() error { return e.Err }

// RecordingLinkOut is err, r's refusal of file, a path inside r, the recording
// at dir ("" is the working directory), as a *LinkOutError when file, or a
// link on its way, is a symbolic link r does not follow (see LinkOutError).
// It is nil when err is nil, a file that is not there, or anything else.
func RecordingLinkOut(r *os.Root, dir, file string, err error) *LinkOutError {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if dir == "" {
		dir = "."
	}
	link, target, ok := linkOut(r, dir, file)
	if !ok {
		return nil
	}
	return &LinkOutError{File: file, Recording: dir, Link: link, Target: target, Inside: absoluteInto(dir, target), Err: err}
}

// maxLinkHops bounds how many links linkOut follows inside the recording, as
// the kernel bounds a lookup (ELOOP).
const maxLinkHops = 40

// linkOut walks rel inside r (the folder at) one name at a time, following
// each link r follows (a relative one that stays inside r), and finds the
// first link r does not follow: one with an absolute target, or one whose
// target climbs out of r. It says where that link is inside r and where it
// leads: its target when absolute, else the place it names, joined to at. ok
// is false when there is none: r refused rel for something else.
func linkOut(r *os.Root, at, rel string) (link, target string, ok bool) {
	clean := filepath.Clean(rel)
	if !filepath.IsLocal(clean) {
		return "", "", false
	}
	sep := string(filepath.Separator)
	// cur is a folder inside r, by a path with no link on it ("" is r);
	// rest is what is left to walk from it.
	cur, rest := "", strings.Split(clean, sep)
	for hops := 0; len(rest) > 0; {
		p := filepath.Join(cur, rest[0])
		rest = rest[1:]
		info, err := r.Lstat(p)
		if err != nil {
			return "", "", false
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			cur = p
			continue
		}
		dest, err := r.Readlink(p)
		if err != nil {
			return "", "", false
		}
		if filepath.IsAbs(dest) {
			return p, dest, true
		}
		next := filepath.Join(cur, dest)
		if !filepath.IsLocal(next) {
			return p, filepath.Join(at, next), true
		}
		if hops++; hops > maxLinkHops {
			return "", "", false
		}
		// The link is followed: walk its target from r, then what was left.
		cur, rest = "", append(strings.Split(next, sep), rest...)
	}
	return "", "", false
}

// absoluteInto says target, a link's absolute target, names a place in dir.
func absoluteInto(dir, target string) bool {
	if !filepath.IsAbs(target) {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(abs, target)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}
