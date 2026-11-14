package mapdb

import (
	"context"
	"path/filepath"
	"sync"
)

// setWrites holds the write lock of each set's mapping file that a write
// holds or waits for.
//
// A write of a set's mapping file reads the file, changes what it read and
// writes the whole file back. Two writes that overlap each read the file
// before either writes it, and the second to write puts back a file without
// the first one's change: a test's entry, a worker mark, a delete. The lock
// makes the writes of one file one at a time. It belongs to the process, not
// to a MappingDb: the writers of a recording that goes on each build their
// own (k8s-proxy's recorder and its interval auto-replay round, over one
// directory; the round's hooks join them when they write mappings). A lock is in the table only while a write holds or waits for
// it, so a process that records for days keeps none for the sets it wrote.
//
// It is a lock of this process: another process writing the same file is not
// kept out (keploy writes a set's mapping file from one process at a time).
// Reads take no lock: a write replaces the file whole (a temp file renamed
// over it), so a read sees one write or the other, except where the rename
// fails on Windows and the old file is moved aside first. Writes made outside
// MappingDb (EncodeOverF's callers) do not take it.
var setWrites = writeLocks{held: map[string]*writeLock{}}

type writeLocks struct {
	mu   sync.Mutex
	held map[string]*writeLock
}

type writeLock struct {
	// token holds one value while a write holds the lock.
	token chan struct{}
	// users is the writes that hold the lock or wait for it.
	users int
}

// holdSet takes the write lock of testSetID's mapping file for a write of it,
// waiting while another write holds it, and returns its release. The wait
// ends with ctx: holdSet then returns ctx's error, and the write writes
// nothing. A lock no write holds is taken without looking at ctx, so such a
// write goes on as one did before there was a lock.
//
// Every MappingDb method that writes the file takes it before it reads the
// file, and releases it once it has written. It is not reentrant: a write
// holding it calls no other write.
func (db *MappingDb) holdSet(ctx context.Context, testSetID string) (release func(), err error) {
	return setWrites.hold(ctx, db.fileKey(testSetID))
}

// fileKey names testSetID's mapping file, whatever its format, the same way
// for every MappingDb given that directory, absolute or not. It does not
// resolve symlinks or fold case, so one file reached by two such spellings
// gets two locks; no caller reaches a set that way.
func (db *MappingDb) fileKey(testSetID string) string {
	fileName := db.MapFileName
	if fileName == "" {
		fileName = "mappings"
	}
	key := filepath.Join(db.path, testSetID, fileName)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	return key
}

func (l *writeLocks) hold(ctx context.Context, key string) (func(), error) {
	l.mu.Lock()
	w := l.held[key]
	if w == nil {
		w = &writeLock{token: make(chan struct{}, 1)}
		l.held[key] = w
	}
	w.users++
	l.mu.Unlock()

	select {
	case w.token <- struct{}{}:
	default:
		select {
		case w.token <- struct{}{}:
		case <-ctx.Done():
			l.leave(key, w)
			return nil, ctx.Err()
		}
	}
	return func() {
		<-w.token
		l.leave(key, w)
	}, nil
}

// leave drops a write from w's users, and w from the table with the last.
func (l *writeLocks) leave(key string, w *writeLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	w.users--
	if w.users == 0 {
		delete(l.held, key)
	}
}
