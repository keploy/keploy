package testdb

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Rekeyer keeps what names a test's recording in step when keploy rewrites
// that recording where it lies (design v14 §2.4, the re-key rule). An entry of
// a set's verdicts.yaml names the recordings it applies to by their hash; a
// command that rewrites one (normalize, templatize, the replay's denoise,
// auto-noise, cloud sync) moves an entry that named the old recording onto
// the new one, right after the write, or the entry goes stale and the test
// runs. keploy's enterprise build registers one (SetRekeyer); nothing does
// without it.
type Rekeyer interface {
	// Hash names the recording at path, a test case file of a set under
	// keployDir, as the entries do.
	Hash(keployDir, path string) (string, error)
	// Rekey moves what named test's recording old, in set, onto newHash.
	Rekey(keployDir, set, test, old, newHash string) error
}

var (
	rekeyerMu sync.RWMutex
	rekeyer   Rekeyer
)

// SetRekeyer registers r for every test case write of this process: testdb
// calls it, under the proven switch, on each write that replaces a test
// case's recording (InsertTestCase, UpdateTestCase, ReplaceFiles). nil
// removes it.
func SetRekeyer(r Rekeyer) {
	rekeyerMu.Lock()
	defer rekeyerMu.Unlock()
	rekeyer = r
}

func currentRekeyer() Rekeyer {
	rekeyerMu.RLock()
	defer rekeyerMu.RUnlock()
	return rekeyer
}

// rekey is one write's re-key: the recording's hash before the write, taken
// before anything of it is replaced (its assets included), and what to tell
// once the write is done.
type rekey struct {
	r         Rekeyer
	keployDir string
	set, test string
	path      string
	old       string
}

// beginRekey takes the hash of the recording at path, test's in set, that a
// write is about to replace. It is nil when nothing is re-keyed: without the
// switch or a Rekeyer, and on a write with no old file (a new recording names
// nothing yet). A recording that cannot be named is said, and re-keys
// nothing: an entry that named it goes stale, and the test runs.
func (ts *TestYaml) beginRekey(set, test, path string) *rekey {
	r := currentRekeyer()
	if r == nil || !provenswitch.On() || rewriting(ts.TcsPath, set) {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		// No recording (a name's reservation is an empty file).
		return nil
	}
	old, err := r.Hash(ts.TcsPath, path)
	if err != nil {
		ts.logger.Warn("keploy could not name a recording it is about to rewrite, so an entry of verdicts.yaml that names it is not moved onto the new one: that entry is now stale, and the test runs",
			zap.String("testSet", set), zap.String("testCase", test), zap.Error(err),
			zap.String("next_step", "quarantine the test again if it is still to be skipped"))
		return nil
	}
	return &rekey{r: r, keployDir: ts.TcsPath, set: set, test: test, path: path, old: old}
}

// done re-keys once the write put the new recording in place.
func (k *rekey) done(logger *zap.Logger) {
	if k == nil {
		return
	}
	newHash, err := k.r.Hash(k.keployDir, k.path)
	if err == nil && newHash != k.old {
		err = k.r.Rekey(k.keployDir, k.set, k.test, k.old, newHash)
	}
	if err != nil {
		logger.Warn("keploy rewrote a test's recording but could not move its entry of verdicts.yaml onto the new one: that entry is now stale, and the test runs",
			zap.String("testSet", k.set), zap.String("testCase", k.test), zap.Error(err),
			zap.String("next_step", "quarantine the test again if it is still to be skipped"))
	}
}

