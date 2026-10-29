package mockdb

import (
	"container/list"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/cespare/xxhash/v2"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// A process that replays the same test set again and again (k8s-proxy's
// auto-replay replays a recorded set cycle after cycle) read the set's mock
// file whole each time: IndexTestSetMocks decodes every document of it, which
// is most of the CPU a replay spends before its first test. The index that
// read leaves is a small entry per mock (models.Mock.LeavePayloadOnDisk) and
// where each document is in the file, so a later read of a file of the same
// content can be answered from it without decoding a document.
//
// indexCache keeps those indexes, within a byte budget (SetIndexCacheBudget),
// least recently used first out. It is off (budget 0) until a process sets a
// budget: a CLI replay reads each set's file once, so a cache would only hold
// memory there.
//
// A kept index answers a read only of the same content, read by the same
// rules:
//   - The same file, unchanged: the same device and inode, size and
//     modification time, and the same first and last 64 KiB. This is a
//     second read of the file a process read before, and costs those
//     128 KiB.
//   - Another file of the same size whose whole content hashes the same
//     (xxhash64, seeded per process: hashSeed). A replay that downloads the set into a new directory each
//     time (k8s-proxy in DaemonSet mode) reads a new file each time, which no
//     file identity matches. That costs one pass over the file, which reads
//     no document.
//   - The same format (a json file is decoded otherwise), and the same
//     models.MockRulesGeneration: a registration (a models hook, or a YAML
//     mapper here) or the declared session calls changed since would decode
//     or derive the mocks otherwise. The generation a read began at is the
//     one its index is kept under, so a change during the read is one too.
//
// A read the file changed under (appended to by a live recording, rewritten)
// is not kept. A read the cache answers is what the read that made the index
// returned: the same pools in the same order, the same skipped documents,
// what it logged as it decoded (kept up to maxIndexLogs entries), and its
// mocks' payloads read back from the file being read. Each read gets its own
// copies of the entries (models.Mock.IndexEntryFrom), so what a replay does
// to its mocks never reaches the cache. One thing is not done again: a
// mock's derivation does not count in models.LegacyKindFallbackFires a
// second time.
type indexCache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	// lru holds *indexedFile, the most recently used first.
	lru list.List
}

// mockIndexCache is the process's cache of mock-file indexes.
var mockIndexCache indexCache

// SetIndexCacheBudget sets how many bytes of heap the process's cache of
// mock-file indexes may hold (see indexCache), evicting the least recently
// used until what it holds fits. 0, the default, turns it off and drops
// everything it holds. A process that replays the same test sets again and
// again (k8s-proxy's auto-replay) sets one at start.
func SetIndexCacheBudget(bytes int64) {
	mockIndexCache.mu.Lock()
	defer mockIndexCache.mu.Unlock()
	mockIndexCache.budget = max(bytes, 0)
	mockIndexCache.evictLocked()
}

// IndexCacheStats is what the process's cache of mock-file indexes holds and
// has done since the process started.
type IndexCacheStats struct {
	// Files and Bytes are the indexes held and their estimated heap.
	Files int
	Bytes int64
	// Budget is the byte budget (SetIndexCacheBudget).
	Budget int64
	// SameFileHits, ContentHits and Misses count the reads it was asked:
	// answered from the same file, unchanged; answered from a file of the
	// same content; and read whole.
	SameFileHits, ContentHits, Misses uint64
	// Evictions counts indexes dropped for room.
	Evictions uint64
}

var indexCacheCounts struct {
	sync.Mutex
	sameFile, content, misses, evictions uint64
}

// MockIndexCacheStats reports the process's cache of mock-file indexes.
func MockIndexCacheStats() IndexCacheStats {
	mockIndexCache.mu.Lock()
	st := IndexCacheStats{Files: mockIndexCache.lru.Len(), Bytes: mockIndexCache.used, Budget: mockIndexCache.budget}
	mockIndexCache.mu.Unlock()
	indexCacheCounts.Lock()
	defer indexCacheCounts.Unlock()
	st.SameFileHits, st.ContentHits, st.Misses, st.Evictions = indexCacheCounts.sameFile, indexCacheCounts.content, indexCacheCounts.misses, indexCacheCounts.evictions
	return st
}

func countIndexRead(sameFile, content, miss, evicted uint64) {
	indexCacheCounts.Lock()
	defer indexCacheCounts.Unlock()
	indexCacheCounts.sameFile += sameFile
	indexCacheCounts.content += content
	indexCacheCounts.misses += miss
	indexCacheCounts.evictions += evicted
}

func (c *indexCache) budgetNow() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget
}

