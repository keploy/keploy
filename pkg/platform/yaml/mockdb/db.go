// Package mockdb provides a mock database implementation.
package mockdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/pathsafe"
	"go.uber.org/zap"
)

// mockFormatGob is the on-disk extension for the binary gob mock
// format, enabled via KEPLOY_MOCK_FORMAT=gob. The format is a magic
// header followed by a single continuous gob stream of *models.Mock.
// Readers auto-detect by checking mocks.gob first, falling back to
// mocks.yaml.
const mockFormatGob = "gob"

// gobMockMagic is the version-tagged header written at the start of
// every mocks.gob file. Readers reject files whose first bytes don't
// match this constant. Bump the version suffix when a breaking change
// to the encoded Mock struct forces a format break — old files then
// fail fast at replay time with a clear error instead of silently
// decoding to a corrupt struct.
const gobMockMagic = "keploy-gob-v1\n"

// configuredMockFormat holds the mock format selected via config file
// (record.mockFormat). The env var KEPLOY_MOCK_FORMAT takes precedence
// so ad-hoc runs can override the file without editing it.
//
// Written once at startup from the OSS CLI provider; read by useGobMockFormat.
// No mutex — Go's package-var initialization barrier is sufficient.
var configuredMockFormat string

// SetConfiguredMockFormat is called by the OSS CLI after parsing the
// config file so mockdb knows the file-selected format. Pass "" to
// leave default (yaml).
func SetConfiguredMockFormat(format string) {
	configuredMockFormat = format
}

func useGobMockFormat() bool {
	if v := os.Getenv("KEPLOY_MOCK_FORMAT"); v != "" {
		return v == mockFormatGob
	}
	return configuredMockFormat == mockFormatGob
}

// useGobFormat is the per-instance gob-vs-structured decision: the
// KEPLOY_MOCK_FORMAT env override wins, then this db's per-session
// MockFormat (set by multi-app callers), then the package-global default.
// Single-session callers leave MockFormat empty and get exactly the old
// useGobMockFormat() behaviour.
func (ys *MockYaml) useGobFormat() bool {
	if v := os.Getenv("KEPLOY_MOCK_FORMAT"); v != "" {
		return v == mockFormatGob
	}
	if ys.MockFormat != "" {
		return ys.MockFormat == mockFormatGob
	}
	return useGobMockFormat()
}

type MockYaml struct {
	MockPath  string
	MockName  string
	Logger    *zap.Logger
	idCounter int64
	Format    yaml.Format

	// MockFormat is the per-instance record-time format (yaml vs gob),
	// orthogonal to Format (the yaml/json storage encoding). Empty means
	// "use the package-global default" (configuredMockFormat), preserving
	// single-session behaviour. Multi-app/per-session callers set this so
	// each session honours its own spec.MockFormat instead of the process
	// global — see useGobFormat.
	MockFormat string

	// Async gob writer: background goroutine drains gobQueue and
	// encodes to a persistent *os.File + bufio + gob.Encoder. Parser
	// goroutines never block on disk or gob encoding. Sync fallback
	// activates when the queue is full so no mock is dropped.
	//
	// The writer lifecycle is restartable across Close/InsertMock
	// cycles so that re-record flows (same Recorder / same mockDB,
	// multiple Start calls) flush+close between sessions but still
	// accept mocks for the next session. The lifecycle lock
	// (gobLifecycle) guards the transition between "running" and
	// "quiesced"; gobRunning tracks which state we are in. Do not start
	// the writer under a sync.Once — it cannot be reset, which was the
	// original re-record bug (gobLifecycleInit only makes the lock).
	//
	// The lock is a one-slot channel, not a mutex, so Close can stop
	// waiting for it: an insert that found the queue full holds it while
	// it waits for room, and on a stuck disk waits for as long as the disk
	// does.
	gobLifecycle     chan struct{}
	gobLifecycleInit sync.Once
	gobRunning       bool
	gobQueue         chan gobWriteJob
	gobStop          chan struct{}
	gobDone          chan struct{}
	gobMu            sync.Mutex
	gobFilePath      string
	gobFile          *os.File
	gobBufw          *bufio.Writer
	gobEnc           *gob.Encoder
	gobOverflows     atomic.Uint64
	// gobStopClosed is true after Close() has invoked close(gobStop).
	// A subsequent Close() that arrives after the first one timed out
	// waiting for gobDone must not close the channel a second time
	// (that would panic). Cleared when the writer finally exits and
	// we transition back to gobRunning=false.
	gobStopClosed bool
	// gobFlushErr holds the terminal flush/close error from
	// gobFlushAndClose so that Close() can surface it to its caller
	// (and therefore to the Recorder.Start deferred-cleanup logger)
	// instead of silently losing the tail of mocks.gob on a disk-full
	// or permission-change shutdown.
	gobFlushErr error
	// gobFileW is the file the writer writes to as its buffer hands bytes
	// on: it says which encoded mocks the file has taken whole (gobWritten).
	// Guarded by gobMu, as every write to the file is.
	gobFileW *gobFileWriter
	// gobQueued counts the session's mocks handed to the store (queued for
	// the writer), and gobWritten those the mock file took whole: Close
	// says how many of them were not written, read without the writer's
	// locks, which a stuck writer holds.
	gobQueued, gobWritten atomic.Uint64
	// gobPathSeen is gobFilePath, for a Close that cannot take gobMu.
	gobPathSeen atomic.Value
	// gobSets holds the sets (setFileKey) an insert found to write in gob,
	// so the gob hot path looks at a set's files once, not on every insert.
	// Forgotten at Close and when a set's files are deleted.
	gobSets sync.Map
}

// gobFileWriter is the gob file as the writer's buffer writes to it. Each
// write is handed on in pieces of at most progressChunk, and every mock whose
// encoded bytes the file has then taken whole is counted in written: a mock
// is written once its last byte is, so a write a slow disk takes long over
// counts the mocks it lands while it goes, and a write that never returns
// leaves the mocks it holds counted as not written. Used under gobMu only.
type gobFileWriter struct {
	w io.Writer
	// took is how many bytes the file took; ends, oldest first, the stream
	// offset each encoded mock not yet taken whole ends at.
	took    uint64
	ends    []uint64
	written *atomic.Uint64
}

const progressChunk = 32 << 10

// gobFileOf is what the writer writes a gob file through: the file itself.
// Tests stand a disk that is stuck, or fails, in its place.
var gobFileOf = func(f *os.File) io.Writer { return f }

// closeGobFile is how the writer closes a gob file it has finished: the
// file's own Close. Tests stand a file whose close fails (a write back the OS
// reports only then) in its place.
var closeGobFile = func(f *os.File) error { return f.Close() }

// closeMockFile is how appendMock closes the yaml or json mock file it
// appended to: the file's own Close. Tests stand a file whose close fails in
// its place.
var closeMockFile = func(f *os.File) error { return f.Close() }

func (f *gobFileWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > progressChunk {
			chunk = chunk[:progressChunk]
		}
		n, err := f.w.Write(chunk)
		written += n
		f.took += uint64(n)
		f.settle()
		if err != nil {
			return written, err
		}
		b = b[n:]
	}
	return written, nil
}

// encoded says a mock was encoded: its bytes end at the stream's offset end
// (the file took + what the buffer holds).
func (f *gobFileWriter) encoded(end uint64) {
	f.ends = append(f.ends, end)
	f.settle()
}

// settle counts the mocks the file has taken whole.
func (f *gobFileWriter) settle() {
	i := 0
	for i < len(f.ends) && f.ends[i] <= f.took {
		i++
	}
	if i > 0 {
		f.written.Add(uint64(i))
		f.ends = f.ends[i:]
	}
}

// prunedMockInfo is the structured per-mock entry logged when
// UpdateMocks drops a mock. Collected into a single slice and emitted
// once per prune call so operators can see exactly which mocks were
// dropped without flooding the log with one line per mock.
type prunedMockInfo struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Metadata map[string]string `json:"metadata"`
}

// maxPrunedMocksLogged caps how many per-mock entries get attached to
// the "pruned mocks successfully" debug log. Replays on large test
// sets can prune ~10^5 mocks; logging every entry would produce a
// single multi-MB log line that slows down encoding and overwhelms
// ingestion. Above the cap we set prunedMocksTruncated=true and rely
// on the pruned-count total for the overall picture.
const maxPrunedMocksLogged = 100

type gobWriteJob struct {
	mock *models.Mock
	// testSetPath is the full directory path — "<MockPath>/<testSetID>"
	// — not just the test-set identifier. Kept as a full path because
	// gobReopenLocked mkdir's it and reuses it in filepath.Join.
	testSetPath string
	filename    string
}

const mockFileLockStripeCount = 256

var mockFileLockStripes [mockFileLockStripeCount]sync.RWMutex

func New(Logger *zap.Logger, mockPath string, mockName string) *MockYaml {
	return NewWithFormat(Logger, mockPath, mockName, yaml.FormatYAML)
}

func NewWithFormat(Logger *zap.Logger, mockPath string, mockName string, format yaml.Format) *MockYaml {
	return &MockYaml{
		MockPath:  mockPath,
		MockName:  mockName,
		Logger:    Logger,
		idCounter: -1,
		Format:    format,
	}
}

func mockFileLockKey(path, fileName string, format yaml.Format) string {
	fullPath := filepath.Join(path, fileName+"."+format.FileExtension())
	if absPath, err := filepath.Abs(fullPath); err == nil {
		return absPath
	}
	return fullPath
}

func getMockFileLock(lockKey string) *sync.RWMutex {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(lockKey))
	return &mockFileLockStripes[hasher.Sum32()%mockFileLockStripeCount]
}

func resolveMockFileMode(targetPath string) (os.FileMode, error) {
	info, err := os.Stat(targetPath)
	if err == nil {
		return info.Mode().Perm(), nil
	}
	if os.IsNotExist(err) {
		return 0o777, nil
	}
	return 0, err
}

// The merge of what the learn step learned onto a recording is the learn
// package's, which the agent shares for a test set's retry cycles
// (learn.KeptLearning).
var (
	mergeLearned      = learn.MergeLearned
	mergeVariable     = learn.MergeVariable
	mergeReqBodyNoise = learn.MergeReqBodyNoise
)

// replaceFile puts a rewritten mock file in place (yaml.ReplaceFile); tests
// swap it to make the replace fail.
var replaceFile = yaml.ReplaceFile

// UpdateMocks prunes unused mocks from the mock file and keeps required ones.
//
// mockNames is a keep-set keyed by mock name (values carry models.MockState details).
// Mocks present in mockNames are retained; other mocks may still be retained by
// timestamp-based exemptions (for replay writes and startup/init traffic).
//
// startupCutoffTime is the startup-mock exemption boundary: any mock recorded
// before it is a "startup mock" (captured from app boot up to and including the
// first StartupMockTestCaseWindow test cases) and is kept even when no executed
// test consumes it. The caller (replay) computes it from the per-test-case
// request timestamps; a zero value disables the exemption.
func (ys *MockYaml) UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState, pruneBefore time.Time, startupCutoffTime time.Time) error {
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockFileName, ys.Format))
	lock.Lock()
	defer lock.Unlock()

	// gob is recorded as a single mocks.gob blob (orthogonal to the yaml/json
	// StorageFormat axis). If we find one on disk, the read/prune path stays
	// in gob land regardless of what ys.Format says; otherwise we fall through
	// to the format-detection logic below for yaml/json.
	gobPath := filepath.Join(path, mockFileName+".gob")
	if _, err := os.Stat(gobPath); err == nil {
		return ys.updateMocksGob(ctx, testSetID, gobPath, mockNames, pruneBefore, startupCutoffTime)
	}

	// Detect the format the mocks file is actually stored in (may differ
	// from ys.Format after a StorageFormat switch). If no mocks file exists
	// at all, nothing to prune.
	existsAny, detectedFormat, err := yaml.FileExistsAny(ctx, ys.Logger, path, mockFileName, ys.Format)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to stat mocks file", zap.String("path", path))
		return err
	}
	if !existsAny {
		return nil
	}

	ext := "." + detectedFormat.FileExtension()
	ys.Logger.Debug("pruning unused mocks",
		zap.Any("consumedMocks", mockNames),
		zap.String("testSetID", testSetID),
		zap.String("path", filepath.Join(path, mockFileName+ext)),
		zap.String("detectedFormat", string(detectedFormat)),
		zap.Time("pruneBefore", pruneBefore))

	// One mock at a time: read, decode, judge, and write the ones that stay
	// to the replacement before reading the next (see rewrite.go). Written
	// back in the format read, so a prune never migrates the file's format.
	// A second pass keeps what a kept mock requires, and the async epochs a
	// replay serves (see prune_deps.go).
	verdict := &pruneVerdict{keeps: func(m *models.Mock) bool {
		return pruneKeeps(m, mockNames, pruneBefore, startupCutoffTime)
	}}
	st, err := ys.prunePass(ctx, path, mockFileName, detectedFormat, verdict)
	if err == nil && st.again {
		st, err = ys.prunePass(ctx, path, mockFileName, detectedFormat, verdict)
	}
	if err != nil {
		return err
	}

	ys.Logger.Debug("pruned mocks successfully",
		zap.String("testSetID", testSetID),
		zap.Int("total", st.total),
		zap.Int("kept", st.kept),
		zap.Int("pruned", st.pruned),
		zap.Int("copiedVerbatim", st.copied),
		zap.Int("keptAsDependency", verdict.asDependency),
		zap.Strings("keptAsDependencyMocks", verdict.asDependencyNames),
		zap.Int("keptAsAsyncEpoch", verdict.asTimeline),
		zap.Strings("keptAsAsyncEpochMocks", verdict.asTimelineNames),
		zap.Any("prunedMocks", st.prunedMocks),
		zap.Bool("prunedMocksTruncated", st.pruned > len(st.prunedMocks)),
		zap.Time("pruneBefore", pruneBefore))

	return nil
}

