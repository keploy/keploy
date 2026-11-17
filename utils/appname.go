package utils

import (
	"bufio"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// AppNameSource is what named an app that was given no name.
type AppNameSource string

const (
	// AppNameFromRemote is the git repository's origin remote: the last
	// segment of its URL, without .git.
	AppNameFromRemote AppNameSource = "the git remote origin"
	// AppNameFromRepo is the folder of a git repository with no origin
	// remote; for a linked worktree, the folder of the repository it belongs to.
	AppNameFromRepo AppNameSource = "the git repository's folder"
	// AppNameFromDir is the working directory, outside any git repository.
	AppNameFromDir AppNameSource = "the working directory"
)

// andPath says that the path rel of a folder inside the repository named
// the app too, after the repository's name.
func (s AppNameSource) andPath(rel string) AppNameSource {
	return s + AppNameSource(" and the path "+rel+" inside the repository")
}

// DefaultAppName is the name keploy gives an app a run was given no name for
// (no --app-name, no appName in keploy.yml): AppNameFor the working directory.
// It fails as GetLastDirectory does once the working directory is gone.
func DefaultAppName() (string, AppNameSource, error) {
	dir, err := WorkingDirectory()
	if err != nil {
		return "", "", err
	}
	name, from := AppNameFor(dir)
	return name, from, nil
}

// WorkingDirectory is the folder the process is in. It fails as
// GetLastDirectory does once that folder is gone.
func WorkingDirectory() (string, error) {
	return workingDirectory(os.Getwd)
}

// AppNameFor names the app recorded from dir. Inside a git repository it is
// the repository's name (its origin remote's, or, with no origin, the name of
// the repository's folder) at the repository's top folder, and below it that
// name, a "-", and dir's path inside the repository with each "/" a "-". So
// the services of a monorepo are separate apps: shop/services/orders of the
// repository shop is "shop-services-orders". A linked worktree's path is
// counted from the worktree's own top folder. Outside a repository the name
// is dir's own, as it was before keploy read repositories. Nothing else in
// the name changes: dots and spaces stay as the folders have them.
//
// The search stops at the user's home folder, and under sudo at the home of
// the user who ran sudo too: a repository there (dotfiles kept in ~/.git)
// never names the folders under it.
//
// It reads the repository's files and runs no git: a run as root in a
// checkout that another user owns, or in an image with no git installed,
// names the app the same way. A remote set only in a file the repository's
// config includes (include.path) is not read: that repository is named after
// its folder.
func AppNameFor(dir string) (string, AppNameSource) {
	dir = filepath.Clean(dir)
	top, name, common, ok := findGitRepository(dir, homeFolders())
	if !ok {
		return filepath.Base(dir), AppNameFromDir
	}
	from := AppNameFromRepo
	if remote := repoNameFromURL(originURL(filepath.Join(common, "config"))); remote != "" {
		name, from = remote, AppNameFromRemote
	}
	// top is dir or a folder above it, so Rel cannot fail.
	rel, _ := filepath.Rel(top, dir)
	if rel == "." {
		return name, from
	}
	rel = filepath.ToSlash(rel)
	return name + "-" + strings.ReplaceAll(rel, "/", "-"), from.andPath(rel)
}

// gitFileLimit bounds the reads of a repository's small files: a .git file,
// commondir, and config. A config past it is read up to it.
const gitFileLimit = 1 << 20

// findGitRepository finds the repository dir is in, as git does: the nearest
// folder at or above dir that holds .git, below the first of homes it meets.
// It returns that folder (top), the repository's name as its folder names it,
// and its common git directory, which holds the config. A linked worktree is
// named after the folder of the repository it was added from, so every
// worktree of one repository names the app alike.
func findGitRepository(dir string, homes []os.FileInfo) (top, name, common string, ok bool) {
	for d := dir; ; {
		if isOneOf(d, homes) {
			return "", "", "", false
		}
		dotGit := filepath.Join(d, ".git")
		if fi, err := os.Stat(dotGit); err == nil {
			gitDir := dotGit
			if fi.Mode().IsRegular() {
				// A linked worktree or a submodule: "gitdir: <path>".
				target, ok := readGitPointer(dotGit, "gitdir:")
				if !ok {
					return d, filepath.Base(d), dotGit, true
				}
				gitDir = resolveFrom(d, target)
			}
			common := gitDir
			if target, ok := readGitPointer(filepath.Join(gitDir, "commondir"), ""); ok {
				common = resolveFrom(gitDir, target)
			}
			if filepath.Clean(common) == filepath.Clean(gitDir) {
				return d, filepath.Base(d), common, true
			}
			// A linked worktree: the repository's own folder holds its
			// common git directory, or is it when that is bare (repo.git).
			if filepath.Base(common) == ".git" {
				return d, filepath.Base(filepath.Dir(common)), common, true
			}
			return d, strings.TrimSuffix(filepath.Base(common), ".git"), common, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", "", false
		}
		d = parent
	}
}

// lookupUser is user.Lookup; tests stand in a user of their own.
var lookupUser = user.Lookup

// homeFolders are the folders a repository search stops at: the user's home
// ($HOME), and under sudo the home of the user who ran sudo (SUDO_USER),
// which sudo may have replaced $HOME with root's. One that is not there is
// nil, and stops no search.
func homeFolders() []os.FileInfo {
	home, _ := os.UserHomeDir()
	paths := []string{home}
	if name := os.Getenv("SUDO_USER"); name != "" {
		if u, err := lookupUser(name); err == nil {
			paths = append(paths, u.HomeDir)
		}
	}
	homes := make([]os.FileInfo, 0, len(paths))
	for _, p := range paths {
		fi, _ := os.Stat(p)
		homes = append(homes, fi)
	}
	return homes
}

// isOneOf says whether the folder d is one of folders, by whatever path it
// was reached (a symlinked /home). A nil FileInfo, of a folder d's Stat
// failed for or of one that is not there, is the same file as none.
func isOneOf(d string, folders []os.FileInfo) bool {
	fi, _ := os.Stat(d)
	for _, f := range folders {
		if os.SameFile(fi, f) {
			return true
		}
	}
	return false
}

func resolveFrom(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// readGitPointer reads the one-line file git keeps a path in (.git's
// "gitdir: <path>", or commondir's bare path), after prefix.
func readGitPointer(path, prefix string) (string, bool) {
	b := readRegularFile(path, 4096)
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	return line, line != ""
}

// readRegularFile reads at most limit bytes of path, and only of a regular
// file: a FIFO named config would block the open for good. It is nil for a
// path that is not a regular file it can open.
func readRegularFile(path string, limit int64) []byte {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, limit))
	return b
}