func (c *indexCache) on() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.budget > 0
}

// evictLocked drops the least recently used indexes until what is held fits
// the budget. The caller holds mu.
func (c *indexCache) evictLocked() {
	var evicted uint64
	for c.used > c.budget && c.lru.Len() > 0 {
		f := c.lru.Remove(c.lru.Back()).(*indexedFile)
		c.used -= f.bytes
		evicted++
	}
	if evicted > 0 {
		countIndexRead(0, 0, 0, evicted)
	}
}

// indexedFile is the index of one mock file's content: what a read of it
// left, before the pools were built from it. It is never changed once kept.
type indexedFile struct {
	format yaml.Format
	size   int64
	gen    uint64
	// sum is xxhash64 (seeded with hashSeed) of the whole file, edges of its
	// first and last 64 KiB.
	sum, edges uint64
	// docs are the file's documents in file order.
	docs         []indexedDoc
	hasContent   bool
	connFailures int
	unmet        []models.UnmetRequirement
	// logs are what the read logged as it decoded the documents, at Info
	// and above, up to maxIndexLogs; logsDropped is how many more it logged.
	logs        []loggedEntry
	logsDropped int
	bytes       int64

	// seen is the files read with this content, the last maxSeenFiles; the
	// cache's mu guards it.
	seen []seenFile
}

// seenFile is a file a read found the content of an indexedFile in.
type seenFile struct {
	info os.FileInfo
	mod  time.Time
}

const maxSeenFiles = 4

// indexedDoc is one document of a mock file: the entries of the mocks it
// decoded to, each with its payload left in the file (src nil), or, for a
// document that decoded to none, what the read listed of it as skipped.
type indexedDoc struct {
	mocks   []*models.Mock
	skipped *skippedEntry
}

type skippedEntry struct {
	name     string
	kind     models.Kind
	windowed models.WindowedMock
	// inWindow says windowed is set: its request time could be read.
	inWindow bool
}

// fileProbe is the identity of the mock file a read opened, and, once a
// lookup worked them out, the hashes of its content.
type fileProbe struct {
	path   string
	format yaml.Format
	info   os.FileInfo
	gen    uint64
	// edges and sum are set when hasEdges and hasSum.
	edges, sum       uint64
	hasEdges, hasSum bool
}

func newFileProbe(reader *yaml.MockReader) (*fileProbe, error) {
	info, err := reader.Stat()
	if err != nil {
		return nil, err
	}
	return &fileProbe{path: reader.Path(), format: reader.Format(), info: info, gen: models.MockRulesGeneration()}, nil
}

// edgeBytes is how much of each end of a file the same-file check hashes.
const edgeBytes = 64 << 10

// hashSeed seeds every hash the cache keys by, a random one per process.
// xxhash64 is not collision-resistant: with a fixed seed, a recording made to
// collide with another set's file of the same size could be answered with
// that set's index in a process that serves several apps (k8s-proxy). A seed
// the recording's author cannot know leaves them no collision to aim at.
// xxhash64 is still no keyed MAC; the seed takes away the known target.
var hashSeed = func() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails: it crashes instead.
	return binary.LittleEndian.Uint64(b[:])
}()

