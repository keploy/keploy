package yaml

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"
)

// File is a file as it is stored: where, and its bytes.
type File struct {
	Path string
	Data []byte
}

// The names ReplaceFiles gives the files it writes beside a file it replaces.
// Each is written whole before the next step reads it.
const (
	// ReplaceNewSuffix is the file's new bytes, before they are in place.
	ReplaceNewSuffix = ".keploy-new"
	// ReplaceOldSuffix is the file as it was, kept until every file of the
	// group is in place. It is copied under ReplacePartSuffix and renamed to
	// this name once whole, so a file of this name is always the whole file.
	ReplaceOldSuffix = ".keploy-old"
	// ReplacePartSuffix is the file as it was while it is being copied.
	ReplacePartSuffix = ".keploy-part"
	// ReplaceAbsentSuffix is an empty file that says the target did not exist
	// before the group: putting the group back removes the target.
	ReplaceAbsentSuffix = ".keploy-absent"
)

// replaceSuffixes are the names of every file ReplaceFiles writes beside a
// target.
var replaceSuffixes = []string{ReplaceNewSuffix, ReplaceOldSuffix, ReplacePartSuffix, ReplaceAbsentSuffix}

// ErrReplaceInterrupted says a directory holds files an interrupted
// ReplaceFiles left (FindLeftovers).
var ErrReplaceInterrupted = errors.New("an interrupted write left its files behind")

// ReplaceFiles stores every file of files, or none of them: each is written
// beside its target first, and the targets are replaced only once every one
// of them is written. When a file cannot be written, nothing was replaced;
// when a target cannot be replaced, the targets already replaced are put back
// as they were, a target that did not exist removed. An error therefore means
// every file is as it was before the call, unless it says a file could not be
// put back, and then it names it.
//
// A process killed outright leaves files beside the targets, and which ones
// says how far it got (FindLeftovers): new bytes not yet in place mean it
// stopped before every target was replaced, and the copies of the files as
// they were put the group back; no new bytes left mean every target was
// replaced, and only the copies remain to delete. The steps happen in that
// order on stable storage too: each file is flushed before it is renamed,
// and each directory before the next step.
func ReplaceFiles(logger *zap.Logger, files []File) error {
	return ReplaceFilesWith(logger, ReplaceHooks{}, files)
}

// ReplaceHooks are the platform a ReplaceFilesWith call works through, for a
// test. Rename makes every rename (os.Rename when nil), so a test can make
// the platform refuse one and check what the group leaves. Step, when not
// nil, is called after each change the call makes on disk (a file created, a
// file written whole, a rename, a removal), so a test can stop the call there
// as a kill would and check what the next run is told.
type ReplaceHooks struct {
	Rename func(src, dst string) error
	Step   func()
}

// ReplaceFilesWith is ReplaceFiles through hooks.
func ReplaceFilesWith(logger *zap.Logger, hooks ReplaceHooks, files []File) error {
	r := replacer{logger: logger, rename: hooks.Rename, step: hooks.Step}
	if r.rename == nil {
		r.rename = os.Rename
	}
	if r.step == nil {
		r.step = func() {}
	}
	return r.replace(files)
}

// replacer is one ReplaceFiles call's platform (ReplaceHooks).
type replacer struct {
	logger *zap.Logger
	rename func(src, dst string) error
	step   func()
}

// staged is one file of a ReplaceFiles group: its target, its new bytes
// written beside it, and the target as it was ("" when there was none), or
// the marker that says there was none. part is the copy of the target while
// it is written.
type staged struct {
	dst, fresh, old, part, absent string
}

