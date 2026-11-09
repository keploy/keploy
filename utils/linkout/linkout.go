// Package linkout says plainly why os.Root refused a path inside a folder
// whose files come with the repository keploy runs in, such as a recording:
// the link that leads out, or the name that does, and where it leads, in
// place of os.Root's own words.
package linkout

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Error is the error for a recording's file that keploy does not open
// because the file, or a link on its way, is a symbolic link os.Root does not
// follow: one that leads out of the recording, or one into it by an absolute
// path; or because the file's own name leads out ("..") or is an absolute
// path, which os.Root refuses as well. It says what keploy refused as it says
// any refusal of the file, then which link that is and where it leads, or
// where the name leads, in place of os.Root's own words; errors.Is and
// errors.As still see os.Root's error through it.
type Error struct {
	// Refused says what keploy refused, as it says it for any refusal of
	// the file, such as `the recording's file "F" is read only inside its
	// recording "R"`.
	Refused string
	// File is the recording's file, relative to the recording.
	File string
	// Recording is the recording's folder.
	Recording string
	// Link is where the link os.Root does not follow is, relative to the
	// recording, with every link before it followed: File itself, a folder
	// on its way, or a place a link on the way leads to. It is "" when
	// File's own name leads out or is absolute: no link is involved.
	Link string
	// Target is where Link leads: its own target when that is absolute, else
	// the place it names from the recording's folder. With no Link, it is
	// the place File names: itself when absolute, else joined to the
	// recording's folder.
	Target string
	// Inside says Target is an absolute path into the recording.
	Inside bool
	// Err is os.Root's refusal.
	Err error
}

func (e *Error) Error() string {
	if e.Link == "" {
		if e.Inside {
			return fmt.Sprintf("%s: it names %q by an absolute path, which keploy does not open; name it relative to %q", e.Refused, e.Target, e.Recording)
		}
		return fmt.Sprintf("%s: it names %q, a place outside it; name a place inside it", e.Refused, e.Target)
	}
	subject := "it is a link"
	if e.Link != e.File {
		subject = fmt.Sprintf("it leads through %q, a link", e.Link)
	}
	if e.Inside {
		return fmt.Sprintf("%s: %s by the absolute path %q, which keploy does not follow; make the link relative", e.Refused, subject, e.Target)
	}
	return fmt.Sprintf("%s: %s that leads out of it, to %q; replace the link with what it leads to", e.Refused, subject, e.Target)
}

func (e *Error) Unwrap() error { return e.Err }

// Explain is err, r's refusal of file, a path inside r, the recording
// at dir ("" is the working directory), as a *Error when file's own
// name leads out of r or is absolute, or file, or a link on its way, is a
// symbolic link r does not follow (see Error). refused is what the
// caller says for any refusal of file; the error says it first. It is nil
// when err is nil, a file that is not there, or anything else, which the
// caller says as before.
func Explain(r *os.Root, dir, file, refused string, err error) *Error {
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if dir == "" {
		dir = "."
	}
	if target, ok := namedOut(dir, file); ok {
		return &Error{Refused: refused, File: file, Recording: dir, Target: target, Inside: absoluteInto(dir, target), Err: err}
	}
	link, target, ok := linkOut(r, dir, file)
	if !ok {
		return nil
	}
	return &Error{Refused: refused, File: file, Recording: dir, Link: link, Target: target, Inside: absoluteInto(dir, target), Err: err}
}

// namedOut says where file, a path os.Root refuses by its name alone, leads:
// itself when absolute, or, when it climbs out by "..", the place it names
// from dir. ok is false for any other name, one that stays inside.
func namedOut(dir, file string) (target string, ok bool) {
	clean := filepath.Clean(file)
	if filepath.IsAbs(clean) {
		return clean, true
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return filepath.Join(dir, clean), true
	}
	return "", false
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