// openSame opens the probe's file and checks it is still the one probed.
func (p *fileProbe) openSame() (*os.File, error) {
	f, err := os.Open(p.path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (!os.SameFile(info, p.info) || info.Size() < p.info.Size()) {
		err = errors.New("the mock file changed while it was read")
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// hashEdges sets p.edges: xxhash64 (seeded with hashSeed) of the first and
// the last edgeBytes of the probed size (the whole file when it is shorter
// than two of them).
func (p *fileProbe) hashEdges() error {
	if p.hasEdges {
		return nil
	}
	f, err := p.openSame()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	size := p.info.Size()
	h := xxhash.NewWithSeed(hashSeed)
	if size <= 2*edgeBytes {
		if _, err := io.Copy(h, io.NewSectionReader(f, 0, size)); err != nil {
			return err
		}
	} else {
		if _, err := io.Copy(h, io.NewSectionReader(f, 0, edgeBytes)); err != nil {
			return err
		}
		if _, err := io.Copy(h, io.NewSectionReader(f, size-edgeBytes, edgeBytes)); err != nil {
			return err
		}
	}
	p.edges, p.hasEdges = h.Sum64(), true
	return nil
}

// hashAllHook, when set, runs as hashAll starts its pass over the file: a
// test changes the file there.
var hashAllHook func()

// hashAll sets p.sum: xxhash64 (seeded with hashSeed) of the file's probed
// size, read once.
func (p *fileProbe) hashAll() error {
	if p.hasSum {
		return nil
	}
	if err := p.hashEdges(); err != nil {
		return err
	}
	if hashAllHook != nil {
		hashAllHook()
	}
	f, err := p.openSame()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := xxhash.NewWithSeed(hashSeed)
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(h, io.NewSectionReader(f, 0, p.info.Size()), buf); err != nil {
		return err
	}
	p.sum, p.hasSum = h.Sum64(), true
	return nil
}

// unchanged reports whether the probed file is still as it was probed: the
// same file, size and modification time.
func (p *fileProbe) unchanged() bool {
	info, err := os.Stat(p.path)
	return err == nil && os.SameFile(info, p.info) && info.Size() == p.info.Size() && info.ModTime().Equal(p.info.ModTime())
}

// lookup finds the kept index of the probed file's content, nil when none is
// kept (see indexCache for what matches).
func (c *indexCache) lookup(p *fileProbe) *indexedFile {
	c.mu.Lock()
	var sameSize bool
	var sameFile *indexedFile
	for e := c.lru.Front(); e != nil; e = e.Next() {
		f := e.Value.(*indexedFile)
		if f.format != p.format || f.size != p.info.Size() || f.gen != p.gen {
			continue
		}
		sameSize = true
		for _, s := range f.seen {
			if os.SameFile(s.info, p.info) && s.mod.Equal(p.info.ModTime()) {
				sameFile = f
				break
			}
		}
		if sameFile != nil {
			break
		}
	}
	c.mu.Unlock()
	if !sameSize {
		countIndexRead(0, 0, 1, 0)
		return nil
	}
	if sameFile != nil {
		if p.hashEdges() == nil && p.edges == sameFile.edges && c.use(sameFile, nil) {
			countIndexRead(1, 0, 0, 0)
			return sameFile
		}
	}
	if p.hashAll() != nil {
		countIndexRead(0, 0, 1, 0)
		return nil
	}
	c.mu.Lock()
	var found *indexedFile
	for e := c.lru.Front(); e != nil; e = e.Next() {
		f := e.Value.(*indexedFile)
		if f.format == p.format && f.size == p.info.Size() && f.gen == p.gen && f.sum == p.sum {
			found = f
			break
		}
	}
	c.mu.Unlock()
	if found == nil || !c.use(found, p) {
		countIndexRead(0, 0, 1, 0)
		return nil
	}
	countIndexRead(0, 1, 0, 0)
	return found
}

// use marks f the most recently used, and, when seen is set, that its file
// holds f's content. It is false when f was evicted meanwhile.
func (c *indexCache) use(f *indexedFile, seen *fileProbe) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for e := c.lru.Front(); e != nil; e = e.Next() {
		if e.Value != f {
			continue
		}
		c.lru.MoveToFront(e)
		if seen != nil {
			f.seen = append(f.seen, seenFile{info: seen.info, mod: seen.info.ModTime()})
			if len(f.seen) > maxSeenFiles {
				f.seen = append(f.seen[:0], f.seen[len(f.seen)-maxSeenFiles:]...)
			}
		}
		return true
	}
	return false
}

// keep adds f, the index of the probed file's content, evicting the least
// recently used for room. An index bigger than the whole budget is not kept.
func (c *indexCache) keep(f *indexedFile, p *fileProbe) {
	f.format, f.size, f.gen, f.sum, f.edges = p.format, p.info.Size(), p.gen, p.sum, p.edges
	f.seen = []seenFile{{info: p.info, mod: p.info.ModTime()}}
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.bytes > c.budget {
		return
	}
	for e := c.lru.Front(); e != nil; e = e.Next() {
		o := e.Value.(*indexedFile)
		if o.format == f.format && o.size == f.size && o.gen == f.gen && o.sum == f.sum {
			// Another read kept the same content first.
			c.lru.Remove(e)
			c.used -= o.bytes
			break
		}
	}
	c.lru.PushFront(f)
	c.used += f.bytes
	c.evictLocked()
}

// indexBuilder collects a read's index as it decodes the file. It gives up,
// and keeps nothing, once what it has collected outgrows budget.
type indexBuilder struct {
	f      indexedFile
	budget int64
	logs   *logSink
	full   bool
}

func newIndexBuilder(budget int64) *indexBuilder {
	return &indexBuilder{budget: budget, logs: &logSink{}}
}

// docOverhead is an indexedDoc's own heap, and a skippedDoc's.
var (
	docOverhead     = int64(unsafe.Sizeof(indexedDoc{})) + int64(unsafe.Sizeof([]*models.Mock{}))
	skippedOverhead = int64(unsafe.Sizeof(skippedEntry{}))
)

// add records one document: its mocks' entries, each copied as left on disk
// once the read is done with it, or what was listed of it as skipped.
func (b *indexBuilder) add(mocks []*models.Mock, skipped *skippedEntry) {
	if b.full {
		return
	}
	d := indexedDoc{skipped: skipped}
	b.f.bytes += docOverhead
	if skipped != nil {
		b.f.bytes += skippedOverhead + int64(len(skipped.name)+len(skipped.kind))
	}
	if len(mocks) > 0 {
		d.mocks = make([]*models.Mock, len(mocks))
		b.f.bytes += int64(len(mocks)) * int64(unsafe.Sizeof((*models.Mock)(nil)))
		for i, m := range mocks {
			d.mocks[i] = m.IndexEntryFrom(nil)
			if d.mocks[i] == nil {
				// Only a mock whose payload is on disk can be kept.
				b.full = true
				return
			}
			b.f.bytes += d.mocks[i].IndexEntryBytes()
		}
	}
	b.f.docs = append(b.f.docs, d)
	if b.f.bytes > b.budget {
		b.full = true
		b.f.docs = nil
	}
}

// finish keeps the index when the read was whole and the file is as it was
// when the read began. The check comes after the hash: only a file still as
// probed once hashed was hashed with the content the read decoded.
func (b *indexBuilder) finish(p *fileProbe, hasContent bool, connFailures int, unmet []models.UnmetRequirement) {
	if b.full || p.hashAll() != nil || !p.unchanged() {
		return
	}
	b.f.hasContent, b.f.connFailures = hasContent, connFailures
	b.f.unmet = append([]models.UnmetRequirement(nil), unmet...)
	b.f.bytes += int64(len(unmet)) * int64(unsafe.Sizeof(models.UnmetRequirement{}))
	b.f.logs, b.f.logsDropped = b.logs.take()
	b.f.bytes += b.logs.bytes
	mockIndexCache.keep(&b.f, p)
}

// decodeLogger is logger, with what is logged through it at Info and above
// also recorded for the index.
func (b *indexBuilder) decodeLogger(logger *zap.Logger) *zap.Logger {
	return logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, &logRecorder{sink: b.logs})
	}))
}

