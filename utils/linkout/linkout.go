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
	"runtime"
	"strings"
)

// Error is the error for a recording's file that keploy does not open
// because the file, or a link on its way, is a symbolic link os.Root does not
// follow: one that leads out of the recording, one into it by an absolute
// path, or one whose relative target climbs out of it and back in; or because
// the file's own name leads out ("..") or is an absolute path, which os.Root
// refuses as well. It says what keploy refused as it says
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
	// File's own name leads out or is absolute: no link is involved. It is
	// named with slashes on every OS, as a recording names its own files.
	Link string
	// Target is where Link leads: its own target when that is absolute, else
	// the place it names from the folder it is in, as os.Root walks it,
	// joined to the recording's folder. With no Link, it is the place File
	// names: itself when absolute, else, as os.Root walks it, with the links
	// on its way followed, joined to the recording's folder. It is a path on
	// disk, in the OS's own form.
	Target string
	// Inside says Target is a place in the recording named by an absolute
	// path: File's own name, or Link's own target.
	Inside bool
	// Back says Target is a place in the recording that File's own name, or
	// Link's relative target, reaches only by climbing out of the recording
	// first (".."), which os.Root refuses as it refuses any path that climbs
	// out.
	Back bool
	// Err is os.Root's refusal.
	Err error
}

func (e *Error) Error() string {
	if e.Link == "" {
		if e.Inside {
			return fmt.Sprintf("%s: it names %q by an absolute path, which keploy does not open; name it relative to %q", e.Refused, e.Target, e.Recording)
		}
		if e.Back {
			return fmt.Sprintf("%s: it names %q by a path that climbs out of it and back in, which keploy does not open; name it relative to %q without climbing out", e.Refused, e.Target, e.Recording)
		}
		return fmt.Sprintf("%s: it names %q, a place outside it; name a place inside it", e.Refused, e.Target)
	}
	subject := "it is a link"
	// File is the caller's name, perhaps not clean or, on Windows, with
	// backslashes: it is the link itself when it names the same place in the
	// same form as Link.
	if e.Link != filepath.ToSlash(filepath.Clean(e.File)) {
		subject = fmt.Sprintf("it leads through %q, a link", e.Link)
	}
	if e.Inside {
		return fmt.Sprintf("%s: %s by the absolute path %q, which keploy does not follow; make the link relative", e.Refused, subject, e.Target)
	}
	if e.Back {
		return fmt.Sprintf("%s: %s that climbs out of it and back in, to %q, which keploy does not follow; make the link lead there without climbing out", e.Refused, subject, e.Target)
	}
	return fmt.Sprintf("%s: %s that leads out of it, to %q; replace the link with what it leads to", e.Refused, subject, e.Target)
}

func (e *Error) Unwrap() error { return e.Err }

// Explain is err, r's refusal of file, a path inside r, the recording
// at dir ("" is the working directory), as a *Error when err is r's refusal
// of file for leading out of r (escape) because file's own name leads out of
// r or is absolute, or file, or a link on its way, is a symbolic link r does
// not follow (see Error). refused is what the caller says for any refusal of
// file; the error says it first. It is nil for anything else, which the
// caller says as before: no error, a file that is not there, or any other
// cause, such as a folder keploy may not change or a name that is there
// already, even at a link out that the operation did not follow (os.Root's
// Remove, Rename and Mkdir, and an O_EXCL create, do not follow a link at the
// end of the name).
func Explain(r *os.Root, dir, file, refused string, err error) *Error {
	if !errors.Is(err, escape(r)) {
		return nil
	}
	if dir == "" {
		dir = "."
	}
	e := &Error{Refused: refused, File: file, Recording: dir, Err: err}
	var place string
	var ok bool
	if e.Link, place, ok = walk(r, file); !ok {
		return nil
	}
	e.Link = filepath.ToSlash(e.Link)
	e.Target, e.Inside, e.Back = where(dir, place)
	return e
}

// escape is os.Root's error for a name that leads out of it (errPathEscapes
// in the standard library's os package, which does not export it): what r
// says, wrapped, for "..", which every root refuses before it looks at a
// file. It is never nil; for a closed r it is os.ErrClosed, and then the walk
// finds nothing.
func escape(r *os.Root) error {
	_, err := r.Stat("..")
	return errors.Unwrap(err)
}

// where says where place, a path the recording at dir does not open, leads:
// place itself when absolute, else place from dir. inside says place is
// absolute and in dir; back says place is relative and comes back into dir
// after it climbs out.
func where(dir, place string) (target string, inside, back bool) {
	if filepath.IsAbs(place) {
		return place, absoluteInto(dir, place), false
	}
	abs, err := filepath.Abs(dir)
	return filepath.Join(dir, place), false, err == nil && absoluteInto(abs, filepath.Join(abs, place))
}