// pruneStats are what one pass of the prune did, for its log.
type pruneStats struct {
	total, kept, pruned, copied int
	prunedMocks                 []prunedMockInfo
	// again says the pass was discarded: it dropped a mock that a mock it
	// kept requires, and the verdict is set up for the pass that keeps it.
	again bool
}

// recordPruned counts a mock the pass drops, and names it in the log's
// list while that has room. Only when debug logging is enabled: on large
// test sets the allocation and reflection cost of collecting
// names/kinds/metadata is a significant overhead if the emitted log will be
// dropped by the logger anyway.
func (st *pruneStats) recordPruned(mock *models.Mock, debugEnabled bool) {
	st.pruned++
	if debugEnabled && len(st.prunedMocks) < maxPrunedMocksLogged {
		if st.prunedMocks == nil {
			st.prunedMocks = make([]prunedMockInfo, 0, maxPrunedMocksLogged)
		}
		st.prunedMocks = append(st.prunedMocks, prunedMockInfo{
			Name:     mock.Name,
			Kind:     string(mock.Kind),
			Metadata: mock.Spec.Metadata,
		})
	}
}

// prunePass reads the yaml/json mock file once, judging each mock with
// verdict, and puts the mocks it keeps in place of the file; or, in a first
// pass that dropped a mock a kept one requires, discards what it wrote and
// says so (pruneStats.again).
func (ys *MockYaml) prunePass(ctx context.Context, path, mockFileName string, format yaml.Format, verdict *pruneVerdict) (pruneStats, error) {
	var st pruneStats
	ext := "." + format.FileExtension()
	reader, err := yaml.NewMockReaderF(ctx, ys.Logger, path, mockFileName, format)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read the mocks from file", zap.String("at_path", filepath.Join(path, mockFileName+ext)))
		return st, err
	}
	defer reader.Close()

	rw := newMockFileRewriter(ys.Logger, path, mockFileName, format)
	defer rw.discard()

	debugEnabled := ys.Logger.Core().Enabled(zap.DebugLevel)
	fileLogger := ys.Logger.With(zap.String("mock_file", reader.Path()))
	for {
		doc, readErr, decodeErr := nextDoc(reader, fileLogger, true)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			utils.LogError(ys.Logger, readErr, "failed to decode the file documents", zap.String("at_path", filepath.Join(path, mockFileName+ext)))
			return st, fmt.Errorf("failed to decode the file documents. error: %v", readErr.Error())
		}
		if decodeErr != nil {
			return st, fmt.Errorf("failed to decode the mocks in %s: %w", reader.Path(), decodeErr)
		}
		if doc.verbatim {
			// A document this keploy cannot decode is not one it can judge,
			// and a connection failure is not one it changes: copied through
			// as it is (see fileDoc.verbatim).
			if err := rw.copyDoc(doc); err != nil {
				return st, err
			}
			if !doc.incomplete {
				st.copied++
			}
			continue
		}
		for _, mock := range doc.mocks {
			st.total++
			keep, err := verdict.judge(mock)
			if err != nil {
				return st, err
			}
			if keep {
				if err := rw.write(mock); err != nil {
					return st, err
				}
				st.kept++
				continue
			}
			st.recordPruned(mock, debugEnabled)
		}
	}

	// Done reading: close before the replace, which some platforms refuse
	// over a file that is still open.
	_ = reader.Close()
	if verdict.secondPass() {
		st.again = true
		return st, nil
	}
	if err := verdict.checkPlaces(); err != nil {
		return st, err
	}
	if verdict.track && st.pruned == 0 {
		return st, nil
	}
	return st, rw.commit()
}

// updateMocksGob implements RemoveUnusedMocks for mocks.gob. The
// filter decision matches the YAML path exactly (keep config mocks,
// mocks named in mockNames, post-replay mocks, and startup mocks
// recorded before startupCutoffTime — prune everything else). The
// rewrite rules are different because gob doesn't support append: the
// kept mocks are streamed into a fresh single-encoder stream with the magic
// header, which replaces the file once complete. An existing gob writer
// on this MockYaml must be quiesced before we touch the file so a
// concurrent InsertMock doesn't race the truncate-and-rewrite.
func (ys *MockYaml) updateMocksGob(ctx context.Context, testSetID, gobPath string, mockNames map[string]models.MockState, pruneBefore, startupCutoffTime time.Time) error {
	ys.Logger.Debug("pruning unused mocks (gob)",
		zap.Any("consumedMocks", mockNames),
		zap.String("testSetID", testSetID),
		zap.String("path", gobPath),
		zap.Time("pruneBefore", pruneBefore))

	// Quiesce any in-flight async writer on this MockYaml before we
	// rewrite the gob file. An active writer holds ys.gobFile /
	// ys.gobBufw / ys.gobEnc; rewriting the file out from under it
	// would corrupt the next Encode. Close drains the queue and
	// resets lifecycle state; the next InsertMock restarts a fresh
	// writer via the inline init in insertMockGob. It waits for the writer
	// until the caller's ctx ends: a disk that is stuck is the caller's to
	// give up on (CloseUnless).
	if err := ys.CloseUnless(ctx.Done()); err != nil {
		utils.LogError(ys.Logger, err, "failed to quiesce async gob writer before pruning; check disk space and writer state", zap.String("path", gobPath))
		return err
	}

	// Bail early if the caller has already cancelled before we touch
	// the tmp file. Big test-sets have ~10^5 mocks and the filter+
	// encode loop below can run for seconds; a cancelled recorder
	// should not sit here rewriting a file whose result nobody is
	// waiting for.
	if err := ctx.Err(); err != nil {
		return err
	}

	// A second pass keeps what a kept mock requires, and the async epochs a
	// replay serves (see prune_deps.go).
	verdict := &pruneVerdict{keeps: func(m *models.Mock) bool {
		return pruneKeeps(m, mockNames, pruneBefore, startupCutoffTime)
	}}
	st, err := ys.prunePassGob(ctx, gobPath, verdict)
	if err == nil && st.again {
		st, err = ys.prunePassGob(ctx, gobPath, verdict)
	}
	if err != nil {
		if ctx.Err() == nil {
			utils.LogError(ys.Logger, err, "failed to prune gob mocks", zap.String("path", gobPath))
		}
		return err
	}

	ys.Logger.Debug("pruned mocks successfully (gob)",
		zap.String("testSetID", testSetID),
		zap.Int("total", st.total),
		zap.Int("kept", st.kept),
		zap.Int("pruned", st.pruned),
		zap.Int("keptAsDependency", verdict.asDependency),
		zap.Strings("keptAsDependencyMocks", verdict.asDependencyNames),
		zap.Int("keptAsAsyncEpoch", verdict.asTimeline),
		zap.Strings("keptAsAsyncEpochMocks", verdict.asTimelineNames),
		zap.Any("prunedMocks", st.prunedMocks),
		zap.Bool("prunedMocksTruncated", st.pruned > len(st.prunedMocks)),
		zap.Time("pruneBefore", pruneBefore))
	return nil
}

// prunePassGob is prunePass for mocks.gob: the kept mocks are streamed into a
// fresh single-encoder stream, which replaces the file once complete, unless
// the pass is a first one to do again (pruneStats.again).
func (ys *MockYaml) prunePassGob(ctx context.Context, gobPath string, verdict *pruneVerdict) (pruneStats, error) {
	var st pruneStats
	rw, err := newGobFileRewriter(gobPath)
	if err != nil {
		return st, err
	}
	defer rw.discard()

	// See prunePass for why per-mock collection is gated on debug level —
	// the gob path is the one most likely to hit ~10^5-mock test sets, so
	// skipping the allocation when the log is a no-op matters more here.
	debugEnabled := ys.Logger.Core().Enabled(zap.DebugLevel)
	err = forEachGobMock(gobPath, func(mock *models.Mock) error {
		// The gob stream has no per-line reader watching ctx, so a very
		// large set checks it every ctxCheckEvery mocks.
		if err := ctxErrEvery(ctx, st.total); err != nil {
			return err
		}
		st.total++
		keep, err := verdict.judge(mock)
		if err != nil {
			return err
		}
		if keep {
			st.kept++
			return rw.write(mock)
		}
		st.recordPruned(mock, debugEnabled)
		return nil
	})
	if err != nil {
		return st, err
	}
	if verdict.secondPass() {
		st.again = true
		return st, nil
	}
	if err := verdict.checkPlaces(); err != nil {
		return st, err
	}
	if verdict.track && st.pruned == 0 {
		return st, nil
	}
	return st, rw.commit()
}

// DeleteMocks takes the named mocks out of the set's mock file, in the
// format it is stored in (yaml, json or gob), and keeps every other mock as
// it is. As UpdateMocks does, it keeps a named mock that a kept mock requires
// or that a replay serves as an async epoch (prune_deps.go): taking it out
// would break a recording that stays. It returns, sorted, the named mocks it
// took out. A set with no mock file, or one it takes none of names out of,
// is not written, and nothing is returned.
func (ys *MockYaml) DeleteMocks(ctx context.Context, testSetID string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	named := make(map[string]bool, len(names))
	for _, n := range names {
		named[n] = true
	}
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockFileName, ys.Format))
	lock.Lock()
	defer lock.Unlock()

	verdict := &pruneVerdict{keeps: func(m *models.Mock) bool { return !named[m.Name] }, track: true}
	gobPath := filepath.Join(path, mockFileName+".gob")
	var err error
	if _, statErr := os.Stat(gobPath); statErr == nil {
		// As updateMocksGob: an async gob writer of this MockYaml is
		// quiesced before its file is rewritten.
		if err := ys.Close(); err != nil {
			return nil, err
		}
		var st pruneStats
		st, err = ys.prunePassGob(ctx, gobPath, verdict)
		if err == nil && st.again {
			_, err = ys.prunePassGob(ctx, gobPath, verdict)
		}
	} else {
		existsAny, format, statErr := yaml.FileExistsAny(ctx, ys.Logger, path, mockFileName, ys.Format)
		if statErr != nil {
			return nil, statErr
		}
		if !existsAny {
			return nil, nil
		}
		var st pruneStats
		st, err = ys.prunePass(ctx, path, mockFileName, format, verdict)
		if err == nil && st.again {
			_, err = ys.prunePass(ctx, path, mockFileName, format, verdict)
		}
	}
	if err != nil {
		return nil, err
	}
	out := slices.Clone(verdict.dropped)
	sort.Strings(out)
	return out, nil
}

// PersistMockNoise merges learned request-body noise (MockState.ReqBodyNoise,
// detected under --schema-noise-detection) into the on-disk mocks WITHOUT
// pruning anything. This is the persistence path when mock pruning
// (--remove-unused-mocks) is not enabled — previously the learned noise rode
// only inside UpdateMocks, so running detection without pruning silently
// discarded everything that was learned at process exit.
func (ys *MockYaml) PersistMockNoise(ctx context.Context, testSetID string, mockStates map[string]models.MockState) error {
	_, err := ys.persistLearned(ctx, testSetID, mockStates)
	return err
}

// PersistLearned writes what the learn step of record's full round learned
// (MockState.ReqBodyNoise, Variables and Binds; package matcher/learn) onto
// the set's mocks, without pruning anything, as PersistMockNoise does, and
// returns by mock name what it added (mergeLearned), sorted. What a mock
// already carried is not added again.
func (ys *MockYaml) PersistLearned(ctx context.Context, testSetID string, mockStates map[string]models.MockState) (map[string][]string, error) {
	return ys.persistLearned(ctx, testSetID, mockStates)
}

// mergedOnto merges st onto mock (mergeLearned) and returns what it added
// and whether it changed the mock. Under the proven switch a merge can change
// a mock and add nothing: an answer place on a variable the mock has, an echo
// or a clock read that no longer holds, noise at a variable's place dropped;
// the merge changes only the mock's variables and its request noise, which
// are compared. Without the switch what it adds decides, as before (noise
// only merges, and only a new place changes it).
func mergedOnto(mock *models.Mock, st models.MockState, binds models.MockVariables) ([]string, bool) {
	if !provenswitch.On() {
		a := mergeLearned(mock, st, binds)
		return a, len(a) > 0
	}
	vars, noise := mock.Spec.Variables.Clone(), learn.MergeReqBodyNoise(nil, mock.Spec.ReqBodyNoise)
	a := mergeLearned(mock, st, binds)
	return a, len(a) > 0 || !reflect.DeepEqual(vars, mock.Spec.Variables.Clone()) ||
		!reflect.DeepEqual(noise, learn.MergeReqBodyNoise(nil, mock.Spec.ReqBodyNoise))
}

