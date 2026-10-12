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
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
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
	// accept mocks for the next session. gobLifecycleMu guards the
	// transition between "running" and "quiesced"; gobRunning tracks
	// which state we are in. Do not use sync.Once here — it cannot be
	// reset, which was the original re-record bug.
	gobLifecycleMu sync.Mutex
	gobRunning     bool
	gobQueue       chan gobWriteJob
	gobStop        chan struct{}
	gobDone        chan struct{}
	gobMu          sync.Mutex
	gobFilePath    string
	gobFile        *os.File
	gobBufw        *bufio.Writer
	gobEnc         *gob.Encoder
	gobOverflows   atomic.Uint64
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

// replaceFile puts a rewritten mock file in place (yaml.ReplaceFile); tests
// swap it to make the replace fail.
var replaceFile = yaml.ReplaceFile

// mergeReqBodyNoise returns a fresh map combining the existing on-disk
// request-body noise with newly-detected noise carried on the MockState.
// Existing entries win on key collision (noise is monotonic), and every slice
// is copied so the result shares no backing storage with its inputs.
func mergeReqBodyNoise(existing, detected map[string][]string) map[string][]string {
	out := make(map[string][]string, len(existing)+len(detected))
	for k, v := range existing {
		vc := make([]string, len(v))
		copy(vc, v)
		out[k] = vc
	}
	for k, v := range detected {
		if _, ok := out[k]; ok {
			continue
		}
		vc := make([]string, len(v))
		copy(vc, v)
		out[k] = vc
	}
	return out
}

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

	reader, err := yaml.NewMockReaderF(ctx, ys.Logger, path, mockFileName, detectedFormat)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read the mocks from file", zap.String("at_path", filepath.Join(path, mockFileName+ext)))
		return err
	}
	defer reader.Close()

	// One mock at a time: read, decode, judge, and write the ones that stay
	// to the replacement before reading the next (see rewrite.go). Written
	// back in the format read, so a prune never migrates the file's format.
	rw := newMockFileRewriter(ys.Logger, path, mockFileName, detectedFormat)
	defer rw.discard()

	// Only build the per-mock log slice when debug logging is enabled
	// — on large test sets the allocation and reflection cost of
	// collecting names/kinds/metadata is a significant overhead if
	// the emitted log will be dropped by the logger anyway.
	debugEnabled := ys.Logger.Core().Enabled(zap.DebugLevel)
	total, kept, prunedCount, copied := 0, 0, 0, 0
	var prunedMocks []prunedMockInfo
	if debugEnabled {
		prunedMocks = make([]prunedMockInfo, 0, maxPrunedMocksLogged)
	}
	fileLogger := ys.Logger.With(zap.String("mock_file", reader.Path()))
	for {
		doc, readErr, decodeErr := nextDoc(reader, fileLogger, true)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			utils.LogError(ys.Logger, readErr, "failed to decode the file documents", zap.String("at_path", filepath.Join(path, mockFileName+ext)))
			return fmt.Errorf("failed to decode the file documents. error: %v", readErr.Error())
		}
		if decodeErr != nil {
			return fmt.Errorf("failed to decode the mocks in %s: %w", reader.Path(), decodeErr)
		}
		if doc.verbatim {
			// A document this keploy cannot decode is not one it can judge,
			// and a connection failure is not one it changes: copied through
			// as it is (see fileDoc.verbatim).
			if err := rw.copyDoc(doc); err != nil {
				return err
			}
			if !doc.incomplete {
				copied++
			}
			continue
		}
		for _, mock := range doc.mocks {
			total++
			if pruneKeeps(mock, mockNames, pruneBefore, startupCutoffTime) {
				if err := rw.write(mock); err != nil {
					return err
				}
				kept++
				continue
			}
			prunedCount++
			if debugEnabled && len(prunedMocks) < maxPrunedMocksLogged {
				prunedMocks = append(prunedMocks, prunedMockInfo{
					Name:     mock.Name,
					Kind:     string(mock.Kind),
					Metadata: mock.Spec.Metadata,
				})
			}
		}
	}

	// Done reading: close before the replace, which some platforms refuse
	// over a file that is still open.
	_ = reader.Close()
	if err := rw.commit(); err != nil {
		return err
	}

	ys.Logger.Debug("pruned mocks successfully",
		zap.String("testSetID", testSetID),
		zap.Int("total", total),
		zap.Int("kept", kept),
		zap.Int("pruned", prunedCount),
		zap.Int("copiedVerbatim", copied),
		zap.Any("prunedMocks", prunedMocks),
		zap.Bool("prunedMocksTruncated", prunedCount > len(prunedMocks)),
		zap.Time("pruneBefore", pruneBefore))

	return nil
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
	// writer via the inline init in insertMockGob.
	if err := ys.Close(); err != nil {
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

	rw, err := newGobFileRewriter(gobPath)
	if err != nil {
		return err
	}
	defer rw.discard()

	// See the YAML path above for why per-mock collection is gated on
	// debug level — the gob path is the one most likely to hit
	// ~10^5-mock test sets, so skipping the allocation when the log
	// is a no-op matters more here.
	debugEnabled := ys.Logger.Core().Enabled(zap.DebugLevel)
	total, kept, prunedCount := 0, 0, 0
	var prunedMocks []prunedMockInfo
	if debugEnabled {
		prunedMocks = make([]prunedMockInfo, 0, maxPrunedMocksLogged)
	}
	err = forEachGobMock(gobPath, func(mock *models.Mock) error {
		// The gob stream has no per-line reader watching ctx, so a very
		// large set checks it every ctxCheckEvery mocks.
		if err := ctxErrEvery(ctx, total); err != nil {
			return err
		}
		total++
		if pruneKeeps(mock, mockNames, pruneBefore, startupCutoffTime) {
			kept++
			return rw.write(mock)
		}
		prunedCount++
		if debugEnabled && len(prunedMocks) < maxPrunedMocksLogged {
			prunedMocks = append(prunedMocks, prunedMockInfo{
				Name:     mock.Name,
				Kind:     string(mock.Kind),
				Metadata: mock.Spec.Metadata,
			})
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			utils.LogError(ys.Logger, err, "failed to prune gob mocks", zap.String("path", gobPath))
		}
		return err
	}

	if err := rw.commit(); err != nil {
		return err
	}

	ys.Logger.Debug("pruned mocks successfully (gob)",
		zap.String("testSetID", testSetID),
		zap.Int("total", total),
		zap.Int("kept", kept),
		zap.Int("pruned", prunedCount),
		zap.Any("prunedMocks", prunedMocks),
		zap.Bool("prunedMocksTruncated", prunedCount > len(prunedMocks)),
		zap.Time("pruneBefore", pruneBefore))
	return nil
}

// PersistMockNoise merges learned request-body noise (MockState.ReqBodyNoise,
// detected under --schema-noise-detection) into the on-disk mocks WITHOUT
// pruning anything. This is the persistence path when mock pruning
// (--remove-unused-mocks) is not enabled — previously the learned noise rode
// only inside UpdateMocks, so running detection without pruning silently
// discarded everything that was learned at process exit.
func (ys *MockYaml) PersistMockNoise(ctx context.Context, testSetID string, mockStates map[string]models.MockState) error {
	// Only mocks that actually carry learned noise matter.
	withNoise := make(map[string]map[string][]string)
	for name, st := range mockStates {
		if len(st.ReqBodyNoise) > 0 {
			withNoise[name] = st.ReqBodyNoise
		}
	}
	if len(withNoise) == 0 {
		return nil
	}

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
		noise, ok := withNoise[mock.Name]
		if !ok {
			return false
		}
		merged := mergeReqBodyNoise(mock.Spec.ReqBodyNoise, noise)
		changed := len(merged) != len(mock.Spec.ReqBodyNoise)
		mock.Spec.ReqBodyNoise = merged
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
		// same reasoning as updateMocksGob.
		if err := ys.Close(); err != nil {
			utils.LogError(ys.Logger, err, "failed to quiesce async gob writer before noise persistence", zap.String("path", gobPath))
			return err
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
			return err
		}
		if !changes {
			return nil
		}
		rw, err := newGobFileRewriter(gobPath)
		if err != nil {
			return err
		}
		defer rw.discard()
		total := 0
		err = forEachGobMock(gobPath, func(mock *models.Mock) error {
			if err := ctxErrEvery(ctx, total); err != nil {
				return err
			}
			total++
			merge(mock)
			return rw.write(mock)
		})
		if err != nil {
			return err
		}
		if err := rw.commit(); err != nil {
			return err
		}
		logPersisted(total)
		return nil
	}

	existsAny, detectedFormat, err := yaml.FileExistsAny(ctx, ys.Logger, path, mockFileName, ys.Format)
	if err != nil {
		return err
	}
	if !existsAny {
		return nil
	}

	// eachMock streams the file's mocks to fn, and the documents a rewrite
	// does not re-encode (see fileDoc.verbatim) to onRaw when that is set,
	// closing the file before it returns.
	eachMock := func(fn func(*models.Mock) error, onRaw func(doc fileDoc) error) error {
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
			if doc.verbatim {
				if onRaw != nil {
					if err := onRaw(doc); err != nil {
						return err
					}
				}
				continue
			}
			for _, mock := range doc.mocks {
				if err := fn(mock); err != nil {
					return err
				}
			}
		}
	}

	changes := false
	err = eachMock(func(mock *models.Mock) error {
		if merge(mock) {
			changes = true
			return errStopScan
		}
		return nil
	}, nil)
	if err != nil && !errors.Is(err, errStopScan) {
		return err
	}
	if !changes {
		return nil
	}

	rw := newMockFileRewriter(ys.Logger, path, mockFileName, detectedFormat)
	defer rw.discard()
	total := 0
	if err := eachMock(func(mock *models.Mock) error {
		total++
		merge(mock)
		return rw.write(mock)
	}, rw.copyDoc); err != nil {
		return err
	}
	if err := rw.commit(); err != nil {
		return err
	}
	logPersisted(total)
	return nil
}