// maxIndexLogs is how many of what a read logged as it decoded an index
// keeps to log again.
const maxIndexLogs = 64

type loggedEntry struct {
	entry  zapcore.Entry
	fields []zapcore.Field
}

type logSink struct {
	mu      sync.Mutex
	entries []loggedEntry
	dropped int
	bytes   int64
}

func (s *logSink) take() ([]loggedEntry, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries, s.dropped
}

// logRecorder is a zapcore.Core that records each entry in its sink.
type logRecorder struct {
	fields []zapcore.Field
	sink   *logSink
}

func (r *logRecorder) Enabled(l zapcore.Level) bool { return l >= zapcore.InfoLevel }

func (r *logRecorder) With(fields []zapcore.Field) zapcore.Core {
	return &logRecorder{fields: append(r.fields[:len(r.fields):len(r.fields)], fields...), sink: r.sink}
}

func (r *logRecorder) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if r.Enabled(e.Level) {
		return ce.AddCore(e, r)
	}
	return ce
}

func (r *logRecorder) Write(e zapcore.Entry, fields []zapcore.Field) error {
	r.sink.mu.Lock()
	defer r.sink.mu.Unlock()
	if len(r.sink.entries) >= maxIndexLogs {
		r.sink.dropped++
		return nil
	}
	all := append(r.fields[:len(r.fields):len(r.fields)], fields...)
	r.sink.entries = append(r.sink.entries, loggedEntry{entry: e, fields: all})
	r.sink.bytes += int64(unsafe.Sizeof(loggedEntry{})) + int64(len(e.Message)) + int64(len(all))*int64(unsafe.Sizeof(zapcore.Field{}))
	return nil
}

func (r *logRecorder) Sync() error { return nil }

// logAgain logs what the read that made f logged as it decoded, through
// logger.
func (f *indexedFile) logAgain(logger *zap.Logger) {
	for _, l := range f.logs {
		if ce := logger.Check(l.entry.Level, l.entry.Message); ce != nil {
			ce.Write(l.fields...)
		}
	}
	if f.logsDropped > 0 {
		logger.Warn("the mock file's index was reused from an earlier read of the same content, which logged more as it decoded than is kept to log again",
			zap.Int("not_logged_again", f.logsDropped))
	}
}