func (ys *MockYaml) persistLearned(ctx context.Context, testSetID string, mockStates map[string]models.MockState) (map[string][]string, error) {
	// Only mocks that carry learned noise or variables matter, and the
	// earlier mocks other mocks' variables are bound at (Binds).
	withNoise, binds := learn.FoldLearned(mockStates)
	if len(withNoise) == 0 {
		return nil, nil
	}
	added := map[string][]string{}

	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockFileName, ys.Format))
	lock.Lock()
	defer lock.Unlock()

	// merge applies the learned noise to one mock; returns true when it
	// changed the mock, so a file nothing changed in is never rewritten.
	// Every parser (HTTP included) stores noise uniformly on the
	// kind-agnostic MockSpec.ReqBodyNoise. Previously this path skipped every
	// non-HTTP mock, so learning under --schema-noise-detection WITHOUT
	// --remove-unused-mocks silently discarded the learned noise at exit.
	merge := func(mock *models.Mock) bool {
		st, ok := withNoise[mock.Name]
		if !ok {
			return false
		}
		_, changed := mergedOnto(mock, st, binds[mock.Name])
		return changed
	}
	// record is merge for the pass that writes: it keeps what it added.
	record := func(mock *models.Mock) bool {
		st, ok := withNoise[mock.Name]
		if !ok {
			return false
		}
		a, changed := mergedOnto(mock, st, binds[mock.Name])
		if len(a) > 0 {
			added[mock.Name] = a
		}
		return changed
	}

	logPersisted := func(total int) {
		ys.Logger.Info("persisted learned request-body noise onto mocks",
			zap.String("testSetID", testSetID),
			zap.Int("mocksWithLearnedNoise", len(withNoise)),
			zap.Int("totalMocks", total))
	}

	// Like the prune, the write-back streams the file one mock at a time
	// (see rewrite.go). A first, decode-only pass stops at the first mock
	// the learned noise would change, and only then is the file rewritten:
	// noise already on disk is re-learned on every later run, which so costs
	// a read, never a rewrite.
	gobPath := filepath.Join(path, mockFileName+".gob")
	if _, err := os.Stat(gobPath); err == nil {
		// Quiesce any in-flight async gob writer before the rewrite —
		// same reasoning, and the same end at ctx, as updateMocksGob.
		if err := ys.CloseUnless(ctx.Done()); err != nil {
			utils.LogError(ys.Logger, err, "failed to quiesce async gob writer before noise persistence", zap.String("path", gobPath))
			return nil, err
		}
		scanned, changes := 0, false
		err := forEachGobMock(gobPath, func(mock *models.Mock) error {
			if err := ctxErrEvery(ctx, scanned); err != nil {
				return err
			}
			scanned++
			if merge(mock) {
				changes = true
				return errStopScan
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStopScan) {
			return nil, err
		}
		if !changes {
			return nil, nil
		}
		rw, err := newGobFileRewriter(gobPath)
		if err != nil {
			return nil, err
		}
		defer rw.discard()
		total := 0
		err = forEachGobMock(gobPath, func(mock *models.Mock) error {
			if err := ctxErrEvery(ctx, total); err != nil {
				return err
			}
			total++
			record(mock)
			return rw.write(mock)
		})
		if err != nil {
			return nil, err
		}
		if err := rw.commit(); err != nil {
			return nil, err
		}
		logPersisted(total)
		return added, nil
	}

	existsAny, detectedFormat, err := yaml.FileExistsAny(ctx, ys.Logger, path, mockFileName, ys.Format)
	if err != nil {
		return nil, err
	}
	if !existsAny {
		return nil, nil
	}

	// eachDoc streams the file's documents to fn, closing the file before
	// it returns.
	eachDoc := func(fn func(doc fileDoc) error) error {
		reader, err := yaml.NewMockReaderF(ctx, ys.Logger, path, mockFileName, detectedFormat)
		if err != nil {
			return err
		}
		defer reader.Close()
		fileLogger := ys.Logger.With(zap.String("mock_file", reader.Path()))
		for {
			doc, readErr, decodeErr := nextDoc(reader, fileLogger, true)
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return fmt.Errorf("failed to decode the file documents for noise persistence: %w", readErr)
			}
			if decodeErr != nil {
				return fmt.Errorf("failed to decode the mocks in %s for noise persistence: %w", reader.Path(), decodeErr)
			}
			if err := fn(doc); err != nil {
				return err
			}
		}
	}

	changes := false
	err = eachDoc(func(doc fileDoc) error {
		if doc.verbatim {
			return nil
		}
		for _, mock := range doc.mocks {
			if merge(mock) {
				changes = true
				return errStopScan
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopScan) {
		return nil, err
	}
	if !changes {
		return nil, nil
	}

	rw := newMockFileRewriter(ys.Logger, path, mockFileName, detectedFormat)
	defer rw.discard()
	total := 0
	if err := eachDoc(func(doc fileDoc) error {
		if doc.verbatim {
			// Copied as read (see fileDoc.verbatim).
			return rw.copyDoc(doc)
		}
		changed := false
		for _, mock := range doc.mocks {
			total++
			changed = record(mock) || changed
		}
		if !changed && provenswitch.On() {
			// Under the proven switch a document the learn step changes
			// nothing of is copied as the file holds it, so a hand edit
			// inside it (a note, a changed value, its own layout) stays as
			// the hand made it: keploy mock record keeps the hand edits of
			// what it does not record anew, and the learn step of its full
			// round must not undo them. (The comment lines a file starts
			// with are its header, which every write of the file writes
			// anew.) With the switch off every mock is written anew, as
			// before.
			return rw.copyDoc(doc)
		}
		for _, mock := range doc.mocks {
			if err := rw.write(mock); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := rw.commit(); err != nil {
		return nil, err
	}
	logPersisted(total)
	return added, nil
}

func (ys *MockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error {
	mock.Name = fmt.Sprint("mock-", ys.getNextID())
	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}
	// An insert writes to the mock file the set already has, in that file's
	// format; only a set with none takes the configured format (insertFormat).
	// gob is the binary record-time format (async writer, ~28% CPU win over
	// yaml).
	useGob, effFormat, err := ys.insertFormat(ctx, mockPath, mockFileName)
	if err != nil {
		return err
	}
	if useGob {
		// gob writes the struct as it is, with no per-kind encoder to refuse
		// a connection failure the readers would skip (see
		// connFailureSchemaOf), so refuse it here.
		//
		// The keploy that records the kind must never write it to gob at
		// all: a gob reader up to v3.6.107 has no kind check and reads it as
		// a mock with no metadata.destAddr, which sets recorded.unknown and
		// turns loopback refusal off for the whole test set; and gob cannot
		// refuse a field it does not know, so every keploy's gob prune would
		// drop what a later format adds (see the ConnectionFailure kind).
		if mock.Kind == models.ConnectionFailure {
			if err := mock.ValidateConnFailure(); err != nil {
				utils.LogError(ys.Logger, err, "refusing to write an invalid connection failure mock", zap.String("mock_name", mock.Name))
				return fmt.Errorf("%w (gob): %w", models.ErrMockEncode, err)
			}
		}
		return ys.insertMockGob(ctx, mock, mockPath, mockFileName)
	}

	buf := getDocBuffer()
	defer putDocBuffer(buf)
	if err := ys.appendMock(ctx, mock, mockPath, mockFileName, effFormat, buf); err != nil {
		return err
	}
	// The mock's document is written: hand it to the recorder's receiver,
	// which gives it to the AfterMockInsert hooks (record.MockContext.Encoded)
	// so a hook that keeps a copy of this file does not encode the mock again.
	// It is the document only: not the version comment or the "---" before it.
	// The file is unlocked by now, so the receiver holds up no writer of a file
	// whose lock stripe this one shares, and may call back into the MockDB.
	// The format is spelled as the file's extension: what was written, even
	// for a MockYaml whose Format was left empty (written as YAML).
	models.HandMockDoc(ctx, mock, buf.Bytes(), effFormat.FileExtension())
	return nil
}

// ErrMixedMockFiles is an insert into a set that holds both a mocks.gob and a
// YAML or NDJSON mock file. The readers read the mocks.gob alone, so a mock
// added to either file would be lost or would hide the other file's mocks:
// the insert is refused, never written to one of them.
var ErrMixedMockFiles = errors.New("the test set holds a gob mock file and a text mock file")

// setFileKey names a set's mock files without their extension.
func setFileKey(mockPath, mockFileName string) string {
	return filepath.Join(mockPath, mockFileName)
}

// insertFormat is where an insert into the set at mockPath writes: to the mock
// file the set already has, in that file's format (useGob for a mocks.gob,
// else the text format of its mocks.yaml or mocks.json), and only for a set
// with none, in the configured format. An insert used to take the configured
// format alone, while the readers read a mocks.gob whenever there is one: a
// mock added under a yaml config to a set recorded in gob went to a mocks.yaml
// the readers never read, and one added under a gob config to a set recorded
// in yaml began a mocks.gob that hid the set's mocks.yaml.
//
// A set found to write in gob is remembered (gobSets), so the gob hot path
// looks at the set's files once. A text set is looked at on every insert, as
// the yaml writer always did, which opens its file on every insert anyway.
func (ys *MockYaml) insertFormat(ctx context.Context, mockPath, mockFileName string) (useGob bool, text yaml.Format, err error) {
	key := setFileKey(mockPath, mockFileName)
	if _, ok := ys.gobSets.Load(key); ok {
		return true, "", nil
	}
	gobPath := key + ".gob"
	hasGob := false
	if _, serr := os.Stat(gobPath); serr == nil {
		hasGob = true
	} else if !os.IsNotExist(serr) {
		return false, "", fmt.Errorf("check for the set's gob mock file %s before adding a mock to the set: %w", gobPath, serr)
	}
	hasText, detected, terr := yaml.FileExistsAny(ctx, ys.Logger, mockPath, mockFileName, ys.Format)
	if terr != nil {
		return false, "", fmt.Errorf("check for the set's mock file in %s before adding a mock to it: %w", mockPath, terr)
	}
	switch {
	case hasGob && hasText:
		textPath := key + "." + detected.FileExtension()
		return false, "", fmt.Errorf("%w: %s and %s; keploy reads only %s, so the mock was not added to either: keep the one that holds the set (move the other out of the set) and run again: %w",
			ErrMixedMockFiles, gobPath, textPath, filepath.Base(gobPath), models.ErrMocksNotWritten)
	case hasText:
		return false, detected, nil
	case hasGob || ys.useGobFormat():
		ys.gobSets.Store(key, struct{}{})
		return true, "", nil
	}
	return false, ys.Format, nil
}

// textMockFileBeside is the set's YAML or NDJSON mock file when it has one
// beside its mocks.gob, which the readers then do not read; empty otherwise.
func (ys *MockYaml) textMockFileBeside(ctx context.Context, path, mockFileName string) string {
	exists, detected, err := yaml.FileExistsAny(ctx, zap.NewNop(), path, mockFileName, ys.Format)
	if err != nil || !exists {
		return ""
	}
	return filepath.Join(path, mockFileName+"."+detected.FileExtension())
}

// appendMock appends mock's document to the mocks file in effFormat, under the
// file's lock. The document is made in buf, and buf is what is written.
func (ys *MockYaml) appendMock(ctx context.Context, mock *models.Mock, mockPath, mockFileName string, effFormat yaml.Format, buf *bytes.Buffer) (err error) {
	lock := getMockFileLock(mockFileLockKey(mockPath, mockFileName, effFormat))
	lock.Lock()
	defer lock.Unlock()

	// Bail before opening the file if the recorder ctx is already cancelled.
	// Nothing on disk is touched yet — this is the only safe early-exit
	// point. Past this point we MUST flush the bufio writer before returning:
	// a document larger than its buffer reaches the file in pieces as it is
	// written, so skipping writer.Flush() drops the tail of the mock.
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// CreateFileF picks the right extension for `effFormat` so the json pass
	// writes .json and the yaml pass writes .yaml.
	isFileEmpty, err := yaml.CreateFileF(ctx, ys.Logger, mockPath, mockFileName, effFormat)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to create file", zap.String("path directory", mockPath), zap.String("file", mockFileName))
		return err
	}

	filePath := filepath.Join(mockPath, mockFileName+"."+effFormat.FileExtension())
	file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to open mock file for append: %w", err)
	}
	// The file's close error is the mock's: a write the file system refuses
	// only as the file closes (NFS, a quota, some FUSE mounts) may have left
	// the mock out, and an insert that returned nil would have the recording
	// look complete. It runs after the deferred flush below. An error already
	// returned is kept.
	defer func() {
		if cerr := closeMockFile(file); cerr != nil && err == nil {
			err = fmt.Errorf("the mock file failed as it was closed: %w", cerr)
		}
	}()

	writer := bufio.NewWriter(file)
	// Belt-and-braces: always flush the bufio writer before file.Close,
	// even on an error return below. file.Close() does NOT drain a
	// wrapping bufio.Writer. The mock's document is made in memory and
	// written whole, so what an error return can leave in the buffer is the
	// file's version comment, which must reach the file (see below), or part
	// of a document whose write failed, of which more may already be in the
	// file. The deferred Flush is best-effort: errors are logged at debug
	// because the happy-path Flush below surfaces real flush errors as the
	// function return value.
	defer func() {
		if flushErr := writer.Flush(); flushErr != nil && ys.Logger != nil {
			ys.Logger.Debug("deferred bufio flush returned error",
				zap.String("path", filePath),
				zap.Error(flushErr))
		}
	}()

	// Encode, then write. Each branch takes a different in-memory
	// representation: JSON builds NetworkTrafficDocJSON directly (no yaml.Node
	// anywhere), YAML writes the document EncodeMock's yaml.Node would (below).
	// Either way the mock's document is made in buf first, and buf is what is
	// written, so it is also what InsertMock hands the recorder's receiver.
	switch effFormat {
	case yaml.FormatJSON:
		jsonDoc, handled, err := EncodeMockJSON(mock, ys.Logger)
		if err != nil {
			// No errMapperEncode arm here on purpose: EncodeMockJSON projects
			// the OSS kinds itself and never invokes a registered mapper, so
			// every error it returns is json.Marshal on keploy's own spec —
			// i.e. genuinely a payload fault. The mapper case arrives as
			// handled=false and is caught below.
			return fmt.Errorf("%w (json): %w", models.ErrMockEncode, err)
		}
		if !handled {
			// EncodeMockJSON projects only the OSS kinds; it never consults the
			// mapper registry. A kind an enterprise mapper owns therefore lands
			// here looking exactly like "keploy cannot encode this" — and
			// tagging it ErrMockEncode would make the recorder skip EVERY mock
			// of that kind and revoke every test that touched it, finishing
			// green with an empty test set. That is the outcome errMapperEncode
			// exists to prevent, so stay fatal when a mapper is registered.
			if hasMapperForKind(mock.Kind) {
				return fmt.Errorf("%w: mockdb: kind %q has a registered MockYAMLMapper but the json format cannot encode it; record with storageFormat yaml", errMapperEncode, mock.Kind)
			}
			return fmt.Errorf("%w (json): unsupported mock kind %q", models.ErrMockEncode, mock.Kind)
		}
		if err := json.NewEncoder(buf).Encode(jsonDoc); err != nil {
			return fmt.Errorf("failed to encode mock json: %w", err)
		}
		// json.Encoder appends the trailing '\n' — NDJSON-ready.
		if _, err := writer.Write(buf.Bytes()); err != nil {
			return fmt.Errorf("failed to write mock json: %w", err)
		}
	default:
		// YAML path.
		// The version header is keyed off isFileEmpty, which CreateFileF only
		// reports true for the call that CREATED the file. So it has exactly one
		// chance: skip it here and the whole test set loses its provenance
		// comment, because every later InsertMock sees a file that exists.
		// Write it before the encode.
		if isFileEmpty {
			if version := utils.GetVersionAsComment(); version != "" {
				if _, err := writer.WriteString(version); err != nil {
					return fmt.Errorf("failed to write version comment: %w", err)
				}
			}
		}
		// The SEPARATOR, by contrast, must wait for a successful encode. A
		// skippable failure returns below and the deferred flush commits
		// whatever is already buffered, so writing "---" first injected a stray
		// empty YAML document into mocks.yaml for every dropped mock.
		//
		// A kind whose spec can be marshaled in place is written in one pass
		// (encode_inplace.go); the others through EncodeMock's yaml.Node.
		// Either way the document is made in memory first: marshaling in
		// place can fail partway (a value YAML cannot marshal), and what the
		// emitter had streamed by then would be half a document in the file.
		if v, inPlace := encodeMockInPlace(mock); inPlace {
			// A mock that holds a string a block scalar cannot carry is
			// written with it double-quoted (yaml.EncodeQuoted): as the
			// literal block yaml.v3 picks, it would not read back, and
			// the file would stop loading from it on.
			if yaml.NeedsQuoting(v) {
				q, err := yaml.EncodeQuoted(v)
				if err != nil {
					return fmt.Errorf("%w (yaml): %w", models.ErrMockEncode, err)
				}
				v = q
			}
			if err := encodeYAMLDoc(buf, v); err != nil {
				// A payload fault, as EncodeMock's Node encode of the same
				// value is: skippable.
				return fmt.Errorf("%w (yaml): %w", models.ErrMockEncode, err)
			}
		} else {
			doc, err := EncodeMock(mock, ys.Logger)
			if err != nil {
				// Only keploy's own encoders are pure enough to be skippable. A
				// registered mapper can fail for environmental reasons, so those
				// stay fatal — see errMapperEncode.
				if errors.Is(err, errMapperEncode) {
					return fmt.Errorf("failed to encode mock (yaml): %w", err)
				}
				return fmt.Errorf("%w (yaml): %w", models.ErrMockEncode, err)
			}
			if err := encodeYAMLDoc(buf, &doc); err != nil {
				return fmt.Errorf("failed to encode mock yaml: %w", err)
			}
		}
		if !isFileEmpty {
			if _, err := writer.WriteString("---\n"); err != nil {
				return fmt.Errorf("failed to write document separator: %w", err)
			}
		}
		if _, err := writer.Write(buf.Bytes()); err != nil {
			return fmt.Errorf("failed to write mock yaml: %w", err)
		}
	}

	// Always flush — never gate on ctx here. The document is in the bufio
	// buffer, and part of it already in the file when it was larger than the
	// buffer. Skipping Flush would leave the file truncated mid-mock, which is
	// the recorder-shutdown-flush truncation bug: the last mock in flight when
	// SIGINT cancels the recorder ctx lost its trailing bytes (rows 2/3 of a
	// multi-row MySQL binary result set, missing rowNullBuffer, no
	// FinalResponse marker), tripping wire-encode validation at replay time.
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush mock writer: %w", err)
	}
	return nil
}

// insertMockGob enqueues the mock for async encoding. One background
// goroutine owns the open file + encoder; parsers never block on disk
// while the queue has room. An insert that finds it full waits for room,
// in its turn, so mocks are never dropped or reordered; gobOverflows
// counts those inserts for observability.
//
// The writer-alive check and the channel send run under the same
// lifecycle lock Close uses. This makes the "is the writer still
// accepting jobs?" invariant atomic across the send: Close cannot
// transition from running=true to stopClosed in between our check
// and our send, so any job we enqueue is guaranteed to be drained
// by the current writer (or by its drainAndClose on the way out).
func (ys *MockYaml) insertMockGob(ctx context.Context, mock *models.Mock, mockPath, mockFileName string) error {
	ys.lifecycle() <- struct{}{}
	if ys.gobStopClosed {
		ys.unlockLifecycle()
		return fmt.Errorf("gob mock writer is closing; the recording session must complete its shutdown before new mocks can be accepted")
	}
	if !ys.gobRunning {
		ys.gobQueue = make(chan gobWriteJob, 4096)
		ys.gobStop = make(chan struct{})
		ys.gobDone = make(chan struct{})
		ys.gobRunning = true
		go ys.gobWriterLoop()
	}
	// Deep-copy before enqueue. InsertMock returns synchronously; a
	// caller that subsequently mutates the same *Mock (e.g. a
	// RecordHooks.AfterMockInsert that tags telemetry fields on the
	// same pointer, or a producer pool that reuses Mock structs)
	// would otherwise race with the async gob encoder and persist
	// an unintended payload. DeepCopy clones Mock's top-level
	// MockSpec slices, maps, and pointers so the usual
	// after-InsertMock field tagging is safe; it does not
	// transitively clone every nested object reachable through a
	// protocol payload, so callers should still avoid mutating
	// deeply nested state in a mock they have handed off. The copy
	// cost is bounded by the mock's own size and is acceptable vs.
	// the alternative (encoding to bytes synchronously on every
	// InsertMock, which would defeat the whole async-writer win).
	// Strip the runtime-only lifetime fields before they reach the encoder —
	// gob ignores the json:"-" tags that keep them out of every other format.
	// The reader clears them too (so existing files are repaired), but not
	// writing them keeps the on-disk shape honest about what it means.
	gobMock := mock.DeepCopy()
	var zeroLifetime models.Lifetime
	gobMock.TestModeInfo.Lifetime = zeroLifetime
	gobMock.TestModeInfo.LifetimeDerived = false
	job := gobWriteJob{mock: gobMock, testSetPath: mockPath, filename: mockFileName}
	// Counted before the send, so the writer never counts it as written
	// before it is counted as handed over; taken back if it is not sent.
	ys.gobQueued.Add(1)
	select {
	case ys.gobQueue <- job:
		ys.unlockLifecycle()
		return nil
	case <-ctx.Done():
		ys.gobQueued.Add(^uint64(0))
		ys.unlockLifecycle()
		return ctx.Err()
	default:
	}
	// The queue is full: the disk is slower than the capture. The insert
	// waits for room, in its turn, as the writer writes. It used to write
	// its mock itself, ahead of the thousands queued before it: out of order
	// in its file, and, for another test set's, the writer then reopened
	// (truncated) the first set's file under the mocks it had written there.
	// The lifecycle lock stays held while it waits, so Close cannot stop the
	// writer under it; a Close told to stop (CloseUnless) does not wait
	// behind it.
	ys.gobOverflows.Add(1)
	select {
	case ys.gobQueue <- job:
		ys.unlockLifecycle()
		return nil
	case <-ctx.Done():
		ys.gobQueued.Add(^uint64(0))
		ys.unlockLifecycle()
		return ctx.Err()
	}
}

// lifecycle is the gob writer's lifecycle lock (gobLifecycle): send to take
// it, unlockLifecycle to give it back.
func (ys *MockYaml) lifecycle() chan struct{} {
	ys.gobLifecycleInit.Do(func() { ys.gobLifecycle = make(chan struct{}, 1) })
	return ys.gobLifecycle
}

func (ys *MockYaml) unlockLifecycle() { <-ys.lifecycle() }

func (ys *MockYaml) gobWriterLoop() {
	defer close(ys.gobDone)
	for {
		select {
		case job, ok := <-ys.gobQueue:
			if !ok {
				ys.gobFlushAndClose()
				return
			}
			if err := ys.gobWriteOne(job); err != nil {
				// Accumulate into gobFlushErr so Close() surfaces any
				// steady-state encode failures to the recorder's
				// deferred cleanup log — previously these errors were
				// only logged and Close() could still return nil even
				// when one or more mocks had been dropped mid-session.
				ys.gobMu.Lock()
				ys.gobFlushErr = errors.Join(ys.gobFlushErr, err)
				ys.gobMu.Unlock()
				utils.LogError(ys.Logger, err, "async gob mock writer failed for one mock — continuing with the rest; check disk space on the mocks output directory, verify write permissions on the test-set path, and re-run with --debug to see the exact failing file path. To bypass gob while triaging, set KEPLOY_MOCK_FORMAT=yaml (or remove record.mockFormat from keploy.yml) to fall back to YAML",
					zap.String("testSetPath", job.testSetPath),
					zap.String("mockName", job.mock.Name),
					zap.String("mockOutputDir", ys.MockPath))
			}
		case <-ys.gobStop:
			ys.drainAndClose()
			return
		}
	}
}

// drainAndClose is the shutdown path for the writer goroutine. It
// consumes every job still buffered in gobQueue and records any
// encoding failures into gobFlushErr so Close() can surface them to
// the caller — previously these errors were dropped on the floor and
// Close would return nil even when the shutdown lost mocks.
func (ys *MockYaml) drainAndClose() {
	for {
		select {
		case job := <-ys.gobQueue:
			if err := ys.gobWriteOne(job); err != nil {
				ys.gobMu.Lock()
				ys.gobFlushErr = errors.Join(ys.gobFlushErr, err)
				ys.gobMu.Unlock()
				utils.LogError(ys.Logger, err, "failed to persist a queued gob mock while shutting down; check disk space on the mocks output directory, verify write permissions on the test-set path, and retry. To keep recording while investigating, set KEPLOY_MOCK_FORMAT=yaml (or remove record.mockFormat from keploy.yml) to fall back to YAML",
					zap.String("testSetPath", job.testSetPath),
					zap.String("mockName", job.mock.Name),
					zap.String("mockOutputDir", ys.MockPath))
			}
		default:
			ys.gobFlushAndClose()
			return
		}
	}
}

// persisted is mock as a mocks.gob holds it: its origin as a file holds one
// (models.Origin.Persisted, without what only travels), on a copy when that
// differs; mock itself otherwise. gob writes every field a mock has.
func persisted(mock *models.Mock) *models.Mock {
	if mock == nil {
		return nil
	}
	if p := mock.Origin.Persisted(); p != mock.Origin {
		c := mock.ShallowCopy()
		c.Origin = p
		return c
	}
	return mock
}

func (ys *MockYaml) gobWriteOne(job gobWriteJob) error {
	ys.gobMu.Lock()
	defer ys.gobMu.Unlock()
	return ys.gobEncodeLocked(job.mock, job.testSetPath, job.filename)
}

// gobEncodeLocked encodes mock into the gob file of its test set, opening it
// first when the writer is on another (or none), and counts it as encoded
// at the offset its bytes end at: it is written once the file has taken
// them (gobFileWriter). A mock that fails here is never counted as written.
func (ys *MockYaml) gobEncodeLocked(mock *models.Mock, mockPath, mockFileName string) error {
	want := filepath.Join(mockPath, mockFileName+".gob")
	if ys.gobFilePath != want || ys.gobFile == nil {
		if err := ys.gobReopenLocked(mockPath, mockFileName); err != nil {
			return err
		}
	}
	if err := ys.gobEnc.Encode(persisted(mock)); err != nil {
		return err
	}
	ys.gobFileW.encoded(ys.gobFileW.took + uint64(ys.gobBufw.Buffered()))
	return nil
}

func (ys *MockYaml) gobReopenLocked(mockPath, mockFileName string) error {
	if ys.gobFile != nil {
		// The file the writer leaves is finished as at Close: what it could
		// not take is said (gobFlushErr), and its mocks not written stay
		// counted as such (gobQueued less gobWritten).
		ys.gobFinishFileLocked()
	}
	if err := os.MkdirAll(mockPath, 0o777); err != nil {
		return fmt.Errorf("mkdir mock dir: %w", err)
	}
	filePath := filepath.Join(mockPath, mockFileName+".gob")
	// A file holds one gob stream: its type table lives in the encoder that
	// began it, so a new encoder cannot write on after it (the reader's one
	// decoder fails at the second type table). A file that already holds
	// mocks is therefore kept by carrying them over (gobCarryOverLocked):
	// the writer begins its stream in a file beside it, encodes the file's
	// mocks into it first, and puts it in the file's place before it writes
	// any mock of its own. The file is replaced whole or not at all.
	//
	// The writer used to open the file with O_TRUNC, on the idea that each
	// writer owns its file. A writer that adds to a set it did not begin
	// (keploy mock replay --on-miss record) or comes back to a set it left
	// (another test set's mocks in between, a Close in between) then wiped
	// the mocks the set held: the recorded set was gone.
	carry, err := gobFileHoldsData(filePath)
	if err != nil {
		return err
	}
	open := filePath
	var f *os.File
	if carry {
		f, err = createTempBeside(filePath, ".append.*.tmp")
		if err != nil {
			return fmt.Errorf("start a gob stream beside %s to add to it: %w", filePath, err)
		}
		open = f.Name()
	} else if f, err = os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666); err != nil {
		return fmt.Errorf("open gob mock file: %w", err)
	}
	ys.gobFile = f
	ys.gobFileW = &gobFileWriter{w: gobFileOf(f), written: &ys.gobWritten}
	// 256 KB buffer holds dozens of mocks before a syscall; bufio
	// autoflushes at fill. Shutdown drains explicitly.
	ys.gobBufw = bufio.NewWriterSize(ys.gobFileW, 256*1024)
	if _, werr := ys.gobBufw.WriteString(gobMockMagic); werr != nil {
		ys.gobAbandonLocked(open, carry)
		return fmt.Errorf("write gob magic: %w", werr)
	}
	ys.gobEnc = gob.NewEncoder(ys.gobBufw)
	if carry {
		if cerr := ys.gobCarryOverLocked(filePath, open); cerr != nil {
			ys.gobAbandonLocked(open, true)
			return fmt.Errorf("%s was left as it was, and the mock not added to it: its mocks could not be carried into the stream the writer adds to it in: %w", filePath, cerr)
		}
	}
	ys.gobFilePath = filePath
	ys.gobPathSeen.Store(filePath)
	return nil
}

// gobFileHoldsData says whether path is a file with something in it, which
// a writer opening it must keep. An absent or empty file holds nothing.
func gobFileHoldsData(path string) (bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat gob mock file: %w", err)
	}
	return info.Size() > 0, nil
}

// createTempBeside creates a temp file in path's directory, named after it
// with pattern, with path's permissions (os.CreateTemp alone makes it 0600,
// which would narrow the file it replaces).
func createTempBeside(path, pattern string) (*os.File, error) {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+pattern)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(f.Name(), mode); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

// gobCarryOverLocked encodes the mocks the gob file at filePath holds into
// the writer's new stream, which begins in the temp file tmpPath, and puts
// that file in filePath's place; the writer then writes on in filePath. The
// mocks are written to the disk before the replace, so a stop at any point
// leaves filePath whole: as it was, or with every mock it held. A tail the
// file was cut in (an interrupted write) is a mock no reader can read; it is
// not carried, which is said. Any other read error keeps the file as it was.
func (ys *MockYaml) gobCarryOverLocked(filePath, tmpPath string) error {
	carried := 0
	cut, err := forEachGobMockCut(filePath, func(m *models.Mock) error {
		carried++
		return ys.gobEnc.Encode(m)
	})
	if err != nil {
		return err
	}
	if err := ys.gobBufw.Flush(); err != nil {
		return fmt.Errorf("write the carried mocks: %w", err)
	}
	if err := ys.gobFile.Sync(); err != nil {
		return fmt.Errorf("sync the carried mocks: %w", err)
	}
	// Closed before the replace, and filePath opened again after it: a file
	// held open cannot be renamed on Windows. The encoder and its offsets go
	// on in the reopened file, which is the same stream.
	if err := ys.gobFile.Close(); err != nil {
		ys.gobFile = nil
		return fmt.Errorf("close the carried mocks: %w", err)
	}
	ys.gobFile = nil
	if err := replaceFile(ys.Logger, tmpPath, filePath); err != nil {
		return fmt.Errorf("put the carried mocks in place: %w", err)
	}
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		// The file is whole, with every mock it held; the writer cannot add
		// to it.
		return fmt.Errorf("open it to add to it: %w", err)
	}
	ys.gobFile = f
	ys.gobFileW.w = gobFileOf(f)
	if cut {
		ys.Logger.Warn("the gob mock file ended part-way through a mock (an interrupted write): that mock could not be read and is not kept; the mocks before it are, and new mocks are added after them",
			zap.String("mock_file", filePath), zap.Int("mocks_kept", carried))
	}
	return nil
}