func (r replacer) replace(files []File) error {
	if r.logger == nil {
		r.logger = zap.L()
	}
	group := make([]staged, 0, len(files))
	// discard removes what staging wrote, once every target is as it was
	// (none touched, or every one put back). The new bytes go last: while any
	// is left, the next run reads the group as one that had not put every
	// file in place, and its steps leave the files as they were; with the
	// copies left last, it would read a group that had finished.
	discard := func() {
		for _, s := range group {
			for _, p := range []string{s.part, s.old, s.absent} {
				if p != "" {
					removeQuietly(r.logger, p)
					r.step()
				}
			}
		}
		for _, s := range group {
			if s.fresh != "" {
				removeQuietly(r.logger, s.fresh)
				r.step()
			}
		}
	}
	for _, f := range files {
		s, err := r.stage(f)
		// A file staged in part is discarded with the rest.
		group = append(group, s)
		if err != nil {
			discard()
			return fmt.Errorf("failed to write %s beside the file it replaces, so no file was changed: %w", filepath.Base(f.Path), err)
		}
	}
	// Every file beside its target is on stable storage before any target
	// changes.
	syncDirs(r.logger, group)
	for i, s := range group {
		err := ReplaceFileWith(r.logger, r.rename, s.fresh, s.dst)
		if err == nil {
			r.step()
			continue
		}
		kept := r.putBack(group[:i])
		if !r.inPlace(s) {
			kept = append(kept, filepath.Base(s.dst))
		}
		if len(kept) > 0 {
			return fmt.Errorf("failed to put %s in place (%w), and could not put back %s: each file as it was is beside it, named with the suffix %s, and the next run names the steps that put the group back",
				filepath.Base(s.dst), err, strings.Join(kept, ", "), ReplaceOldSuffix)
		}
		discard()
		return fmt.Errorf("failed to put %s in place, so every file was put back as it was: %w", filepath.Base(s.dst), err)
	}
	// Every target is replaced on stable storage before the copies go: a
	// copy removed is never one the group could still need.
	syncDirs(r.logger, group)
	for _, s := range group {
		for _, p := range []string{s.old, s.absent} {
			if p != "" {
				removeQuietly(r.logger, p)
				r.step()
			}
		}
	}
	return nil
}

// stage writes f's bytes beside its target, then the target as it is now
// beside it too (under ReplacePartSuffix, renamed to ReplaceOldSuffix once
// whole), or, when there is none, the marker that says so. No name may exist
// already: a file of that name is what an interrupted ReplaceFiles left, and
// is never written over. The staged it returns names only the files this
// call created.
func (r replacer) stage(f File) (staged, error) {
	s := staged{dst: f.Path}
	mode := os.FileMode(0o644)
	current, err := os.ReadFile(f.Path)
	existed := err == nil
	if !existed && !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	if existed {
		if info, statErr := os.Stat(f.Path); statErr == nil {
			mode = info.Mode().Perm()
		}
	}
	fresh := f.Path + ReplaceNewSuffix
	created, err := r.writeNew(fresh, f.Data, mode)
	if created {
		s.fresh = fresh
	}
	if err != nil {
		return s, err
	}
	if !existed {
		absent := f.Path + ReplaceAbsentSuffix
		created, err = r.writeNew(absent, nil, mode)
		if created {
			s.absent = absent
		}
		return s, err
	}
	part := f.Path + ReplacePartSuffix
	created, err = r.writeNew(part, current, mode)
	if created {
		s.part = part
	}
	if err != nil {
		return s, err
	}
	old := f.Path + ReplaceOldSuffix
	if _, err := os.Lstat(old); err == nil {
		return s, &os.PathError{Op: "create", Path: old, Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return s, err
	}
	if err := r.rename(part, old); err != nil {
		return s, err
	}
	r.step()
	s.part, s.old = "", old
	return s, nil
}

// writeNew writes data to a file that must not exist yet, and flushes it to
// stable storage, so the rename that puts it in place moves a whole file.
// created says the file is this call's, whether or not it was written whole.
func (r replacer) writeNew(path string, data []byte, mode os.FileMode) (created bool, err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return false, err
	}
	r.step()
	defer func() {
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}()
	if _, err := file.Write(data); err != nil {
		return true, err
	}
	if err := file.Sync(); err != nil {
		return true, err
	}
	r.step()
	return true, nil
}

// syncDirs flushes the directory of each target of group to stable storage,
// so the names written so far are there before the next step changes any.
// A platform that cannot flush a directory (Windows) skips it.
func syncDirs(logger *zap.Logger, group []staged) {
	done := map[string]bool{}
	for _, s := range group {
		dir := filepath.Dir(s.dst)
		if done[dir] {
			continue
		}
		done[dir] = true
		d, err := os.Open(dir)
		if err != nil {
			logger.Debug("could not open a directory to flush it", zap.String("dir", dir), zap.Error(err))
			continue
		}
		if err := d.Sync(); err != nil {
			logger.Debug("could not flush a directory", zap.String("dir", dir), zap.Error(err))
		}
		_ = d.Close()
	}
}

// inPlace reports whether the target of s, whose replace failed, is at its
// name. A failed replace leaves its target where it was (ReplaceFileWith),
// unless it had moved the target aside and could not move it back: the name
// is then free, and the file as it was is put there.
func (r replacer) inPlace(s staged) bool {
	if s.old == "" {
		return true
	}
	if _, err := os.Stat(s.dst); !errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return r.rename(s.old, s.dst) == nil
}