func (ys *MockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error {
	mock.Name = fmt.Sprint("mock-", ys.getNextID())
	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}
	// gob is the binary record-time format (async writer, ~28% CPU win
	// over yaml). When selected it is mutually exclusive with yaml/json,
	// so it gets to short-circuit before the format-detection block.
	if ys.useGobFormat() {
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

	// Resolve the effective mock-file format: if a mocks file in either
	// format already exists, append to THAT file in ITS format (don't create
	// a parallel file in the other format). Only when no mocks file exists
	// yet do we use the configured StorageFormat for a fresh file.
	effFormat := ys.Format
	if existsAny, detected, statErr := yaml.FileExistsAny(ctx, ys.Logger, mockPath, mockFileName, ys.Format); statErr == nil && existsAny {
		effFormat = detected
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

// appendMock appends mock's document to the mocks file in effFormat, under the
// file's lock. The document is made in buf, and buf is what is written.
func (ys *MockYaml) appendMock(ctx context.Context, mock *models.Mock, mockPath, mockFileName string, effFormat yaml.Format, buf *bytes.Buffer) error {
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
	defer file.Close()

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
// goroutine owns the open file + encoder; parsers never block on
// disk. Queue-full falls back to synchronous write so mocks are never
// dropped — tracked via gobOverflows for observability.
//
// The writer-alive check and the channel send run under the same
// lifecycle lock Close uses. This makes the "is the writer still
// accepting jobs?" invariant atomic across the send: Close cannot
// transition from running=true to stopClosed in between our check
// and our send, so any job we enqueue is guaranteed to be drained
// by the current writer (or by its drainAndClose on the way out).
func (ys *MockYaml) insertMockGob(ctx context.Context, mock *models.Mock, mockPath, mockFileName string) error {
	ys.gobLifecycleMu.Lock()
	if ys.gobStopClosed {
		ys.gobLifecycleMu.Unlock()
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
	select {
	case ys.gobQueue <- job:
		ys.gobLifecycleMu.Unlock()
		return nil
	case <-ctx.Done():
		ys.gobLifecycleMu.Unlock()
		return ctx.Err()
	default:
		ys.gobOverflows.Add(1)
		// Keep the lifecycle lock held across the sync fallback. If we
		// released it here, a concurrent Close() could flush+close the
		// writer (setting gobFile=nil), and then gobWriteSync's
		// gobWriteOne would call gobReopenLocked — which TRUNCATES
		// the gob file with O_TRUNC — destroying everything written
		// earlier in the session. Serializing the sync fallback
		// against Close() is the cost of the "no dropped mocks"
		// guarantee on the overflow path; it only kicks in when the
		// 4096-slot queue is already full, which is already a
		// degraded-throughput mode.
		err := ys.gobWriteSync(ctx, mock, mockPath, mockFileName)
		ys.gobLifecycleMu.Unlock()
		return err
	}
}

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

func (ys *MockYaml) gobWriteOne(job gobWriteJob) error {
	ys.gobMu.Lock()
	defer ys.gobMu.Unlock()
	want := filepath.Join(job.testSetPath, job.filename+".gob")
	if ys.gobFilePath != want || ys.gobFile == nil {
		if err := ys.gobReopenLocked(job.testSetPath, job.filename); err != nil {
			return err
		}
	}
	return ys.gobEnc.Encode(job.mock)
}

func (ys *MockYaml) gobReopenLocked(mockPath, mockFileName string) error {
	if ys.gobFile != nil {
		_ = ys.gobBufw.Flush()
		_ = ys.gobFile.Close()
		ys.gobFile = nil
		ys.gobBufw = nil
		ys.gobEnc = nil
	}
	if err := os.MkdirAll(mockPath, 0o777); err != nil {
		return fmt.Errorf("mkdir mock dir: %w", err)
	}
	filePath := filepath.Join(mockPath, mockFileName+".gob")
	// Truncate on every open. A gob stream's type table lives in the
	// encoder; reusing a file across multiple encoder sessions (e.g.
	// re-record cycles, or a switch-back to an earlier test-set's
	// file) would embed a second type table mid-file and the reader's
	// single gob.Decoder would fail with "duplicate type" / garbage.
	// Each gob session therefore owns its file exclusively: the first
	// write truncates any prior content and the file carries exactly
	// one continuous gob stream until Close. Callers that need
	// append-like semantics must use yaml (the default format).
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return fmt.Errorf("open gob mock file for overwrite: %w", err)
	}
	ys.gobFile = f
	// 256 KB buffer holds dozens of mocks before a syscall; bufio
	// autoflushes at fill. Shutdown drains explicitly.
	ys.gobBufw = bufio.NewWriterSize(f, 256*1024)
	if _, werr := ys.gobBufw.WriteString(gobMockMagic); werr != nil {
		_ = f.Close()
		ys.gobFile = nil
		ys.gobBufw = nil
		return fmt.Errorf("write gob magic: %w", werr)
	}
	ys.gobEnc = gob.NewEncoder(ys.gobBufw)
	ys.gobFilePath = filePath
	return nil
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
	var flushErr, closeErr error
	if ys.gobBufw != nil {
		flushErr = ys.gobBufw.Flush()
	}
	if ys.gobFile != nil {
		closeErr = ys.gobFile.Close()
	}
	ys.gobFile = nil
	ys.gobBufw = nil
	ys.gobEnc = nil
	combined := errors.Join(flushErr, closeErr)
	// Join with any encode/write errors that gobWriterLoop or
	// drainAndClose already accumulated, rather than overwriting
	// them. Otherwise a successful final flush after earlier drops
	// would mask the real mid-session failures and Close() would
	// return nil on a partially-lost session.
	ys.gobFlushErr = errors.Join(ys.gobFlushErr, combined)
	return ys.gobFlushErr
}

// gobWriteSync is the sync fallback when the async queue is full.
// Reuses the async writer's open file + encoder under the mutex so
// the type-table in the running gob stream stays consistent — the
// reader uses a single gob.Decoder for the whole file, and creating
// a fresh encoder here would emit a second type-table that the
// / reader cannot resume across.
func (ys *MockYaml) gobWriteSync(ctx context.Context, mock *models.Mock, mockPath, mockFileName string) error {
	ys.gobMu.Lock()
	defer ys.gobMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	want := filepath.Join(mockPath, mockFileName+".gob")
	if ys.gobFilePath != want || ys.gobFile == nil {
		if err := ys.gobReopenLocked(mockPath, mockFileName); err != nil {
			return err
		}
	}
	if err := ys.gobEnc.Encode(mock); err != nil {
		return fmt.Errorf("failed to encode mock gob: %w", err)
	}
	// Flush immediately so the "sync" semantics hold — by the time
	// this returns, bytes are in the OS buffer, not just the bufio.
	return ys.gobBufw.Flush()
}

// Close drains the async gob writer and flushes the file. Safe to
// call multiple times, and safe to call between record sessions —
// the writer goroutine exits after flushing, and the next InsertMock
// starts a fresh goroutine via its own inline init (see
// insertMockGob). This is what makes re-record cycles (multiple
// Recorder.Start on the same mockDB instance) work without dropping
// mocks.
func (ys *MockYaml) Close() error {
	// Hold the lifecycle lock for the entire teardown. While we wait
	// for the writer goroutine to drain, no concurrent InsertMock
	// can start a new writer — ys.gobRunning stays true and
	// ys.gobQueue / ys.gobStop / ys.gobDone cannot be reassigned out
	// from under the draining goroutine. A second concurrent Close()
	// blocks on this same lock and observes gobRunning=false after
	// the first Close completes, so close(gobStop) is never called
	// twice.
	ys.gobLifecycleMu.Lock()
	defer ys.gobLifecycleMu.Unlock()
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
	select {
	case <-ys.gobDone:
	case <-time.After(5 * time.Second):
		// Leave gobRunning=true + gobStopClosed=true so a retry of
		// Close enters this function, skips the already-closed stop,
		// and just waits on gobDone again.
		return fmt.Errorf("timed out waiting for gob writer to flush")
	}
	ys.gobRunning = false
	ys.gobStopClosed = false
	// Operator visibility: if the async queue filled up during the
	// session and the sync fallback fired, report the count so disk
	// stalls / undersized queues are caught at post-run review
	// instead of requiring the user to notice slower rps.
	// Swap the overflow counter to zero atomically so re-record cycles
	// (next Start on the same MockYaml) don't count this session's
	// overflows again on their own Close.
	if overflows := ys.gobOverflows.Swap(0); overflows > 0 {
		if ys.Logger != nil {
			ys.Logger.Info("gob mock writer: synchronous fallback fired during session (queue was full)",
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
	if flushErr != nil {
		return fmt.Errorf("gob writer flush/close during shutdown: %w", flushErr)
	}
	return nil
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
}

// pruned reports whether the mapping prune drops the named mock: it is mapped
// to a specific test and this run does not need it.
func (r *mockRouter) pruned(name string) bool {
	_, isMappedToSpecificTest := r.mocksThatHaveMappings[name]
	_, isNeededForCurrentRun := r.mocksWeNeed[name]
	return isMappedToSpecificTest && !isNeededForCurrentRun
}

// route classifies one decoded mock. fromGob marks a mock read from mocks.gob,
// whose per-test pool keeps the PostgresV2 dual-pool quirk.
func (r *mockRouter) route(mock *models.Mock, fromGob bool) {
	pruned := r.pruned(mock.Name)
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
			if !r.pruned(m.Name) {
				out.Unfiltered = append(out.Unfiltered, m)
			}
		}
	}
}

// readMockPools reads and decodes the test set's mock file once and builds the
// wanted pools from it:
//
//   - Filtered (poolPerTest): the per-test mocks, in file order. From a gob
//     file, PostgresV2 mocks too, through the lax window filter.
//   - Unfiltered (poolSession): the session and connection mocks, through the
//     lax FilterConfigMocks.
//   - AllSession (poolAllSession): Unfiltered without the mapping prune.
//   - AllPerTest (poolAllPerTest): the per-test candidates Filtered is drawn
//     from, before the mapping prune and the window filter.
//
// The mapping prune drops a mock that is mapped to a specific test this run
// does not need; Filtered and Unfiltered apply it.
func (ys *MockYaml) readMockPools(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool, want mockPools) (models.TestSetMocks, error) {
	var out models.TestSetMocks
	r := &mockRouter{
		want:                  want,
		mocksThatHaveMappings: mocksThatHaveMappings,
		mocksWeNeed:           mocksWeNeed,
		perTest:               make([]*models.Mock, 0),
		session:               make([]*models.Mock, 0),
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
		mocks, cut, err := readGobMocksCut(gobPath)
		if err != nil {
			return models.TestSetMocks{}, err
		}
		logger := ys.Logger.With(zap.String("mock_file", gobPath))
		if cut {
			logger.Warn("the mock file ends part-way through its last mock, probably because a write was interrupted (the recorder stopped, or the disk filled), and that mock was not read",
				zap.String("next_step", "re-record the test set"))
		}
		connFailures := 0
		for _, mock := range mocks {
			if mock.Kind == models.ConnectionFailure {
				// gob has no per-kind decoder: a connection failure is
				// validated here, as DecodeMocks and DecodeMocksJSON validate
				// theirs.
				if !connFailureSupported(mock, logger, false) {
					addSkipped(&out, mock.Name, mock.Kind)
					continue
				}
				connFailures++
			}
			r.route(mock, true)
		}
		warnConnFailuresNotReplayed(logger, testSetID, connFailures)
		if want&poolPerTest != 0 {
			out.Filtered = pkg.FilterTcsMocks(ctx, ys.Logger, r.perTest, afterTime, beforeTime, false)
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
		msg := "failed to read the mocks from config file"
		if want&poolPerTest != 0 {
			msg = "failed to read the mocks from file"
		}
		utils.LogError(ys.Logger, err, msg, zap.String("session", filepath.Base(path)))
		return models.TestSetMocks{}, err
	}
	defer reader.Close()

	// When the mocks file is JSON we go through ReadNextDocJSON +
	// DecodeMocksJSON, skipping the yaml.Node bridge entirely. YAML files
	// keep the original path for full backwards compatibility with
	// existing recordings.
	readerIsJSON := reader.Format() == yaml.FormatJSON

	// The decoders' skip ERRORs name the mock, and mock names repeat in every
	// test set, so they say which file.
	logger := ys.Logger.With(zap.String("mock_file", reader.Path()))
	hasContent := false
	connFailures := 0
	for {
		doc, readErr, decodeErr := nextDoc(reader, logger, false)
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return models.TestSetMocks{}, fmt.Errorf("failed to decode the file documents. error: %v", readErr.Error())
		}
		hasContent = true
		if decodeErr != nil {
			msg := "failed to decode the config mocks from doc"
			if readerIsJSON {
				msg = "failed to decode the config mocks from json doc"
			}
			utils.LogError(ys.Logger, decodeErr, msg, zap.String("session", filepath.Base(path)))
			return models.TestSetMocks{}, fmt.Errorf("failed to decode the mocks in %s: %w", reader.Path(), decodeErr)
		}
		if len(doc.mocks) == 0 {
			addSkipped(&out, doc.name, doc.kind)
		}
		for _, mock := range doc.mocks {
			if mock.Kind == models.ConnectionFailure {
				connFailures++
			}
			r.route(mock, false)
		}
	}
	warnConnFailuresNotReplayed(logger, testSetID, connFailures)

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
	_ = ctx
	for _, id := range []string{stagingID, targetID} {
		if err := pathsafe.ValidateSingleSegment(id, false); err != nil {
			return fmt.Errorf("rejecting PromoteStagedSet: testSetID %q must be a non-empty single-segment name (no separators, no drive/volume prefix, not '.' or '..'): %w", id, err)
		}
	}

	// Finalize any open (gob) writer so staging's file is complete on disk before
	// it is moved. No-op for yaml/json, whose writes are synchronous. A flush
	// error means the staged recording is incomplete — do not promote it.
	if err := ys.Close(); err != nil {
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
	_ = ctx
	if err := pathsafe.ValidateSingleSegment(stagingID, false); err != nil {
		return fmt.Errorf("rejecting DiscardStagedSet: testSetID %q must be a non-empty single-segment name (no separators, no drive/volume prefix, not '.' or '..'): %w", stagingID, err)
	}
	// Release any open (gob) writer handle before removing the directory.
	if err := ys.Close(); err != nil {
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