// maxLinks is how many links os.Root follows in one lookup, the link out
// among them (rootMaxSymlinks in the standard library's os/root.go). Past
// it os.Root says ELOOP, too many links, not a link out; so walk gives up
// there too.
const maxLinks = 8

// cleansFirst says os.Root cleans a path, with the target of each link it
// follows put in place of the link, before it walks it, as it does on
// Windows (rootCleanPath in the standard library's os/root_windows.go).
// Elsewhere it walks the name as it is: a ".." leaves the folder the walk
// has reached, after the links before it are followed.
var cleansFirst = runtime.GOOS == "windows"

// part is a name on the path os.Root walks, and where it came from: by is
// the index, among the links the walk followed, of the link whose target
// holds it, or -1 for file's own name.
type part struct {
	name string
	by   int
}

// followed is a link the walk followed: where it is, from the recording,
// and how many names came after it then (the rest of the path, which the
// walk reaches only after the link's target).
type followed struct {
	at    string
	after int
}

// parse is os.Root's splitPathInRoot (os/root.go): s, a name or a link's
// target, split into names put between before and after, each marked by.
// It drops "" and "." (os.Root keeps a "." at the very end, which names the
// folder reached, as its absence does). With clean (os.Root on Windows) the
// whole path is cleaned first. out says os.Root refuses it at once: s is
// absolute or begins with a separator, or, cleaned, it climbs out.
func parse(s string, by int, before, after []part, clean bool) (parts []part, out bool) {
	if filepath.IsAbs(s) || strings.HasPrefix(filepath.ToSlash(s), "/") {
		return nil, true
	}
	if clean {
		all := make([]string, 0, len(before)+1+len(after))
		for _, p := range before {
			all = append(all, p.name)
		}
		all = append(all, s)
		for _, p := range after {
			all = append(all, p.name)
		}
		if s = filepath.Join(all...); !filepath.IsLocal(s) {
			return nil, true
		}
		before, after = nil, nil
	}
	parts = append([]part(nil), before...)
	for _, n := range strings.Split(filepath.ToSlash(s), "/") {
		if n != "" && n != "." {
			parts = append(parts, part{n, by})
		}
	}
	return append(parts, after...), false
}

// join is parts as a path from the recording.
func join(parts []part) string {
	names := make([]string, len(parts))
	for i, p := range parts {
		names[i] = p.name
	}
	return filepath.Join(names...)
}

// walk walks file inside r as os.Root does (doInRoot in the standard
// library's os/root_openat.go): one name at a time, with each link's target
// put in its place, a ".." taken against the folder reached, and at most
// maxLinks links. It stops where os.Root refuses file for leading out: at
// file's own name when absolute, at a link whose target is absolute, or at
// a ".." that climbs out of r. It says the link to blame, "" when it is
// file's own name, and the place that leads to: an absolute target as it is,
// else that link's target (or file) from r's folder, with the links before
// the ".." followed. ok is false when the walk reaches the end, or stops for
// anything else: r refused file for something else.
func walk(r *os.Root, file string) (link, place string, ok bool) {
	parts, out := parse(file, -1, nil, nil, cleansFirst)
	if out {
		return "", filepath.Clean(file), true
	}
	var links []followed
	for i := 0; i < len(parts); {
		if parts[i].name == ".." {
			if i == 0 {
				// It climbs out of r: blame the name, or the link, it is from.
				if by := parts[0].by; by >= 0 {
					return links[by].at, join(parts[:len(parts)-links[by].after]), true
				}
				return "", join(parts), true
			}
			// It leaves the folder before it, which the walk reached.
			parts = append(parts[:i-1], parts[i+1:]...)
			i--
			continue
		}
		at := join(parts[:i+1])
		info, err := r.Lstat(at)
		if err != nil {
			return "", "", false
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			// A file, at the end or where os.Root needs a folder: no link
			// on the way leads out.
			if !info.IsDir() {
				return "", "", false
			}
			i++
			continue
		}
		if len(links) == maxLinks {
			return "", "", false
		}
		dest, err := r.Readlink(at)
		if err != nil {
			return "", "", false
		}
		links = append(links, followed{at, len(parts) - i - 1})
		next, out := parse(dest, len(links)-1, parts[:i], parts[i+1:], cleansFirst)
		if out {
			if filepath.IsAbs(dest) {
				return at, dest, true
			}
			return at, filepath.Join(join(parts[:i]), dest), true
		}
		parts = next
	}
	return "", "", false
}

// absoluteInto says target, an absolute path, names a place in dir.
func absoluteInto(dir, target string) bool {
	if !filepath.IsAbs(target) {
		return false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	// filepath.IsLocal(".") is true: the recording itself is in it.
	rel, err := filepath.Rel(abs, target)
	return err == nil && filepath.IsLocal(rel)
}