// gobAbandonLocked drops a stream the writer could not begin: its file is
// closed, and removed when it is the temp file a carry-over began in
// (temp), so the file the writer meant to add to stays as it was.
func (ys *MockYaml) gobAbandonLocked(path string, temp bool) {
	if ys.gobFile != nil {
		_ = ys.gobFile.Close()
	}
	if temp {
		_ = os.Remove(path)
	}
	ys.gobFile = nil
	ys.gobBufw = nil
	ys.gobFileW = nil
	ys.gobEnc = nil
}

// gobFlushAndClose finalizes the on-disk gob stream. Flush and Close
// errors are both collected — the tail of the file (the bufio buffer)
// and the file descriptor itself each can fail independently (e.g.
// disk full between the last Encode and shutdown, or a permission
// change on the output directory). gobFlushErr records the combined
// result so Close() can surface it. Always returns state to nil so
// the next reopen starts fresh.
func (ys *MockYaml) gobFlushAndClose() error {
	ys.gobMu.Lock()
	defer ys.gobMu.Unlock()
	ys.gobFinishFileLocked()
	return ys.gobFlushErr
}

// gobFinishFileLocked flushes and closes the writer's file and leaves the
// writer with none. Its errors join gobFlushErr, with any encode/write errors
// the writer already had, rather than overwriting them: a successful final
// flush after earlier drops would otherwise mask the mid-session failures,
// and a file left for another test set's lost its tail with nothing said.
func (ys *MockYaml) gobFinishFileLocked() {
	var flushErr, closeErr error
	if ys.gobBufw != nil {
		flushErr = ys.gobBufw.Flush()
	}
	if ys.gobFile != nil {
		closeErr = closeGobFile(ys.gobFile)
	}
	if err := errors.Join(flushErr, closeErr); err != nil {
		ys.gobFlushErr = errors.Join(ys.gobFlushErr, fmt.Errorf("finishing %s: %w", ys.gobFilePath, err))
	}
	ys.gobFile = nil
	ys.gobBufw = nil
	ys.gobFileW = nil
	ys.gobEnc = nil
}