// beginRekeys is beginRekey for each test case file of files, a group
// ReplaceFiles stores: a file of a set's tests directory, in this store's
// format (TestCaseFile's). Any other file of the group (a set's config.yaml)
// names no recording.
func (ts *TestYaml) beginRekeys(files []yaml.File) []*rekey {
	if currentRekeyer() == nil || !provenswitch.On() {
		return nil
	}
	ext := "." + ts.Format.FileExtension()
	root := filepath.Clean(ts.TcsPath)
	var out []*rekey
	for _, f := range files {
		p := filepath.Clean(f.Path)
		if filepath.Ext(p) != ext {
			continue
		}
		dir := filepath.Dir(p)
		if filepath.Base(dir) != "tests" {
			continue
		}
		setDir := filepath.Dir(dir)
		if filepath.Dir(setDir) != root {
			continue
		}
		test := strings.TrimSuffix(filepath.Base(p), ext)
		if k := ts.beginRekey(filepath.Base(setDir), test, p); k != nil {
			out = append(out, k)
		}
	}
	return out
}

// SetRewrite is a command's rewrite of a set's recordings in several writes,
// some of them outside testdb: keploy sanitize, which redacts every test case
// file where it lies, and keploy normalize of a sanitized set, which
// desanitizes the set, normalizes it and sanitizes it again. Each recording is
// named before the first write and re-keyed once, when the command is done:
// a write in between re-keys nothing, as the recordings it leaves (a
// desanitized one) are no entry's.
type SetRewrite struct {
	r         Rekeyer
	logger    *zap.Logger
	keployDir string
	set       string
	key       string
	tests     []string
	paths     map[string]string
	old       map[string]string
}

var (
	rewritesMu sync.Mutex
	rewrites   = map[string]bool{}
)

// rewriteKey names keployDir's set however its path is spelled.
func rewriteKey(keployDir, set string) string {
	dir, err := filepath.Abs(keployDir)
	if err != nil {
		dir = filepath.Clean(keployDir)
	}
	return dir + "\x00" + set
}

// rewriting reports whether a SetRewrite of set is under way: a write to it
// then re-keys nothing of its own.
func rewriting(keployDir, set string) bool {
	rewritesMu.Lock()
	defer rewritesMu.Unlock()
	return rewrites[rewriteKey(keployDir, set)]
}

// BeginSetRewrite names every recording of set, under keployDir, before a
// command rewrites them (SetRewrite). It is nil, and its Done does nothing,
// without the switch or a Rekeyer, and inside another rewrite of the set,
// which re-keys for it.
func BeginSetRewrite(logger *zap.Logger, keployDir, set string) *SetRewrite {
	r := currentRekeyer()
	if r == nil || !provenswitch.On() || validateNameComponent("testSetID", set) != nil {
		return nil
	}
	key := rewriteKey(keployDir, set)
	rewritesMu.Lock()
	if rewrites[key] {
		rewritesMu.Unlock()
		return nil
	}
	rewrites[key] = true
	rewritesMu.Unlock()

	w := &SetRewrite{r: r, logger: logger, keployDir: keployDir, set: set, key: key, paths: map[string]string{}, old: map[string]string{}}
	dir := filepath.Join(keployDir, set, "tests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return w
	}
	for _, e := range entries {
		ext := filepath.Ext(e.Name())
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			continue
		}
		test := strings.TrimSuffix(e.Name(), ext)
		old, err := r.Hash(keployDir, path)
		if err != nil {
			logger.Warn("keploy could not name a recording it is about to rewrite, so an entry of verdicts.yaml that names it is not moved onto the new one: that entry is now stale, and the test runs",
				zap.String("testSet", set), zap.String("testCase", test), zap.Error(err),
				zap.String("next_step", "quarantine the test again if it is still to be skipped"))
			continue
		}
		w.tests = append(w.tests, test)
		w.paths[test], w.old[test] = path, old
	}
	return w
}

// Done re-keys each recording the command rewrote, as it now lies: one it
// left as it was, or removed, re-keys nothing.
func (w *SetRewrite) Done() {
	if w == nil {
		return
	}
	rewritesMu.Lock()
	delete(rewrites, w.key)
	rewritesMu.Unlock()
	for _, test := range w.tests {
		k := &rekey{r: w.r, keployDir: w.keployDir, set: w.set, test: test, path: w.paths[test], old: w.old[test]}
		if _, err := os.Lstat(k.path); err != nil {
			continue
		}
		k.done(w.logger)
	}
}
