package utils

import (
	"bufio"
	"io"
	"os"
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

// DefaultAppName is the name keploy gives an app a run was given no name for
// (no --app-name, no appName in keploy.yml): AppNameFor the working directory.
// It fails as GetLastDirectory does once the working directory is gone.
func DefaultAppName() (string, AppNameSource, error) {
	dir, err := workingDirectory(os.Getwd)
	if err != nil {
		return "", "", err
	}
	name, from := AppNameFor(dir)
	return name, from, nil
}

// AppNameFor names the app recorded from dir. Inside a git repository it is
// the repository's name: its origin remote's, or, with no origin, the name of
// the repository's folder. So a recording made from a subfolder of a checkout,
// or from a mktemp folder inside one, is named after the repository, not after
// "backend" or "tmp.Xa81bq". Outside a repository it is dir's own name.
//
// It reads the repository's files and runs no git: a run as root in a
// checkout that another user owns, or in an image with no git installed,
// names the app the same way. A remote set only in a file the repository's
// config includes (include.path) is not read: that repository is named after
// its folder.
func AppNameFor(dir string) (string, AppNameSource) {
	if top, common, ok := findGitRepository(dir); ok {
		if name := repoNameFromURL(originURL(filepath.Join(common, "config"))); name != "" {
			return name, AppNameFromRemote
		}
		return top, AppNameFromRepo
	}
	return filepath.Base(dir), AppNameFromDir
}

// gitFileLimit bounds the reads of a repository's small files: a .git file,
// commondir, and config. A config past it is read up to it.
const gitFileLimit = 1 << 20

// findGitRepository finds the repository dir is in, as git does: the nearest
// folder at or above dir that holds .git. It returns the repository's name
// as its folder names it (top) and its common git directory, which holds
// the config. A linked worktree's top is the folder of the repository it was
// added from, so every worktree of one repository names the app alike.
func findGitRepository(dir string) (top string, common string, ok bool) {
	for d := filepath.Clean(dir); ; {
		dotGit := filepath.Join(d, ".git")
		if fi, err := os.Stat(dotGit); err == nil {
			gitDir := dotGit
			if fi.Mode().IsRegular() {
				// A linked worktree or a submodule: "gitdir: <path>".
				target, ok := readGitPointer(dotGit, "gitdir:")
				if !ok {
					return filepath.Base(d), dotGit, true
				}
				gitDir = resolveFrom(d, target)
			}
			common := gitDir
			if target, ok := readGitPointer(filepath.Join(gitDir, "commondir"), ""); ok {
				common = resolveFrom(gitDir, target)
			}
			if filepath.Clean(common) == filepath.Clean(gitDir) {
				return filepath.Base(d), common, true
			}
			// A linked worktree: the repository's own folder holds its
			// common git directory, or is it when that is bare (repo.git).
			if filepath.Base(common) == ".git" {
				return filepath.Base(filepath.Dir(common)), common, true
			}
			return strings.TrimSuffix(filepath.Base(common), ".git"), common, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", false
		}
		d = parent
	}
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
	b, _ := readRegularFile(path, 4096)
	line := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0])
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
	return line, line != ""
}

// readRegularFile reads at most limit bytes of path, and only of a regular
// file: a FIFO named config would block the open for good.
func readRegularFile(path string, limit int64) ([]byte, bool) {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit))
	return b, err == nil
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
	b, _ := readRegularFile(path, gitFileLimit)
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
		// A value continues on the next line after a trailing backslash.
		if strings.HasSuffix(line, `\`) && !strings.HasSuffix(line, `\\`) {
			pending = strings.TrimSuffix(line, `\`)
			continue
		}
		if !inOrigin || line == "" {
			continue
		}
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
	name, sub, ok := strings.Cut(h, ".")
	return ok && strings.EqualFold(name, "remote") && strings.EqualFold(sub, "origin")
}

// gitConfigValue is a config value as git reads it: quotes removed, a
// backslash taking the character after it as it is, and a # or ; outside
// quotes starting a comment.
func gitConfigValue(v string) string {
	var b strings.Builder
	quoted := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\' && i+1 < len(v):
			i++
			b.WriteByte(v[i])
		case c == '"':
			quoted = !quoted
		case !quoted && (c == '#' || c == ';'):
			return strings.TrimSpace(b.String())
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimSpace(b.String())
}