// Close drains the async gob writer and flushes the file. Safe to
// call multiple times, and safe to call between record sessions —
// the writer goroutine exits after flushing, and the next InsertMock
// starts a fresh goroutine via its own inline init (see
// insertMockGob). This is what makes re-record cycles (multiple
// Recorder.Start on the same mockDB instance) work without dropping
// mocks.
//
// It waits for the writer for as long as it has mocks to write, however long
// a slow disk takes, and only that: no clock decides that a disk is stuck. It
// used to give up after a fixed 5 s, and then once the writer had made no
// progress for 30 s; a recording on a loaded box or a slow disk then lost
// the mocks the writer had not written yet. A disk that fails a write says
// so (the write returns its error), and Close returns it, with how many of
// the session's mocks were not written, as models.ErrMocksNotWritten (a
// capture loss). A write that never returns is a disk only the caller can
// give up on: CloseUnless's stop.
func (ys *MockYaml) Close() error { return ys.CloseUnless(nil) }

// CloseUnless is Close that stops waiting once stop is closed (a user's
// second interrupt, a caller giving up on a disk that is stuck): the
// session's mocks not written yet are then models.ErrMocksNotWritten, said
// with how many they are, and a later Close waits for them again. That holds
// for the wait for the writer's lifecycle lock too, which an insert that
// found the queue full holds while it waits for room.
func (ys *MockYaml) CloseUnless(stop <-chan struct{}) error {
	// The next session looks at its sets' files again (insertFormat).
	ys.gobSets.Clear()
	// Hold the lifecycle lock for the entire teardown. While we wait
	// for the writer goroutine to drain, no concurrent InsertMock
	// can start a new writer — ys.gobRunning stays true and
	// ys.gobQueue / ys.gobStop / ys.gobDone cannot be reassigned out
	// from under the draining goroutine. A second concurrent Close()
	// blocks on this same lock and observes gobRunning=false after
	// the first Close completes, so close(gobStop) is never called
	// twice.
	if !ys.whileWriting(ys.lifecycle(), nil, stop) {
		return ys.notWritten()
	}
	defer ys.unlockLifecycle()
	if !ys.gobRunning {
		return nil
	}
	// Signal the writer to exit. Guarded by gobStopClosed so a retry
	// after a timeout cannot double-close the channel (which would
	// panic). The writer's stopClosed+running combination is the
	// "teardown in progress" state.
	if !ys.gobStopClosed {
		close(ys.gobStop)
		ys.gobStopClosed = true
	}
	if !ys.whileWriting(nil, ys.gobDone, stop) {
		// Leave gobRunning=true + gobStopClosed=true so a retry of
		// Close enters this function, skips the already-closed stop,
		// and just waits on gobDone again.
		return ys.notWritten()
	}
	ys.gobRunning = false
	ys.gobStopClosed = false
	// The writer has ended: every mock it did not write is said, from the
	// counts, before they start again for the next session.
	queued, left := ys.gobQueued.Load(), ys.MocksLeft()
	ys.gobQueued.Store(0)
	ys.gobWritten.Store(0)
	// Operator visibility: if the async queue filled up during the
	// session and inserts waited for room, report the count so disk
	// stalls / undersized queues are caught at post-run review
	// instead of requiring the user to notice slower rps.
	// Swap the overflow counter to zero atomically so re-record cycles
	// (next Start on the same MockYaml) don't count this session's
	// overflows again on their own Close.
	if overflows := ys.gobOverflows.Swap(0); overflows > 0 {
		if ys.Logger != nil {
			ys.Logger.Info("gob mock writer: inserts waited for room in its queue during the session (the disk was slower than the capture)",
				zap.Uint64("overflowedMocks", overflows),
				zap.Int("queueCapacity", cap(ys.gobQueue)),
				zap.String("hint", "queue capacity is the hard-coded channel size inlined in insertMockGob's writer-init block; raise it in code if disk/encoding is the bottleneck"))
		}
	}
	// Surface the final flush/close error from the writer goroutine so
	// the Recorder.Start deferred-cleanup log makes a disk-full / perm
	// change at shutdown visible to the operator instead of silently
	// dropping the tail of mocks.gob.
	ys.gobMu.Lock()
	flushErr := ys.gobFlushErr
	ys.gobFlushErr = nil
	ys.gobMu.Unlock()
	switch {
	case left > 0:
		if flushErr == nil {
			flushErr = errors.New("the gob mock writer ended without writing them")
		}
		return fmt.Errorf("%w: %d of the %d mocks handed to the gob mock writer were not written: %w",
			models.ErrMocksNotWritten, left, queued, flushErr)
	case flushErr != nil:
		// The file took every mock, then failed as it was closed (a write
		// back the OS reports only then): what it took may not be on disk.
		return fmt.Errorf("%w: the %d mocks handed to the gob mock writer were written, but the mock file failed as it was closed, so they may not be on disk: %w",
			models.ErrMocksNotWritten, queued, flushErr)
	}
	return nil
}