// putBack undoes the replaces of done, the last first: the file as it was is
// renamed back over its target, and a target that did not exist is removed.
// It returns the names of the targets it could not put back.
func (r replacer) putBack(done []staged) []string {
	var kept []string
	for i := len(done) - 1; i >= 0; i-- {
		s := done[i]
		var err error
		if s.old != "" {
			err = ReplaceFileWith(r.logger, r.rename, s.old, s.dst)
		} else if err = os.Remove(s.dst); errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
		if err != nil {
			r.logger.Error("failed to put a file back as it was", zap.String("file", s.dst), zap.Error(err))
			kept = append(kept, filepath.Base(s.dst))
			continue
		}
		r.step()
	}
	return kept
}

func removeQuietly(logger *zap.Logger, path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		logger.Warn("could not remove a file written beside the one it replaced; delete it", zap.String("file", path), zap.Error(err))
	}
}

// Leftovers is what an interrupted ReplaceFiles left in some directories (a
// process killed while it replaced its files, or a file it could not put
// back), and the steps that leave the group's files in one state.
type Leftovers struct {
	// Files is every file it left, sorted.
	Files []string
	// Finished says every target had been replaced: no new bytes were left
	// waiting (ReplaceNewSuffix), so the run was removing the copies it
	// kept, and the files are as it wrote them. The steps then only delete
	// the copies. (A copy in progress, ReplacePartSuffix, is written only
	// after its file's new bytes, so new bytes are left beside it.)
	Finished bool
	// Restore is, when the run had not finished, each file as it was
	// (ReplaceOldSuffix) and the target it is renamed back over.
	Restore [][2]string
	// Created is, when the run had not finished, each target it created
	// (one a ReplaceAbsentSuffix marker names) that is there: putting the
	// group back deletes it.
	Created []string
	// Remove is each file it left that Restore does not move: deleted.
	Remove []string
}

// FindLeftovers finds the files in dirs that an interrupted ReplaceFiles left,
// and the steps that put its group in one state (Leftovers). A directory that
// does not exist holds none.
func FindLeftovers(dirs ...string) (Leftovers, error) {
	var l Leftovers
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return Leftovers{}, err
		}
		for _, e := range entries {
			if leftSuffix(e.Name()) != "" {
				l.Files = append(l.Files, filepath.Join(dir, e.Name()))
			}
		}
	}
	sort.Strings(l.Files)
	l.Finished = true
	for _, f := range l.Files {
		if leftSuffix(f) == ReplaceNewSuffix {
			l.Finished = false
		}
	}
	for _, f := range l.Files {
		suffix := leftSuffix(f)
		target := strings.TrimSuffix(f, suffix)
		switch {
		case l.Finished:
			l.Remove = append(l.Remove, f)
		case suffix == ReplaceOldSuffix:
			l.Restore = append(l.Restore, [2]string{f, target})
		case suffix == ReplaceAbsentSuffix:
			if _, err := os.Lstat(target); err == nil {
				l.Created = append(l.Created, target)
			}
			l.Remove = append(l.Remove, f)
		default:
			l.Remove = append(l.Remove, f)
		}
	}
	return l, nil
}

// leftSuffix is the ReplaceFiles suffix name ends with, "" for none.
func leftSuffix(name string) string {
	for _, s := range replaceSuffixes {
		if strings.HasSuffix(name, s) {
			return s
		}
	}
	return ""
}

// Steps says what to do with the leftovers, each file named by name (a path
// relative to the set, say): put the group back as it was when the run had
// not finished, else delete the copies it kept.
func (l Leftovers) Steps(name func(string) string) string {
	names := func(files []string) string {
		out := make([]string, 0, len(files))
		for _, f := range files {
			out = append(out, name(f))
		}
		return strings.Join(out, ", ")
	}
	if l.Finished {
		return "it had put every file in place and was removing the copies it kept, so the files are as that run wrote them: delete " + names(l.Remove)
	}
	var steps []string
	for _, m := range l.Restore {
		steps = append(steps, "rename "+name(m[0])+" to "+name(m[1]))
	}
	if len(l.Created) > 0 {
		steps = append(steps, "delete "+names(l.Created)+", which that run created")
	}
	if len(l.Remove) > 0 {
		steps = append(steps, "delete "+names(l.Remove))
	}
	return "it had not put every file in place, so put them back as they were (or restore them from version control): " + strings.Join(steps, "; ")
}
