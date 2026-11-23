package mockdb

import (
	"bufio"
	"bytes"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sync"

	"go.keploy.io/server/v3/pkg/platform/yaml"
)

// A whole-file pass over a set's mock file (a replay's read of it, a prune, a
// write-back of what a round learned) never holds the file's lock for the
// length of the file.
//
// The file's lock (getMockFileLock) is what each append takes, and a mock file
// is only ever appended to between the rewrites that replace it: writeMockDoc
// writes a whole document and flushes it before it lets the lock go. So while
// the lock is held against appends the file ends on a document boundary, and
// the bytes before that point do not change until a rewrite replaces the file.
// A pass therefore takes the lock only to note the file's length
// (pinMockFile), reads up to that length with no lock, and a rewrite takes the
// lock again only to put its result in place: it copies what was appended
// meanwhile to the end of its replacement and renames it over the file
// (mockFileRewriter.commitLive). An append waits for that copy, which is as
// long as what came in during the pass, never for the pass.
//
// A recording that an auto-replay round replays and prunes while it records
// (k8s-proxy's) appends to the very file the round reads and rewrites. Holding
// the lock for the whole pass held the recorder's every append: 49 s for a
// 658 MB file, past the recorder's own hand-off bounds, which then gave the
// mock stream up for the rest of the recording.

// mockFileRewriteStripes serialise the rewrites of a mock file with each other
// for their whole pass: two at once would each put in place a file without
// what the other changed. Appends and reads never take them.
var mockFileRewriteStripes [mockFileLockStripeCount]sync.Mutex

// withRewriteLock runs fn, a whole-file rewrite of the set's mock file (in
// any of its formats), holding the file's rewrite lock.
func withRewriteLock(path, mockFileName string, fn func() error) error {
	key := mockFileKey(path, mockFileName)
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	rewrite := &mockFileRewriteStripes[hasher.Sum32()%mockFileLockStripeCount]
	rewrite.Lock()
	defer rewrite.Unlock()
	return fn()
}

// pinMockFile pins reader, a reader of the set's text mock file not read yet,
// to the file's length now, or to at's (yaml.MockReader.Pin), holding the
// file's lock against appends only for that. It returns what the reader was
// pinned to and that lock. The file is opened before the lock is taken: an
// append writes through the path, never the file a rewrite has put another in
// place of, so the file opened is whole whichever one it is.
func pinMockFile(path, mockFileName string, reader *yaml.MockReader, at os.FileInfo) (os.FileInfo, *sync.RWMutex, error) {
	lock := getMockFileLock(mockFileLockKey(path, mockFileName, reader.Format()))
	lock.RLock()
	defer lock.RUnlock()
	pinned, err := reader.Pin(at)
	return pinned, lock, err
}

// commitLive puts the rewrite in place of its file, which may have grown since
// the rewrite read it up to pinned: under lock, it copies the bytes appended
// after pinned's length, as the file holds them, to the end of the replacement,
// then puts the replacement in place (commit). Those are mocks recorded after
// the pass read the file, which it neither judged nor changed; an append
// waiting on the lock meanwhile lands in the replacement.
func (rw *mockFileRewriter) commitLive(lock *sync.RWMutex, pinned os.FileInfo) error {
	lock.Lock()
	defer lock.Unlock()
	if err := rw.copyAppended(pinned); err != nil {
		return err
	}
	return rw.commit()
}