// whileWriting waits to take lock (a send; nil: none) or for done to close
// (nil: none), until stop closes. It reports whether it got what it waited
// for. It has no bound of its own: the writer is waited for for as long as
// it writes, and a disk that is stuck is given up on by the caller (stop).
func (ys *MockYaml) whileWriting(lock chan struct{}, done, stop <-chan struct{}) bool {
	// What it waits for wins over a stop that came with it.
	select {
	case lock <- struct{}{}:
		return true
	case <-done:
		return true
	default:
	}
	select {
	case lock <- struct{}{}:
		return true
	case <-done:
		return true
	case <-stop:
		return false
	}
}

// notWritten is what a Close that stopped waiting for the gob writer says:
// how many of the session's mocks the mock file had not taken when the stop
// came, as models.ErrMocksNotWritten. Read without the writer's locks, which
// a stuck writer holds.
func (ys *MockYaml) notWritten() error {
	return fmt.Errorf("%w: the stop was asked to end now; %d of the %d mocks handed to the gob mock writer were not written to %s",
		models.ErrMocksNotWritten, ys.MocksLeft(), ys.gobQueued.Load(), ys.gobFilePathSnapshot())
}

// MocksLeft is how many of the session's mocks the mock file has yet to take
// whole: those the gob writer has yet to encode, those it encoded that are
// still in its buffer or in a write the disk has not taken yet, and those it
// could not write.
func (ys *MockYaml) MocksLeft() uint64 {
	// Written first: a mock counted as written was queued before.
	written := ys.gobWritten.Load()
	queued := ys.gobQueued.Load()
	return queued - min(written, queued)
}

// gobFilePathSnapshot is the gob file the writer last opened. Read without
// gobMu, which a writer stuck in a write holds.
func (ys *MockYaml) gobFilePathSnapshot() string {
	p, _ := ys.gobPathSeen.Load().(string)
	return p
}

// readGobMocks decodes every mock in a mocks.gob file (see forEachGobMock).
// On a decode error it returns the mocks decoded before it with the error.
func readGobMocks(path string) ([]*models.Mock, error) {
	out, _, err := readGobMocksCut(path)
	return out, err
}

// readGobMocksCut is readGobMocks that also says whether the file ended
// part-way through a mock (see forEachGobMockCut).
func readGobMocksCut(path string) ([]*models.Mock, bool, error) {
	var out []*models.Mock
	cut, err := forEachGobMockCut(path, func(m *models.Mock) error {
		out = append(out, m)
		return nil
	})
	return out, cut, err
}

// GetFilteredMocks returns the test set's per-test pool: every mock whose
// lifetime is per-test, less the mapping prune (see readMockPools).
func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error) {
	pools, err := ys.readMockPools(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed, poolPerTest)
	if err != nil {
		return nil, err
	}
	return pools.Filtered, nil
}

// GetUnFilteredMocks returns the test set's session pool: every mock whose
// lifetime is session or connection, less the mapping prune (see
// readMockPools).
func (ys *MockYaml) GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error) {
	pools, err := ys.readMockPools(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed, poolSession)
	if err != nil {
		return nil, err
	}
	return pools.Unfiltered, nil
}

// GetTestSetMocks returns, from ONE read of the test set's mock file, what
// GetFilteredMocks and GetUnFilteredMocks return for the same arguments, what
// GetUnFilteredMocks returns without the mapping maps (AllSession), and every
// per-test candidate before the prune and the window filter (AllPerTest).
//
// A replay needs both pools of every test set it runs. Fetched through the two
// methods above, each of which reads and decodes the whole file, the file was
// decoded twice before the first test could run, and the report's mock lookup
// decoded it a third time; decoding a large mocks.yaml is most of a replay's
// start-up.
func (ys *MockYaml) GetTestSetMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (models.TestSetMocks, error) {
	return ys.readMockPools(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed, poolPerTest|poolSession|poolAllSession|poolAllPerTest)
}

// Callers find GetTestSetMocks through this optional interface, so a signature
// that drifted from it would silently send them back to one read per pool.
var _ pkg.TestSetMocksReader = (*MockYaml)(nil)

// IndexTestSetMocks is GetTestSetMocks that leaves every mock's payload in a
// yaml or json mock file (models.Mock.LeavePayloadOnDisk): what it returns
// holds a small entry per mock, and TestSetMocks.Payloads reads each payload
// back, one mock at a time, when the mock is sent. A mocks.gob is one gob
// stream, whose mocks cannot be read back one at a time: its mocks are
// returned whole, and Payloads is nil.
func (ys *MockYaml) IndexTestSetMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) (models.TestSetMocks, error) {
	return ys.readMockPools(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed, poolPerTest|poolSession|poolAllSession|poolAllPerTest|payloadsOnDisk)
}

var _ pkg.TestSetMockIndexReader = (*MockYaml)(nil)

// WalkTestSetMocks hands visit every mock of the test set's mock file, one at a
// time as it is decoded, with the pools IndexTestSetMocks returns it in
// (AllPerTest, AllSession): a mocks.gob mock by mock, a yaml or json file
// document by document. Nothing keeps a mock once visit returns, so a reader
// that keeps a few fields of each holds one decoded mock at a time, never the
// set. visit must not keep m. The documents the decoders skip are not visited,
// and the warnings a read of the file logs (a cut file, a skipped document,
// requirements no mock meets) are left to the replay's own read of it.
func (ys *MockYaml) WalkTestSetMocks(ctx context.Context, testSetID string, visit func(m *models.Mock, perTest, session bool)) error {
	if visit == nil {
		return nil
	}
	_, err := ys.readMockPoolsVisiting(ctx, testSetID, time.Time{}, time.Time{}, nil, nil, poolAllPerTest|poolAllSession, visit)
	return err
}

var _ pkg.TestSetMockWalker = (*MockYaml)(nil)

// mockPools selects the pools readMockPools builds.
type mockPools uint8

const (
	// poolPerTest builds TestSetMocks.Filtered.
	poolPerTest mockPools = 1 << iota
	// poolSession builds TestSetMocks.Unfiltered.
	poolSession
	// poolAllSession builds TestSetMocks.AllSession.
	poolAllSession
	// poolAllPerTest builds TestSetMocks.AllPerTest.
	poolAllPerTest
	// payloadsOnDisk is no pool: it has a yaml or json file's mocks leave
	// their payloads in it (IndexTestSetMocks).
	payloadsOnDisk
)

// mockRouter sorts a mock file's mocks into the candidate lists of the pools
// readMockPools builds, one mock at a time, as they are decoded.
type mockRouter struct {
	want                  mockPools
	mocksThatHaveMappings map[string]bool
	mocksWeNeed           map[string]bool
	// perTest is the per-test pool's candidates, in file order.
	perTest []*models.Mock
	// session is the session pool's candidates, in file order. When
	// poolAllSession is wanted it also holds the mocks the prune drops.
	session []*models.Mock
	// inBoth indexes the session candidates that are per-test candidates
	// too (gob PostgresV2 mocks of session or connection lifetime).
	inBoth []int
	// allPerTest is every per-test candidate, pruned or not, in file order,
	// when poolAllPerTest is wanted.
	allPerTest []*models.Mock
	// visit, when set, is handed each mock as it is routed, with the pools
	// AllPerTest and AllSession would hold it in, instead of any list keeping
	// it (WalkTestSetMocks).
	visit func(m *models.Mock, perTest, session bool)
}

// pruned reports whether the mapping prune drops the mock: it is mapped to a
// specific test and this run does not need it. A mock that is every test's by
// statement is kept, whichever test's mapping names it (sharedByStatement).
func (r *mockRouter) pruned(mock *models.Mock) bool {
	_, isMappedToSpecificTest := r.mocksThatHaveMappings[mock.Name]
	_, isNeededForCurrentRun := r.mocksWeNeed[mock.Name]
	return isMappedToSpecificTest && !isNeededForCurrentRun && !sharedByStatement(mock)
}

// sharedByStatement reports whether a mock is served to every test because a
// statement says so: its recorder stated it session or connection lifetime,
// or it is a call the recorder stated per-test that keploy.yaml declares
// shared (sessionCalls). A replay's mapping names it under the test that
// consumed it, but it is not that test's alone: a run of other tests needs it
// too (a connection's setup, a token another test fetched). A mock recorded
// before lifetimes were stated is pruned by the mapping as it always was, but
// for a MySQL prepared statement's lifecycle (PREPARE, CLOSE, RESET), which
// is its connection's whenever it was recorded (models.DeriveLifetime).
func sharedByStatement(mock *models.Mock) bool {
	if !mock.StatesLifetime() {
		return mock.IsMySQLStatementLifecycle()
	}
	mock.DeriveLifetime()
	lt := mock.TestModeInfo.Lifetime
	return lt == models.LifetimeSession || lt == models.LifetimeConnection
}

// route classifies one decoded mock. fromGob marks a mock read from mocks.gob,
// whose per-test pool keeps the PostgresV2 dual-pool quirk.
func (r *mockRouter) route(mock *models.Mock, fromGob bool) {
	pruned := r.pruned(mock)
	if pruned && r.want&(poolAllSession|poolAllPerTest) == 0 {
		return
	}
	// Unification (Phase 3): resolve the mock's typed Lifetime once via
	// DeriveLifetime — which reads Spec.Metadata["type"] first and falls back
	// to the legacy kind-switch only for pre-tag recordings (logged via
	// LegacyKindFallbackFires). Routing is then purely Lifetime-driven:
	// LifetimePerTest lands in the per-test pool, Session and Connection in
	// the session pool. Untagged mocks of the legacy implicit-session kinds
	// (HTTP, Postgres, MySQL, ...) still resolve to Session via the
	// kind-fallback, so pre-tag recordings keep replaying identically.
	// metadata["scope"] is NOT consulted.
	mock.DeriveLifetime()
	lifetime := mock.TestModeInfo.Lifetime
	inSession := lifetime == models.LifetimeSession || lifetime == models.LifetimeConnection
	// The gob reader puts a PostgresV2 mock in the per-test pool whatever its
	// lifetime, so one of session or connection lifetime is a candidate for
	// both pools there. The YAML reader does not.
	inPerTest := lifetime == models.LifetimePerTest || (fromGob && mock.Kind == models.PostgresV2)
	if r.visit != nil {
		r.visit(mock, inPerTest, inSession)
		return
	}
	toPerTest := inPerTest && !pruned && r.want&poolPerTest != 0
	toSession := inSession && (r.want&poolAllSession != 0 || (!pruned && r.want&poolSession != 0))
	if inPerTest && r.want&poolAllPerTest != 0 {
		r.allPerTest = append(r.allPerTest, mock)
	}
	if toPerTest {
		r.perTest = append(r.perTest, mock)
	}
	if toSession {
		if toPerTest {
			r.inBoth = append(r.inBoth, len(r.session))
		}
		r.session = append(r.session, mock)
	}
}

// separateSharedMocks gives the session pool its own copy of each mock that
// the per-test pool kept too. Read from the file once per pool, each pool had
// its own; read once, both would hold one decode. Without a window the two
// pools would share the *Mock itself, and with one each filter's DeepCopy
// would still share the payloads below the top-level slices, which a
// MockMutator changes in place. A mock the window filter dropped from the
// per-test pool is not copied. filtered is the built per-test pool.
func (r *mockRouter) separateSharedMocks(filtered []*models.Mock) error {
	if len(r.inBoth) == 0 {
		return nil
	}
	// With a window the filter keeps copies, so a kept mock is found by name.
	// A name that is in the file twice may copy a mock that did not need it,
	// which costs a copy and changes nothing.
	kept := make(map[string]bool, len(filtered))
	for _, m := range filtered {
		kept[m.Name] = true
	}
	for _, i := range r.inBoth {
		if !kept[r.session[i].Name] {
			continue
		}
		clone, err := cloneGobMock(r.session[i])
		if err != nil {
			return err
		}
		r.session[i] = clone
	}
	return nil
}

// cloneGobMock returns a copy of a mock decoded from a gob mock file that is
// what decoding it again would return, sharing nothing with it. Mock.DeepCopy
// shares the payloads below the top-level slices, and gives every nil payload
// slice an empty one.
func cloneGobMock(mock *models.Mock) (*models.Mock, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(mock); err != nil {
		return nil, fmt.Errorf("copy gob mock %q: %w", mock.Name, err)
	}
	var clone models.Mock
	if err := gob.NewDecoder(&buf).Decode(&clone); err != nil {
		return nil, fmt.Errorf("copy gob mock %q: %w", mock.Name, err)
	}
	return &clone, nil
}