// repoNameFromURL is the repository's name in a remote URL: its last path
// segment without .git, for https, ssh (git@host:org/repo) and local-path
// remotes alike.
func repoNameFromURL(u string) string {
	u = strings.TrimRight(strings.TrimSpace(u), `/\`)
	u = strings.TrimRight(strings.TrimSuffix(u, ".git"), `/\`)
	if i := strings.LastIndexAny(u, `/\:`); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// originURL is remote.origin.url in the git config file at path, the first
// one when it has several, as `git remote get-url origin` prints; "" when
// there is none.
func originURL(path string) string {
	b := readRegularFile(path, gitFileLimit)
	inOrigin := false
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	sc.Buffer(make([]byte, 0, 64*1024), gitFileLimit)
	pending := ""
	for sc.Scan() {
		line := strings.TrimSpace(pending + sc.Text())
		pending = ""
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				inOrigin = false
				continue
			}
			inOrigin = isOriginSection(line[1:end])
			line = strings.TrimSpace(line[end+1:])
		}
		// A value continues on the next line after a backslash that ends the
		// line and escapes nothing, outside a comment.
		if continues(line) {
			pending = strings.TrimSuffix(line, `\`)
			continue
		}
		if !inOrigin {
			continue
		}
		// A line with no "=" (a blank one among them) sets no url.
		key, value, hasValue := strings.Cut(line, "=")
		if !hasValue || !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		return gitConfigValue(value)
	}
	return ""
}

// isOriginSection says whether a section header's inside names
// [remote "origin"], or the older [remote.origin].
func isOriginSection(h string) bool {
	h = strings.TrimSpace(h)
	if i := strings.IndexAny(h, " \t"); i >= 0 {
		sub, opened := strings.CutPrefix(strings.TrimSpace(h[i+1:]), `"`)
		sub, closed := strings.CutSuffix(sub, `"`)
		return opened && closed && strings.EqualFold(h[:i], "remote") && sub == "origin"
	}
	// With no ".", sub is "" and names no remote.
	name, sub, _ := strings.Cut(h, ".")
	return strings.EqualFold(name, "remote") && strings.EqualFold(sub, "origin")
}

// continues says whether a config line goes on to the next one, as git reads
// it: the line ends with a backslash that escapes nothing (\\ is one
// backslash), outside a comment. A # or ; outside quotes starts a comment,
// which git ends at the line's end whatever it holds, a backslash among them.
func continues(line string) bool {
	quoted := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\':
			if i == len(line)-1 {
				return true
			}
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && (c == '#' || c == ';'):
			return false
		}
	}
	return false
}

// gitConfigValue is a config value as git reads it (config.c parse_value):
//   - quotes removed;
//   - \n, \t and \b read as a newline, a tab and a backspace, and a backslash
//     taking any other character after it as it is (git refuses a value with
//     one of those; a URL holds none);
//   - outside quotes, the spaces it starts and ends with dropped, and each
//     space or tab within it kept as a space;
//   - a # or ; outside quotes starting a comment.
func gitConfigValue(v string) string {
	var b strings.Builder
	quoted := false
	// spaces counts the unquoted spaces read since the last character kept:
	// written before the next one, dropped at the value's end.
	spaces := 0
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !quoted && (c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f') {
			if b.Len() > 0 {
				spaces++
			}
			continue
		}
		if !quoted && (c == '#' || c == ';') || c == '\\' && i == len(v)-1 {
			// A comment, or a backslash at the end that escapes nothing.
			break
		}
		b.WriteString(strings.Repeat(" ", spaces))
		spaces = 0
		switch c {
		case '"':
			quoted = !quoted
		case '\\':
			// Never at the end: that one ended the value above.
			if i++; i < len(v) {
				b.WriteByte(gitEscape(v[i]))
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// gitEscape is the character git reads for a backslash and c.
func gitEscape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'b':
		return '\b'
	}
	return c
}