// copyAppended copies the bytes of the file after pinned's length to the end
// of the replacement. A file replaced or cut since it was pinned is refused,
// and left as it is.
func (rw *mockFileRewriter) copyAppended(pinned os.FileInfo) error {
	f, err := yaml.OpenInRecording(rw.root, rw.targetPath)
	if err != nil {
		return fmt.Errorf("failed to open %s to keep what was appended to it while it was rewritten: %w", rw.targetPath, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat %s to keep what was appended to it while it was rewritten: %w", rw.targetPath, err)
	}
	if !os.SameFile(info, pinned) || info.Size() < pinned.Size() {
		return fmt.Errorf("mockdb: %s was replaced or cut while it was rewritten, so the rewrite is not put in its place", rw.targetPath)
	}
	appended := info.Size() - pinned.Size()
	if appended == 0 {
		return nil
	}
	if _, err := f.Seek(pinned.Size(), io.SeekStart); err != nil {
		return err
	}
	src := bufio.NewReader(io.LimitReader(f, appended))
	if rw.tmp == nil {
		if err := rw.open(); err != nil {
			return err
		}
	}
	if rw.format != yaml.FormatJSON && rw.written == 0 {
		// Each append after a file's first starts with its separator: the
		// replacement holds no document yet for it to separate.
		if head, err := src.Peek(len(yamlDocSeparator)); err == nil && bytes.Equal(head, []byte(yamlDocSeparator)) {
			_, _ = src.Discard(len(yamlDocSeparator))
		}
	}
	if _, err := io.Copy(rw.w, src); err != nil {
		return fmt.Errorf("failed to keep what was appended to %s while it was rewritten: %w", rw.targetPath, err)
	}
	rw.written++
	return nil
}

// yamlDocSeparator is what writeMockDoc writes before every document of a YAML
// mock file but its first.
const yamlDocSeparator = "---\n"

// A prune drops only mocks the round read.
//
// An auto-replay round reads the live set as it starts (its index read), and
// at its end prunes what none of its passing tests consumed, keeping what was
// requested after it began (pruneBefore). A mock requested just before the
// round began, still on its way from the agent when the round read the file,
// lands in the file after that read: no test of the round could consume it,
// and the prune dropped it, while its test case, recorded in the live set,
// is replayed by every later round without it. So a MockYaml notes how far
// it read each mock file (noteRead), and its prune keeps, unjudged, every
// mock past that point (pruneVerdict.unread); its own rewrite of the file
// that keeps every mock (persistLearned) carries the mark onto the file it
// puts in place (movedRead).

// readMark is how far a MockYaml read a set's mock file: the file, and the
// length it read it up to.
type readMark struct {
	info os.FileInfo
	size int64
}

// readMarks are a MockYaml's marks, by the file's key (withRewriteLock's).
type readMarks struct {
	mu sync.Mutex
	m  map[string]readMark
}

func mockFileKey(path, mockFileName string) string {
	key := filepath.Join(path, mockFileName)
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	return key
}

// noteRead notes that the set's mock file was read up to pinned's length: the
// mark moves only further into the file it was taken of, and to another file
// once one replaced it.
func (ys *MockYaml) noteRead(path, mockFileName string, pinned os.FileInfo) {
	if pinned == nil {
		return
	}
	key := mockFileKey(path, mockFileName)
	ys.reads.mu.Lock()
	defer ys.reads.mu.Unlock()
	if ys.reads.m == nil {
		ys.reads.m = map[string]readMark{}
	}
	if old, ok := ys.reads.m[key]; ok && os.SameFile(old.info, pinned) && old.size >= pinned.Size() {
		return
	}
	ys.reads.m[key] = readMark{info: pinned, size: pinned.Size()}
}

// readUpTo is how far the set's mock file pinned is was read, -1 when it was
// not read (a file put in place of the one read counts as not read).
func (ys *MockYaml) readUpTo(path, mockFileName string, pinned os.FileInfo) int64 {
	ys.reads.mu.Lock()
	defer ys.reads.mu.Unlock()
	mark, ok := ys.reads.m[mockFileKey(path, mockFileName)]
	if !ok || pinned == nil || !os.SameFile(mark.info, pinned) {
		return -1
	}
	return mark.size
}

// movedRead carries the mark of the file old onto now, the file a rewrite
// that kept every mock put in its place, where what was read ends at size.
// A mark of another file stays as it is.
func (ys *MockYaml) movedRead(path, mockFileName string, old, now os.FileInfo, size int64) {
	key := mockFileKey(path, mockFileName)
	ys.reads.mu.Lock()
	defer ys.reads.mu.Unlock()
	if mark, ok := ys.reads.m[key]; ok && os.SameFile(mark.info, old) && now != nil {
		ys.reads.m[key] = readMark{info: now, size: size}
	}
}

// forgetRead drops the set's mark: a prune put another file in place of
// the one read.
func (ys *MockYaml) forgetRead(path, mockFileName string) {
	ys.reads.mu.Lock()
	defer ys.reads.mu.Unlock()
	delete(ys.reads.m, mockFileKey(path, mockFileName))
}

// offset is how many bytes the replacement holds so far.
func (rw *mockFileRewriter) offset() int64 {
	if rw.tmp == nil {
		return 0
	}
	pos, err := rw.tmp.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0
	}
	return pos + int64(rw.w.Buffered())
}