// sessionPools builds the wanted session pools from the candidates.
// FilterConfigMocks treats every session and connection mock on its own (it
// never drops one, and orders them by a stable sort), so the pool of the
// unpruned candidates, less the pruned names, is the pool of the pruned
// candidates: Unfiltered is taken from AllSession rather than filtered twice.
func (r *mockRouter) sessionPools(ctx context.Context, logger *zap.Logger, afterTime, beforeTime time.Time, out *models.TestSetMocks) {
	if r.want&poolAllSession == 0 {
		if r.want&poolSession != 0 {
			// The disk loader runs lax; the agent-level filter enforces
			// strictness based on config.
			out.Unfiltered = pkg.FilterConfigMocks(ctx, logger, r.session, afterTime, beforeTime, false)
		}
		return
	}
	out.AllSession = pkg.FilterConfigMocks(ctx, logger, r.session, afterTime, beforeTime, false)
	if r.want&poolSession != 0 {
		out.Unfiltered = make([]*models.Mock, 0, len(out.AllSession))
		for _, m := range out.AllSession {
			if !r.pruned(m) {
				out.Unfiltered = append(out.Unfiltered, m)
			}
		}
	}
}

// readMockPools reads and decodes the test set's mock file once and builds the
// wanted pools from it:
//
//   - Filtered (poolPerTest): the per-test mocks, in file order. From a gob
//     file, PostgresV2 mocks too, through the lax window filter, which the
//     proven switch skips (a test owns what its mapping lists).
//   - Unfiltered (poolSession): the session and connection mocks, through the
//     lax FilterConfigMocks.
//   - AllSession (poolAllSession): Unfiltered without the mapping prune.
//   - AllPerTest (poolAllPerTest): the per-test candidates Filtered is drawn
//     from, before the mapping prune and the window filter.
//
// The mapping prune drops a mock that is mapped to a specific test this run
// does not need; Filtered and Unfiltered apply it.
func (ys *MockYaml) readMockPools(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool, want mockPools) (models.TestSetMocks, error) {
	return ys.readMockPoolsVisiting(ctx, testSetID, afterTime, beforeTime, mocksThatHaveMappings, mocksWeNeed, want, nil)
}

// readMockPoolsVisiting is readMockPools that, given visit, hands it each mock
// as it is decoded (mockRouter.visit) and builds no pool: what it returns then
// lists only the documents the decoders skipped. A visiting read logs
// nothing: what the replay's own read of the file logs is not logged again
// (WalkTestSetMocks), and a read or decode error is returned to its caller.
func (ys *MockYaml) readMockPoolsVisiting(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool, want mockPools, visit func(m *models.Mock, perTest, session bool)) (models.TestSetMocks, error) {
	var out models.TestSetMocks
	r := &mockRouter{
		want:                  want,
		mocksThatHaveMappings: mocksThatHaveMappings,
		mocksWeNeed:           mocksWeNeed,
		perTest:               make([]*models.Mock, 0),
		session:               make([]*models.Mock, 0),
		visit:                 visit,
	}
	if visit != nil {
		want &^= payloadsOnDisk
	}

	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockFileName, ys.Format))
	lock.RLock()
	defer lock.RUnlock()

	// Prefer gob binary format when present (low-latency record output).
	// gob is mutually exclusive with the yaml/json text formats, so we
	// short-circuit and return before the auto-detect reader runs.
	gobPath := filepath.Join(path, mockFileName+".gob")
	if _, err := os.Stat(gobPath); err == nil {
		logger := ys.Logger.With(zap.String("mock_file", gobPath))
		if visit != nil {
			logger = zap.NewNop()
		}
		// A set with a text mock file beside its mocks.gob was left so by an
		// older keploy (an insert now refuses to add to such a set): the
		// text file's mocks are not read, which is said, never left silent.
		if text := ys.textMockFileBeside(ctx, path, mockFileName); text != "" {
			logger.Warn("the test set holds two mock files and only the gob one is read: the mocks in the other are not served",
				zap.String("not_read", text),
				zap.String("next_step", "keep the file that holds the set and move the other out of the test set"))
		}
		connFailures := 0
		var deps models.MockDepsCheck
		// One mock at a time: a visiting read keeps none of them.
		cut, err := forEachGobMockCut(gobPath, func(mock *models.Mock) error {
			if mock.Kind == models.ConnectionFailure {
				// gob has no per-kind decoder: a connection failure is
				// validated here, as DecodeMocks and DecodeMocksJSON validate
				// theirs.
				if !connFailureSupported(mock, logger, false) {
					addSkipped(&out, mock.Name, mock.Kind)
					return nil
				}
				connFailures++
			}
			r.route(mock, true)
			if visit == nil {
				// After routing, which derives the mock's lifetime unless the
				// mapping prune drops it first: its links are then read
				// without deriving it again on a copy.
				deps.Add(mock)
			}
			return nil
		})
		if err != nil {
			return models.TestSetMocks{}, err
		}
		if visit != nil {
			return out, nil
		}
		if cut {
			logger.Warn("the mock file ends part-way through its last mock, probably because a write was interrupted (the recorder stopped, or the disk filled), and that mock was not read",
				zap.String("next_step", "re-record the test set"))
		}
		warnConnFailuresNotReplayed(logger, testSetID, connFailures)
		warnUnmetRequirements(logger, testSetID, gobPath, deps.Unmet())
		if want&poolPerTest != 0 {
			if provenswitch.On() {
				// Under the proven switch a test owns the mocks its mapping
				// lists, whatever their recorded times say, so the per-test
				// pool is read whole, as a yaml or json file's is, for the
				// agent to serve by mapping. The window filter left out a mock
				// whose response is stamped before its request (a recorder's
				// bad stamp, not a bad answer) before the agent saw it: the
				// test that owns it was judged for a recorded call it did not
				// make, and its call to that dependency missed.
				out.Filtered = r.perTest
			} else {
				out.Filtered = pkg.FilterTcsMocks(ctx, logger, r.perTest, afterTime, beforeTime, false)
			}
		}
		if err := r.separateSharedMocks(out.Filtered); err != nil {
			return models.TestSetMocks{}, err
		}
		r.sessionPools(ctx, ys.Logger, afterTime, beforeTime, &out)
		out.AllPerTest = r.allPerTest
		return out, nil
	}

	// Auto-detect the mocks file's format (may be yaml or json regardless
	// of the currently-configured StorageFormat) so replay keeps working
	// across format switches.
	reader, err := yaml.NewMockReaderAny(ctx, ys.Logger, path, mockFileName, ys.Format)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			if visit != nil {
				return out, nil
			}
			// No mocks file in either format — nothing to replay. Use the
			// lax (strict=false) filter to mirror the gob branch above and
			// the agent-level filter; strictness is decided downstream.
			if want&poolPerTest != 0 {
				out.Filtered = pkg.FilterTcsMocks(ctx, ys.Logger, r.perTest, afterTime, beforeTime, false)
			}
			r.sessionPools(ctx, ys.Logger, afterTime, beforeTime, &out)
			out.AllPerTest = r.allPerTest
			return out, nil
		}
		if visit != nil {
			return models.TestSetMocks{}, err
		}
		msg := "failed to read the mocks from config file"
		if want&poolPerTest != 0 {
			msg = "failed to read the mocks from file"
		}
		utils.LogError(ys.Logger, err, msg, zap.String("session", filepath.Base(path)))
		return models.TestSetMocks{}, err
	}
	defer reader.Close()

	// The decoders' skip ERRORs name the mock, and mock names repeat in every
	// test set, so they say which file.
	logger := ys.Logger.With(zap.String("mock_file", reader.Path()))
	if visit != nil {
		// One document at a time, payload and all, kept by no one: no
		// payload source, no index (src stays nil).
		logger = zap.NewNop()
	}
	var src *payloadSource
	if want&payloadsOnDisk != 0 {
		if src, err = newPayloadSource(reader); err != nil {
			return models.TestSetMocks{}, err
		}
		out.Payloads = src
	}
	hasContent := false
	connFailures := 0
	var unmet []models.UnmetRequirement
	// An index read keeps its index for a later read of the same content,
	// and is answered from one kept, when the process keeps them (see
	// indexCache).
	var (
		probe   *fileProbe
		builder *indexBuilder
		kept    *indexedFile
	)
	if src != nil && mockIndexCache.on() {
		if probe, err = newFileProbe(reader); err == nil {
			if kept = mockIndexCache.lookup(probe); kept == nil {
				builder = newIndexBuilder(mockIndexCache.budgetNow())
			}
		}
	}
	if kept != nil {
		kept.logAgain(logger)
		for _, d := range kept.docs {
			if d.skipped != nil {
				d.skipped.addTo(&out)
			}
			for _, entry := range d.mocks {
				r.route(entry.IndexEntryFrom(src), false)
			}
		}
		hasContent, connFailures, unmet = kept.hasContent, kept.connFailures, kept.unmet
	} else {
		if unmet, hasContent, connFailures, err = ys.decodeMockDocs(reader, logger, builder, src, r, &out, path); err != nil {
			return models.TestSetMocks{}, err
		}
		if builder != nil {
			builder.finish(probe, hasContent, connFailures, unmet)
		}
	}
	if visit != nil {
		return out, nil
	}
	warnConnFailuresNotReplayed(logger, testSetID, connFailures)
	warnUnmetRequirements(logger, testSetID, reader.Path(), unmet)
	return ys.finishMockPools(ctx, r, want, hasContent, path, afterTime, beforeTime, out)
}

// decodeMockDocs decodes every document of a yaml or json mock file, routing
// each mock it decodes (r) and listing the documents it skips in out. With
// src, each mock's payload is left in the file; with builder, each document
// is recorded for the index cache, and what the decoders log through it. It
// returns the requirements no mock of the file meets, whether the file holds
// any document, and how many connection failures it holds.
func (ys *MockYaml) decodeMockDocs(reader *yaml.MockReader, logger *zap.Logger, builder *indexBuilder, src *payloadSource, r *mockRouter, out *models.TestSetMocks, path string) (unmet []models.UnmetRequirement, hasContent bool, connFailures int, err error) {
	// When the mocks file is JSON we go through ReadNextDocJSON +
	// DecodeMocksJSON, skipping the yaml.Node bridge entirely. YAML files
	// keep the original path for full backwards compatibility with
	// existing recordings.
	readerIsJSON := reader.Format() == yaml.FormatJSON
	decodeLogger := logger
	if builder != nil {
		decodeLogger = builder.decodeLogger(logger)
	}
	var deps models.MockDepsCheck
	for {
		doc, readErr, decodeErr := nextDoc(reader, decodeLogger, false)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, false, 0, fmt.Errorf("failed to decode the file documents. error: %v", readErr.Error())
		}
		hasContent = true
		if decodeErr != nil {
			msg := "failed to decode the config mocks from doc"
			if readerIsJSON {
				msg = "failed to decode the config mocks from json doc"
			}
			// logger, not ys.Logger: a visiting read's is a Nop one, its
			// error returned to its caller (readMockPoolsVisiting).
			utils.LogError(logger, decodeErr, msg, zap.String("session", filepath.Base(path)))
			return nil, false, 0, fmt.Errorf("failed to decode the mocks in %s: %w", reader.Path(), decodeErr)
		}
		var skipped *skippedEntry
		if len(doc.mocks) == 0 {
			skipped = skippedDocOf(doc.raw, readerIsJSON, doc.incomplete, doc.name, doc.kind)
			skipped.addTo(out)
		}
		for _, mock := range doc.mocks {
			if mock.Kind == models.ConnectionFailure {
				connFailures++
			}
			r.route(mock, false)
			if r.visit == nil {
				deps.Add(mock) // after routing, as the gob reader
			}
			if src != nil {
				// Last: routing and the links read the payload.
				mock.LeavePayloadOnDisk(src, models.PayloadAt{Offset: doc.off, Length: doc.n, Name: mock.Name})
			}
		}
		if builder != nil {
			builder.add(doc.mocks, skipped)
		}
	}
	return deps.Unmet(), hasContent, connFailures, nil
}

// finishMockPools builds the wanted pools of a yaml or json mock file from
// the candidates its documents were routed to (r).
func (ys *MockYaml) finishMockPools(ctx context.Context, r *mockRouter, want mockPools, hasContent bool, path string, afterTime, beforeTime time.Time, out models.TestSetMocks) (models.TestSetMocks, error) {
	if want&poolPerTest != 0 {
		if !hasContent {
			// The file exists and parsed cleanly; it just holds no documents. That
			// is now a reachable state rather than a corruption signal: a recording
			// whose every mock was unencodable writes only the version comment (the
			// recorder skips a bad mock instead of dying, and a skipped mock leaves
			// no document behind). A malformed or truncated file does NOT land here
			// — the decode above returns an error for that.
			//
			// So report zero mocks, loudly, instead of failing the whole test set.
			// The hard error made every test in the set unrunnable and said nothing
			// about why; zero mocks lets the run proceed and produce per-test
			// results that point at the real problem.
			ys.Logger.Warn("mock file contains no mocks; every test in this set will run without mocks",
				zap.String("session", filepath.Base(path)),
				zap.String("next_step", "check the recording logs for dropped mocks (mocks-dropped) — if non-zero, the payloads could not be encoded and the set needs re-recording"))
		} else {
			// NO disk-level window filter: return every per-test mock this
			// test-set needs and let the agent's SetMocksWithWindow decide
			// what to keep. FilterTcsMocks discards the unfiltered (out-of-
			// window) slice, which would silently eat STARTUP-INIT mocks
			// (app-bootstrap traffic whose req-timestamp is strictly before
			// the first test's window start — Hibernate pool init, HikariCP
			// connection validation, driver handshake). The agent's pre-
			// filter promotes those to the session pool via its
			// firstWindowStart cache; dropping them here would defeat that.
			//
			// Pruning based on TestCase mappings (mocksWeNeed /
			// mocksThatHaveMappings) already ran in route, so what reaches
			// here is the minimal relevant set.
			ys.Logger.Debug("per-test mocks count", zap.Int("count", len(r.perTest)))
			out.Filtered = r.perTest
		}
	}
	// No YAML or JSON mock is a candidate for both pools today; this keeps a
	// future one from being shared.
	if err := r.separateSharedMocks(out.Filtered); err != nil {
		return models.TestSetMocks{}, err
	}
	r.sessionPools(ctx, ys.Logger, afterTime, beforeTime, &out)
	out.AllPerTest = r.allPerTest
	return out, nil
}

// addSkipped lists a document the decoders skipped in out.Skipped (see
// models.TestSetMocks).
func addSkipped(out *models.TestSetMocks, name string, kind models.Kind) {
	if name == "" {
		return
	}
	if out.Skipped == nil {
		out.Skipped = map[string]models.Kind{}
	}
	out.Skipped[name] = kind
}

func (ys *MockYaml) getNextID() int64 {
	return atomic.AddInt64(&ys.idCounter, 1)
}

func (ys *MockYaml) GetHTTPMocks(ctx context.Context, testSetID string, mockPath string, mockFileName string) ([]*models.HTTPDoc, error) {

	if ys.MockName != "" {
		ys.MockName = mockFileName
	}
	ys.MockPath = mockPath

	tcsMocks, err := ys.GetUnFilteredMocks(ctx, testSetID, time.Time{}, time.Time{}, nil, nil)
	if err != nil {
		return nil, err
	}

	var httpMocks []*models.HTTPDoc
	for _, mock := range tcsMocks {
		if mock.Kind != "Http" {
			continue
		}
		var httpMock models.HTTPDoc
		httpMock.Kind = mock.GetKind()
		httpMock.Name = mock.Name
		httpMock.Spec.Request = *mock.Spec.HTTPReq
		httpMock.Spec.Response = *mock.Spec.HTTPResp
		httpMock.Spec.Metadata = mock.Spec.Metadata
		httpMock.Version = string(mock.Version)
		httpMocks = append(httpMocks, &httpMock)
	}

	return httpMocks, nil
}

func (ys *MockYaml) DeleteMocksForSet(ctx context.Context, testSetID string) error {
	_ = ctx
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	// The set's next insert takes the configured format again (insertFormat).
	ys.gobSets.Delete(setFileKey(filepath.Join(ys.MockPath, testSetID), mockFileName))

	// Refuse any testSetID that could escape the configured mocks
	// directory. The test-set layout is "<MockPath>/<testSetID>/mocks.*",
	// so the ID must be a single non-empty path segment. A re-record
	// request with testSetID="../../etc" or "a/b" could otherwise
	// turn os.Remove into an arbitrary-file delete or pick up a
	// different test-set's directory; guard before we touch the
	// filesystem.
	//
	// The rules (no separator on either platform's spelling, no volume
	// qualifier, not absolute, no "." / ".." element, while still
	// allowing a '..' SUBSTRING like "v1..v2") live in utils/pathsafe,
	// shared with DebugFileSink.RotateForScope — the other place a
	// test-set ID reaches the filesystem. One definition, so the two
	// cannot drift; see the package doc for why each rule is there,
	// including the Windows "C:" escape from keploy#4045 review round 26.
	if err := pathsafe.ValidateSingleSegment(testSetID, false); err != nil {
		return fmt.Errorf("rejecting DeleteMocksForSet: testSetID %q must be a non-empty single-segment name (no separators, no drive/volume prefix, not '.' or '..') under the mocks output directory: %w", testSetID, err)
	}
	path := filepath.Join(ys.MockPath, testSetID)

	// Delete all three mock-file variants for this test set:
	//   - mocks.yaml / mocks.json (text formats — either may be present
	//     depending on StorageFormat at record time, and a stale yaml
	//     must not shadow a fresh json rerecord, or vice versa).
	//   - mocks.gob (binary format — GetFilteredMocks prefers it when
	//     present, so leaving it around defeats a yaml/json refresh).
	// Missing files are tolerated; only permission/ownership errors
	// surface here.
	candidates := []string{
		filepath.Join(path, mockFileName+"."+yaml.FormatYAML.FileExtension()),
		filepath.Join(path, mockFileName+"."+yaml.FormatJSON.FileExtension()),
		filepath.Join(path, mockFileName+".gob"),
	}
	for _, candidate := range candidates {
		validated, err := yaml.ValidatePath(candidate)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to validate mock path for delete", zap.String("at_path", candidate))
			return err
		}
		if err := os.Remove(validated); err != nil && !os.IsNotExist(err) {
			utils.LogError(ys.Logger, err, "failed to delete stale mock file during refresh; check that the file is not read-only and that the current user owns the mocks output directory, ensure no other keploy process or editor has an open handle on it, then retry — missing files are tolerated, only permission/ownership errors surface here", zap.String("path", validated))
			return err
		}
	}

	ys.Logger.Info("Successfully cleared old mocks for refresh.", zap.String("testSet", testSetID))
	return nil
}

// mockFileVariants returns the three possible on-disk mock-file paths for a set
// directory (yaml / json / gob) — the full set DeleteMocksForSet manages. The
// base name defaults to "mocks" unless a custom MockName was configured.
func (ys *MockYaml) mockFileVariants(setDir string) []string {
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	return []string{
		filepath.Join(setDir, mockFileName+"."+yaml.FormatYAML.FileExtension()),
		filepath.Join(setDir, mockFileName+"."+yaml.FormatJSON.FileExtension()),
		filepath.Join(setDir, mockFileName+".gob"),
	}
}

// PromoteStagedSet replaces targetID's mock files with the ones record captured
// into stagingID (an atomic rename for the common same-format case; a format
// switch is rename-then-remove, see below), then removes the staging set. It is how
// `keploy mock record` avoids destroying an existing recording: capture streams
// into a staging set and is promoted only once the run has produced mocks and
// finished cleanly, so a failed, interrupted or zero-capture run leaves the
// existing set untouched (gaps W1/W14; design §P0b "no delete-first").
//
// The gob writer is an async, truncate-on-open stream, so Close() is called
// first to flush and finalize staging's file before it is moved. A flush error
// aborts the promote WITHOUT touching the target, so a corrupt or partial
// staged set never overwrites a good recording. Then, for each format, the
// staged file is renamed over the target (atomic on one filesystem) and any
// stale target variant of a format that was not staged is removed — mirroring
// DeleteMocksForSet's variant set so a prior format cannot shadow the promoted
// one. An empty staging set is refused, so a bug upstream can never blank the
// target.
func (ys *MockYaml) PromoteStagedSet(ctx context.Context, stagingID, targetID string) error {
	for _, id := range []string{stagingID, targetID} {
		if err := pathsafe.ValidateSingleSegment(id, false); err != nil {
			return fmt.Errorf("rejecting PromoteStagedSet: testSetID %q must be a non-empty single-segment name (no separators, no drive/volume prefix, not '.' or '..'): %w", id, err)
		}
	}

	// Finalize any open (gob) writer so staging's file is complete on disk before
	// it is moved. No-op for yaml/json, whose writes are synchronous. A flush
	// error means the staged recording is incomplete — do not promote it. It
	// waits for the writer until ctx ends: on a disk that is stuck, the
	// caller's give-up is then a counted failure, and the target is kept.
	if err := ys.CloseUnless(ctx.Done()); err != nil {
		return fmt.Errorf("not promoting staged set %q: finalizing its mock file failed, so %q is left untouched: %w", stagingID, targetID, err)
	}

	stagingDir := filepath.Join(ys.MockPath, stagingID)
	targetDir := filepath.Join(ys.MockPath, targetID)
	stagingVariants := ys.mockFileVariants(stagingDir)
	targetVariants := ys.mockFileVariants(targetDir)

	// Record which formats were staged BEFORE moving anything — pass 1 renames
	// the staged files away, so their absence afterwards must not be mistaken for
	// "not staged" in pass 2 (which would then delete what was just promoted).
	wasStaged := make([]bool, len(stagingVariants))
	anyStaged := false
	for i, sv := range stagingVariants {
		if _, err := os.Stat(sv); err == nil {
			wasStaged[i] = true
			anyStaged = true
		}
	}
	// Guard: never wipe the target based on an empty staging set.
	if !anyStaged {
		return fmt.Errorf("nothing staged to promote for %q; leaving %q untouched", stagingID, targetID)
	}

	if err := os.MkdirAll(targetDir, 0o777); err != nil {
		return fmt.Errorf("creating target set dir %q: %w", targetDir, err)
	}

	// Promote in two passes so a complete recording is always present in the
	// target. Pass 1 renames every staged file into place — an atomic replace per
	// format; on the common same-format path this is the whole promote, and a
	// failed rename leaves the previous recording untouched. Pass 2 then removes
	// any target variant of a format that was NOT staged, so a prior format
	// cannot shadow the promoted one. Removing first (the earlier single-pass
	// form) could delete the old recording before the new rename and, if that
	// rename then failed, leave the target empty — the data loss this function
	// exists to prevent.
	//
	// Residual (crash-only, self-healing on the next record): if the format
	// changed from gob to a text format, a crash between the passes leaves the
	// old gob shadowing the new text file until the next record, since the read
	// path prefers gob. No data is lost.
	for i := range stagingVariants {
		if !wasStaged[i] {
			continue
		}
		if err := os.Rename(stagingVariants[i], targetVariants[i]); err != nil {
			return fmt.Errorf("promoting staged mock file %q over %q: %w", stagingVariants[i], targetVariants[i], err)
		}
	}
	for i := range targetVariants {
		if wasStaged[i] {
			continue
		}
		// Validate before the destructive remove, as DeleteMocksForSet does — the
		// ID is already single-segment-checked and the filename is fixed, so this
		// is defense-in-depth against a future path-construction change.
		validated, verr := yaml.ValidatePath(targetVariants[i])
		if verr != nil {
			return fmt.Errorf("validating stale target mock path %q: %w", targetVariants[i], verr)
		}
		if rmErr := os.Remove(validated); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("removing stale target mock file %q: %w", validated, rmErr)
		}
	}

	if err := os.RemoveAll(stagingDir); err != nil {
		ys.Logger.Warn("promoted the staged mock set but could not remove its staging directory; it is safe to delete",
			zap.String("stagingDir", stagingDir), zap.Error(err))
	}
	ys.Logger.Info("promoted staged mock set", zap.String("from", stagingID), zap.String("to", targetID))
	return nil
}

// DiscardStagedSet removes a staging set left by a record that did not complete
// (failed, interrupted, or captured nothing), so the existing target set it was
// never promoted over stays intact. Also called before capture to clear any
// staging directory a previously-crashed run left behind.
func (ys *MockYaml) DiscardStagedSet(ctx context.Context, stagingID string) error {
	if err := pathsafe.ValidateSingleSegment(stagingID, false); err != nil {
		return fmt.Errorf("rejecting DiscardStagedSet: testSetID %q must be a non-empty single-segment name (no separators, no drive/volume prefix, not '.' or '..'): %w", stagingID, err)
	}
	// Release any open (gob) writer handle before removing the directory,
	// waiting for the writer until ctx ends: a disk that is stuck must not
	// hold a discard the caller has given up on.
	if err := ys.CloseUnless(ctx.Done()); err != nil {
		ys.Logger.Debug("discardStagedSet: closing the mock writer reported an error; removing the staging dir anyway", zap.Error(err))
	}
	stagingDir := filepath.Join(ys.MockPath, stagingID)
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("discarding staged set %q: %w", stagingID, err)
	}
	return nil
}

func (ys *MockYaml) GetCurrMockID() int64 {
	return atomic.LoadInt64(&ys.idCounter)
}

func (ys *MockYaml) ResetCounterID() {
	atomic.StoreInt64(&ys.idCounter, -1)
}

// SetCounterID seeds the mock-name counter so the NEXT InsertMock names its
// mock "mock-<id+1>". Used when APPENDING to an existing set (the `keploy mock
// replay --on-miss record` incremental-refresh path) so newly-captured mocks
// don't reuse names already present on disk. Recording a fresh set uses
// ResetCounterID (seed -1 → first mock is mock-0); appending seeds from the
// set's highest existing index instead.
func (ys *MockYaml) SetCounterID(id int64) {
	atomic.StoreInt64(&ys.idCounter, id)
}
