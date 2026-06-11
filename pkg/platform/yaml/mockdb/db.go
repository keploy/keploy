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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
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

type MockYaml struct {
	MockPath  string
	MockName  string
	Logger    *zap.Logger
	idCounter int64
	Format    yaml.Format

	// Synchronous mock writer. The recording consumer calls InsertMock
	// (encode + append to the file's in-memory buffer, NO flush) and drives
	// batching itself: it flushes via FlushMocks when its source channel
	// momentarily empties, then Close does the final flush + close. A single
	// consumer goroutine owns this path, so writes are serialized — there is
	// no background goroutine and no intermediate queue. asyncMu guards the
	// file-state fields below against the (rare) overlap of a write and the
	// final Close.
	asyncNeedsYamlSep bool
	asyncMu           sync.Mutex
	asyncFilePath     string
	asyncFile         *os.File
	asyncBufw         *bufio.Writer
	asyncGobEnc       *gob.Encoder
	// asyncFlushErr holds the terminal flush/close error from
	// asyncFlushAndClose so that Close() can surface it to its caller
	// (and therefore to the Recorder.Start deferred-cleanup logger)
	// instead of silently losing the tail of mocks.gob on a disk-full
	// or permission-change shutdown.
	asyncFlushErr error
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

type asyncWriteJob struct {
	mock *models.Mock
	// testSetPath is the full directory path — "<MockPath>/<testSetID>"
	// — not just the test-set identifier. Kept as a full path because
	// asyncReopenLocked mkdir's it and reuses it in filepath.Join.
	testSetPath string
	filename    string
	effFormat   yaml.Format
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

// writeMocksAtomically writes the given mocks to <path>/<fileName>.<ext> in
// the specified `format`. Callers pass the format actually observed on disk
// (via resolveEffectiveFormat) so a prune/rewrite never silently migrates
// an existing mocks.yaml into mocks.json or vice versa.
func (ys *MockYaml) writeMocksAtomically(path, fileName string, mocks []*models.Mock, format yaml.Format) error {
	targetPath := filepath.Join(path, fileName+"."+format.FileExtension())
	if len(mocks) == 0 {
		if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}

	if err := os.MkdirAll(path, 0o777); err != nil {
		return err
	}

	tmpFile, err := os.CreateTemp(path, fileName+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	writer := bufio.NewWriter(tmpFile)

	if format == yaml.FormatJSON {
		// NDJSON: one JSON object per line. The JSON write path is now
		// fully yaml-free — EncodeMockJSON covers every kind that keploy
		// records (HTTP, DNS, Generic, Redis, Kafka, HTTP/2, gRPC,
		// PostgresV2, MySQL, Mongo). An unexpected kind is treated as an
		// error rather than silently falling back through yaml.Node.
		jsonEnc := json.NewEncoder(writer)
		for _, mock := range mocks {
			jsonDoc, handled, err := EncodeMockJSON(mock, ys.Logger)
			if err != nil {
				_ = tmpFile.Close()
				return err
			}
			if !handled {
				_ = tmpFile.Close()
				return fmt.Errorf("mockdb: unsupported mock kind %q for JSON format", mock.Kind)
			}
			if err := jsonEnc.Encode(jsonDoc); err != nil {
				_ = tmpFile.Close()
				return err
			}
			// json.Encoder already appends a trailing newline.
		}
	} else {
		if version := utils.GetVersionAsComment(); version != "" {
			if _, err := writer.WriteString(version); err != nil {
				_ = tmpFile.Close()
				return err
			}
		}

		for i, mock := range mocks {
			if i > 0 {
				if _, err := writer.WriteString("---\n"); err != nil {
					_ = tmpFile.Close()
					return err
				}
			}
			mockYaml, err := EncodeMock(mock, ys.Logger)
			if err != nil {
				_ = tmpFile.Close()
				return err
			}
			data, err := yamlLib.Marshal(&mockYaml)
			if err != nil {
				_ = tmpFile.Close()
				return err
			}
			if _, err := writer.Write(data); err != nil {
				_ = tmpFile.Close()
				return err
			}
		}
	}

	if err := writer.Flush(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	fileMode, err := resolveMockFileMode(targetPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, fileMode); err != nil {
		return err
	}

	if err := replaceFile(tmpPath, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
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

func replaceFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else {
		renameErr := err
		if _, statErr := os.Stat(dst); statErr != nil {
			if os.IsNotExist(statErr) {
				return renameErr
			}
			return fmt.Errorf("failed to stat target after rename error: %v; initial rename error: %w", statErr, renameErr)
		}

		if removeErr := os.Remove(dst); removeErr != nil {
			return fmt.Errorf("failed to remove target for replace: %v; initial rename error: %w", removeErr, renameErr)
		}

		if retryErr := os.Rename(src, dst); retryErr != nil {
			return fmt.Errorf("failed to replace file after removing existing target: %v; initial rename error: %w", retryErr, renameErr)
		}
	}
	return nil
}

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
func (ys *MockYaml) UpdateMocks(ctx context.Context, testSetID string, mockNames map[string]models.MockState, pruneBefore time.Time, firstTestCaseTime time.Time) error {
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
		return ys.updateMocksGob(ctx, testSetID, gobPath, mockNames, pruneBefore, firstTestCaseTime)
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

	// On a JSON mocks file, decode through the json.RawMessage path so
	// pruning doesn't allocate yaml.Node trees for every mock it reads.
	var mocks []*models.Mock
	if reader.Format() == yaml.FormatJSON {
		var jsonDocs []*yaml.NetworkTrafficDocJSON
		for {
			jd, err := reader.ReadNextDocJSON()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the file documents", zap.String("at_path", filepath.Join(path, mockFileName+ext)))
				return fmt.Errorf("failed to decode the file documents. error: %v", err.Error())
			}
			jsonDocs = append(jsonDocs, jd)
		}
		m, err := DecodeMocksJSON(jsonDocs, ys.Logger)
		if err != nil {
			return err
		}
		mocks = m
	} else {
		var mockYamls []*yaml.NetworkTrafficDoc
		for {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the file documents", zap.String("at_path", filepath.Join(path, mockFileName+ext)))
				return fmt.Errorf("failed to decode the file documents. error: %v", err.Error())
			}
			mockYamls = append(mockYamls, doc)
		}
		m, err := DecodeMocks(mockYamls, ys.Logger)
		if err != nil {
			return err
		}
		mocks = m
	}

	// Only build the per-mock log slice when debug logging is enabled
	// — on large test sets the allocation and reflection cost of
	// collecting names/kinds/metadata is a significant overhead if
	// the emitted log will be dropped by the logger anyway.
	debugEnabled := ys.Logger.Core().Enabled(zap.DebugLevel)
	newMocks := make([]*models.Mock, 0, len(mocks))
	prunedCount := 0
	var prunedMocks []prunedMockInfo
	if debugEnabled {
		prunedMocks = make([]prunedMockInfo, 0, maxPrunedMocksLogged)
	}
	for _, mock := range mocks {
		if mock.Spec.Metadata["type"] == "config" {
			newMocks = append(newMocks, mock)
			continue
		}
		if st, ok := mockNames[mock.Name]; ok {
			// Persist any request-body noise detected during schema-based
			// auto-replay matching (config.Test.SchemaNoiseDetection) onto the
			// disk-read mock before it is re-written.
			if len(st.ReqBodyNoise) > 0 && mock.Kind == models.Kind(models.HTTP) && mock.Spec.HTTPReq != nil {
				mock.Spec.HTTPReq.ReqBodyNoise = mergeReqBodyNoise(mock.Spec.HTTPReq.ReqBodyNoise, st.ReqBodyNoise)
			}
			newMocks = append(newMocks, mock)
			continue
		}
		// Preserve mocks written after replay start.
		if !mock.Spec.ReqTimestampMock.IsZero() && mock.Spec.ReqTimestampMock.After(pruneBefore) {
			newMocks = append(newMocks, mock)
			continue
		}
		// Keep startup/init mocks: mocks recorded before the first test case
		// are connection-level or app-init traffic (DNS, TLS, DB handshake,
		// config fetch, etc.) that only fires once at app startup. In multi-
		// test-set replays without app restart, these won't be consumed in
		// later test-sets but are still needed for future replays.
		if !firstTestCaseTime.IsZero() && !mock.Spec.ReqTimestampMock.IsZero() &&
			mock.Spec.ReqTimestampMock.Before(firstTestCaseTime) {
			newMocks = append(newMocks, mock)
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

	// Write back in the same format we read — preserve existing file's format.
	if err := ys.writeMocksAtomically(path, mockFileName, newMocks, detectedFormat); err != nil {
		return err
	}

	ys.Logger.Debug("pruned mocks successfully",
		zap.String("testSetID", testSetID),
		zap.Int("total", len(mocks)),
		zap.Int("kept", len(newMocks)),
		zap.Int("pruned", prunedCount),
		zap.Any("prunedMocks", prunedMocks),
		zap.Bool("prunedMocksTruncated", prunedCount > len(prunedMocks)),
		zap.Time("pruneBefore", pruneBefore))

	return nil
}

// updateMocksGob implements RemoveUnusedMocks for mocks.gob. The
// filter decision matches the YAML path exactly (keep config mocks,
// mocks named in mockNames, post-replay mocks, and pre-first-test
// startup mocks — prune everything else). The rewrite rules are
// different because gob doesn't support append: we read the whole
// file, filter, and atomically rewrite a fresh single-encoder
// stream with the magic header. An existing gob writer on this
// MockYaml must be quiesced before we touch the file so a
// concurrent InsertMock doesn't race the truncate-and-rewrite.
func (ys *MockYaml) updateMocksGob(ctx context.Context, testSetID, gobPath string, mockNames map[string]models.MockState, pruneBefore, firstTestCaseTime time.Time) error {
	ys.Logger.Debug("pruning unused mocks (gob)",
		zap.Any("consumedMocks", mockNames),
		zap.String("testSetID", testSetID),
		zap.String("path", gobPath),
		zap.Time("pruneBefore", pruneBefore))

	// Quiesce any in-flight async writer on this MockYaml before we
	// rewrite the gob file. An active writer holds ys.asyncFile /
	// ys.asyncBufw / ys.asyncGobEnc; rewriting the file out from under it
	// would corrupt the next Encode. Close drains the queue and
	// resets lifecycle state; the next InsertMock restarts a fresh
	// writer via the inline init in insertMockGob.
	if err := ys.Close(); err != nil {
		utils.LogError(ys.Logger, err, "failed to quiesce async gob writer before pruning; check disk space and writer state", zap.String("path", gobPath))
		return err
	}

	mocks, err := readGobMocks(gobPath)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read gob mocks for pruning", zap.String("path", gobPath))
		return err
	}

	// Bail early if the caller has already cancelled before we touch
	// the tmp file. Big test-sets have ~10^5 mocks and the filter+
	// encode loops below can run for seconds; a cancelled recorder
	// should not sit here rewriting a file whose result nobody is
	// waiting for.
	if err := ctx.Err(); err != nil {
		return err
	}

	// See the YAML path above for why per-mock collection is gated on
	// debug level — the gob path is the one most likely to hit
	// ~10^5-mock test sets, so skipping the allocation when the log
	// is a no-op matters more here.
	debugEnabled := ys.Logger.Core().Enabled(zap.DebugLevel)
	newMocks := make([]*models.Mock, 0, len(mocks))
	prunedCount := 0
	var prunedMocks []prunedMockInfo
	if debugEnabled {
		prunedMocks = make([]prunedMockInfo, 0, maxPrunedMocksLogged)
	}
	for i, mock := range mocks {
		// Check ctx every 1024 entries so a very large filter loop
		// still responds to cancellation without paying the syscall
		// cost on every iteration.
		if i&0x3ff == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if mock.Spec.Metadata["type"] == "config" {
			newMocks = append(newMocks, mock)
			continue
		}
		if st, ok := mockNames[mock.Name]; ok {
			// Persist any request-body noise detected during schema-based
			// auto-replay matching (config.Test.SchemaNoiseDetection) onto the
			// disk-read mock before it is re-written.
			if len(st.ReqBodyNoise) > 0 && mock.Kind == models.Kind(models.HTTP) && mock.Spec.HTTPReq != nil {
				mock.Spec.HTTPReq.ReqBodyNoise = mergeReqBodyNoise(mock.Spec.HTTPReq.ReqBodyNoise, st.ReqBodyNoise)
			}
			newMocks = append(newMocks, mock)
			continue
		}
		if !mock.Spec.ReqTimestampMock.IsZero() && mock.Spec.ReqTimestampMock.After(pruneBefore) {
			newMocks = append(newMocks, mock)
			continue
		}
		if !firstTestCaseTime.IsZero() && !mock.Spec.ReqTimestampMock.IsZero() &&
			mock.Spec.ReqTimestampMock.Before(firstTestCaseTime) {
			newMocks = append(newMocks, mock)
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

	// Atomic rewrite: write to a sibling tmp file under the same
	// directory, then rename over gobPath. os.Rename on the same
	// filesystem is atomic, so a concurrent reader either sees the
	// full old file or the full new one.
	//
	// Preserve the existing file's permissions across the rewrite.
	// os.CreateTemp creates its file 0600, so without the chmod below
	// pruning would quietly narrow mocks.gob from whatever mode the
	// record writer produced (typically 0644 via umask 0022) down to
	// owner-only, which breaks replay for any other user/process on
	// the box. Stat before CreateTemp: if the source file is gone,
	// fall back to the same mode the gob writer uses when it opens
	// mocks.gob fresh (0644) so we do not introduce a new
	// mode-inheritance path.
	dir := filepath.Dir(gobPath)
	base := filepath.Base(gobPath)
	var originalMode os.FileMode = 0644
	if info, statErr := os.Stat(gobPath); statErr == nil {
		originalMode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, base+".prune.*.tmp")
	if err != nil {
		return fmt.Errorf("create gob prune tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	// Match mocks.gob's permissions on the tmp file before the
	// rename. Must happen before any concurrent reader observes the
	// renamed file.
	if err := os.Chmod(tmpPath, originalMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod gob prune tmp to %o: %w", originalMode, err)
	}

	bw := bufio.NewWriterSize(tmp, 256*1024)
	if _, err := bw.WriteString(gobMockMagic); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write gob magic to prune tmp: %w", err)
	}
	enc := gob.NewEncoder(bw)
	for i, mock := range newMocks {
		// Same cancellation cadence as the filter loop above — the
		// encode pass is the expensive one (reflect-heavy) so an
		// op at cancellation is worth the early exit cost.
		if i&0x3ff == 0 {
			if err := ctx.Err(); err != nil {
				_ = tmp.Close()
				return err
			}
		}
		if err := enc.Encode(mock); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("encode mock during gob prune: %w", err)
		}
	}
	if err := bw.Flush(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("flush gob prune tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync gob prune tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close gob prune tmp: %w", err)
	}
	if err := os.Rename(tmpPath, gobPath); err != nil {
		return fmt.Errorf("rename gob prune tmp over %s: %w", gobPath, err)
	}
	cleanup = false

	ys.Logger.Debug("pruned mocks successfully (gob)",
		zap.String("testSetID", testSetID),
		zap.Int("total", len(mocks)),
		zap.Int("kept", len(newMocks)),
		zap.Int("pruned", prunedCount),
		zap.Any("prunedMocks", prunedMocks),
		zap.Bool("prunedMocksTruncated", prunedCount > len(prunedMocks)),
		zap.Time("pruneBefore", pruneBefore))
	return nil
}

func (ys *MockYaml) encodeMockData(job asyncWriteJob) ([]byte, error) {
	if job.effFormat == yaml.FormatJSON {
		jsonDoc, handled, err := EncodeMockJSON(job.mock, ys.Logger)
		if err != nil {
			return nil, fmt.Errorf("failed to encode mock (json): %w", err)
		}
		if !handled {
			return nil, fmt.Errorf("mockdb: unsupported mock kind %q for JSON format", job.mock.Kind)
		}
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(jsonDoc); err != nil {
			return nil, fmt.Errorf("failed to encode mock json: %w", err)
		}
		return buf.Bytes(), nil
	}

	mockYaml, err := EncodeMock(job.mock, ys.Logger)
	if err != nil {
		return nil, fmt.Errorf("failed to encode mock (yaml): %w", err)
	}
	var buf bytes.Buffer
	encoder := yamlLib.NewEncoder(&buf)
	if err := encoder.Encode(&mockYaml); err != nil {
		_ = encoder.Close()
		return nil, fmt.Errorf("failed to encode mock yaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("failed to close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

func (ys *MockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}
	mock.Name = fmt.Sprint("mock-", ys.getNextID())

	effFormat := ys.Format
	isGob := useGobMockFormat()
	if !isGob {
		if existsAny, detected, statErr := yaml.FileExistsAny(context.Background(), ys.Logger, mockPath, mockFileName, ys.Format); statErr == nil && existsAny {
			effFormat = detected
		}
	}

	// Synchronous write: encode + append the mock to the file's in-memory
	// buffer NOW (asyncWriteOne takes asyncMu and opens/rotates the file as
	// needed). We deliberately do NOT flush here — the recording consumer
	// batches flushes via FlushMocks when its source channel momentarily
	// empties, and Close does the final flush. This replaces the old
	// queue+background-goroutine writer: one consumer goroutine drives the
	// whole write path, so there is no separate buffer and no parallelism.
	job := asyncWriteJob{mock: mock.DeepCopy(), testSetPath: mockPath, filename: mockFileName, effFormat: effFormat}
	return ys.asyncWriteOne(job)
}

// asyncMocksWrittenTotal is a process-cumulative count of mocks successfully
// encoded to the file buffer (W in the single-buffer verification). RTRACE:
// TEMP — remove before merge.
var asyncMocksWrittenTotal atomic.Int64

func (ys *MockYaml) asyncWriteOne(job asyncWriteJob) error {
	ys.asyncMu.Lock()
	defer ys.asyncMu.Unlock()

	var currentBase string
	if ys.asyncFilePath != "" {
		currentBase = filepath.Base(ys.asyncFilePath)
		currentBase = strings.TrimSuffix(currentBase, filepath.Ext(currentBase))
	}

	if ys.asyncFile == nil || filepath.Dir(ys.asyncFilePath) != job.testSetPath || currentBase != job.filename {
		if err := ys.asyncReopenLocked(job.testSetPath, job.filename, job.effFormat); err != nil {
			return err
		}
	}

	isGob := useGobMockFormat()
	if isGob && ys.asyncGobEnc != nil {
		if err := ys.asyncGobEnc.Encode(job.mock); err != nil {
			return err
		}
		asyncMocksWrittenTotal.Add(1) // RTRACE: TEMP single-buffer experiment — remove before merge.
		return nil
	}

	// Encode the mock to its on-disk bytes inline and append to the buffer.
	// Called synchronously by the single recording consumer (via InsertMock),
	// so encoding is serialized — simple and race-free. No flush here; the
	// consumer batches flushes via FlushMocks.
	data, err := ys.encodeMockData(job)
	if err != nil {
		return err
	}

	if job.effFormat == yaml.FormatJSON {
		if _, err := ys.asyncBufw.Write(data); err != nil {
			return err
		}
		if _, err := ys.asyncBufw.WriteString("\n"); err != nil {
			return err
		}
	} else {

		if !ys.asyncNeedsYamlSep {
			if version := utils.GetVersionAsComment(); version != "" {
				if _, err := ys.asyncBufw.WriteString(version); err != nil {
					return fmt.Errorf("failed to write version comment: %w", err)
				}
			}
		} else {
			if _, err := ys.asyncBufw.WriteString("---\n"); err != nil {
				return fmt.Errorf("failed to write document separator: %w", err)
			}
		}
		if _, err := ys.asyncBufw.Write(data); err != nil {
			return err
		}
		ys.asyncNeedsYamlSep = true
	}
	asyncMocksWrittenTotal.Add(1) // RTRACE: TEMP single-buffer experiment — remove before merge.
	return nil
}

func (ys *MockYaml) asyncReopenLocked(mockPath, mockFileName string, effFormat yaml.Format) error {
	if ys.asyncFile != nil {
		_ = ys.asyncBufw.Flush()
		_ = ys.asyncFile.Close()
		ys.asyncFile = nil
		ys.asyncBufw = nil
		ys.asyncGobEnc = nil
	}
	if err := os.MkdirAll(mockPath, 0o777); err != nil {
		return fmt.Errorf("mkdir mock dir: %w", err)
	}

	isGob := useGobMockFormat()

	ext := ".gob"
	if !isGob {
		ext = "." + effFormat.FileExtension()
	}
	filePath := filepath.Join(mockPath, mockFileName+ext)

	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !isGob {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(filePath, flags, 0o666)
	if err != nil {
		return fmt.Errorf("open mock file: %w", err)
	}
	ys.asyncFile = f
	ys.asyncBufw = bufio.NewWriterSize(f, 256*1024)

	if isGob {
		if _, werr := ys.asyncBufw.WriteString(gobMockMagic); werr != nil {
			_ = f.Close()
			ys.asyncFile = nil
			ys.asyncBufw = nil
			return fmt.Errorf("write gob magic: %w", werr)
		}
		ys.asyncGobEnc = gob.NewEncoder(ys.asyncBufw)
	} else {
		info, err := ys.asyncFile.Stat()
		ys.asyncNeedsYamlSep = err == nil && info.Size() > 0
	}
	ys.asyncFilePath = filePath
	return nil
}

func (ys *MockYaml) asyncFlushAndClose() error {
	ys.asyncMu.Lock()
	defer ys.asyncMu.Unlock()
	var flushErr, closeErr error
	if ys.asyncBufw != nil {
		flushErr = ys.asyncBufw.Flush()
	}
	if ys.asyncFile != nil {
		closeErr = ys.asyncFile.Close()
	}
	ys.asyncFile = nil
	ys.asyncBufw = nil
	ys.asyncGobEnc = nil
	combined := errors.Join(flushErr, closeErr)
	ys.asyncFlushErr = errors.Join(ys.asyncFlushErr, combined)
	return ys.asyncFlushErr
}

// FlushMocks pushes everything written so far from the in-memory file buffer
// to physical disk. The recording consumer calls it to BATCH writes: it keeps
// calling InsertMock (which only buffers) and then FlushMocks once its source
// channel momentarily empties — so one disk flush covers a whole batch instead
// of one flush per mock. Safe to call concurrently with InsertMock (asyncMu).
func (ys *MockYaml) FlushMocks() error {
	ys.asyncMu.Lock()
	defer ys.asyncMu.Unlock()
	if ys.asyncBufw != nil {
		return ys.asyncBufw.Flush()
	}
	return nil
}

// Close does the final flush + close of the mocks file. With the synchronous
// writer there is no background goroutine to wait on — asyncFlushAndClose
// flushes the buffer, closes the file, and resets the file-state fields so a
// subsequent recording session (re-record) reopens cleanly on the next
// InsertMock. Idempotent: once the file is closed, asyncBufw/asyncFile are nil
// and a second Close is a no-op flush.
func (ys *MockYaml) Close() error {
	err := ys.asyncFlushAndClose()
	// RTRACE: TEMP single-buffer verification — W (mocks written to disk).
	// Compare with host_decoded (M). Remove before merge.
	fmt.Fprintf(os.Stderr, "RTRACE/mockdb-written: async_mocks_written_total=%d\n", asyncMocksWrittenTotal.Load())
	_ = os.Stderr.Sync()
	if err != nil {
		return fmt.Errorf("mock writer flush/close during shutdown: %w", err)
	}
	return nil
}

// readGobMocks decodes every mock in a mocks.gob file. The async
// writer holds one *gob.Encoder alive for the whole session, so the
// on-disk file is a single continuous gob stream — we mirror that on
// the read side with one *gob.Decoder that keeps the type table live
// across Decode calls. Mid-stream ErrUnexpectedEOF is treated as
// end-of-data (partial write from a crashed writer — we lose the tail
// mock, not the batch).
//
// Constraint: because the encoder session owns the type table, you
// cannot usefully append to an existing mocks.gob from a fresh
// encoder — the new encoder's type table will conflict. Readers that
// need to merge multiple sessions must read each file independently.
func readGobMocks(path string) ([]*models.Mock, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	// Verify the magic header. Files recorded before v1 did not emit
	// a header; we reject them with a clear error rather than decoding
	// a garbled Mock struct. Bump gobMockMagic to v2 when the on-disk
	// format changes in a breaking way.
	magic := make([]byte, len(gobMockMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return nil, fmt.Errorf("read gob mock magic: %w (file may be truncated or not a keploy gob mock)", err)
	}
	if string(magic) != gobMockMagic {
		return nil, fmt.Errorf("gob mock file %s: unrecognized magic %q (want %q) — the file was written by a different keploy version", path, magic, gobMockMagic)
	}
	dec := gob.NewDecoder(br)
	var out []*models.Mock
	for {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return out, nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return out, nil
			}
			return out, fmt.Errorf("decode gob mock: %w", err)
		}
		out = append(out, &m)
	}
}

func (ys *MockYaml) GetFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error) {

	var tcsMocks = make([]*models.Mock, 0)
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
		mocks, err := readGobMocks(gobPath)
		if err != nil {
			return nil, err
		}
		for _, mock := range mocks {
			_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]
			_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
			if isMappedToSpecificTest && !isNeededForCurrentRun {
				continue
			}
			// Unification: lifetime-only routing via DeriveLifetime —
			// the same classifier the YAML path below uses. Aligns the
			// gob and YAML read paths so an explicit per-test tag
			// (metadata["type"] == "mocks") on a kind that isUnfiltered-
			// MockKind lists as implicit-session (HTTP, Postgres, ...)
			// correctly lands in the per-test pool instead of being
			// shunted to unfiltered on-read. Previously the gob path
			// gated on kind + config-tag only, which silently dropped
			// every per-test HTTP mock from the filtered pool even
			// when the recorder had explicitly tagged it per-test.
			//
			// PostgresV2 keeps its dual-pool quirk (present in BOTH
			// filtered and unfiltered) — the YAML path mirrors this
			// via its sibling GetUnFilteredMocks reader.
			mock.DeriveLifetime()
			if mock.TestModeInfo.Lifetime == models.LifetimePerTest || mock.Kind == models.PostgresV2 {
				tcsMocks = append(tcsMocks, mock)
			}
		}
		return pkg.FilterTcsMocks(ctx, ys.Logger, tcsMocks, afterTime, beforeTime, false), nil
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
			filtered := pkg.FilterTcsMocks(ctx, ys.Logger, tcsMocks, afterTime, beforeTime, false)
			return filtered, nil
		}
		utils.LogError(ys.Logger, err, "failed to read the mocks from file", zap.String("session", filepath.Base(path)))
		return nil, err
	}
	defer reader.Close()

	// When the mocks file is JSON we go through ReadNextDocJSON +
	// DecodeMocksJSON, skipping the yaml.Node bridge entirely. YAML files
	// keep the original path for full backwards compatibility with
	// existing recordings.
	readerIsJSON := reader.Format() == yaml.FormatJSON

	hasContent := false
	for {
		var mocks []*models.Mock
		if readerIsJSON {
			jsonDoc, err := reader.ReadNextDocJSON()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the file documents. error: %v", err.Error())
			}
			hasContent = true
			mocks, err = DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{jsonDoc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from json doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}
		} else {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the file documents. error: %v", err.Error())
			}
			hasContent = true
			mocks, err = DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}
		}

		for _, mock := range mocks {
			_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]
			_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
			if isMappedToSpecificTest && !isNeededForCurrentRun {
				continue
			}
			// Unification (Phase 3): resolve the mock's typed Lifetime
			// once via DeriveLifetime — which reads
			// Spec.Metadata["type"] first and falls back to the legacy
			// kind-switch only for pre-tag recordings (logged via
			// LegacyKindFallbackFires). Routing into the per-test
			// (tcsMocks) pool is then purely Lifetime-driven.
			// LifetimePerTest lands here; Session and Connection land
			// in the unfiltered/config pool returned by the sibling
			// GetUnFilteredMocks below.
			mock.DeriveLifetime()
			if mock.TestModeInfo.Lifetime == models.LifetimePerTest {
				tcsMocks = append(tcsMocks, mock)
			}
		}
	}

	if !hasContent {
		utils.LogError(ys.Logger, nil, "failed to read the mocks from file (empty)", zap.String("session", filepath.Base(path)))
		return nil, fmt.Errorf("failed to get mocks, empty file")
	}

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
	// mocksThatHaveMappings) already ran in the per-doc loop above,
	// so what reaches here is the minimal relevant set.
	ys.Logger.Debug("per-test mocks count", zap.Int("count", len(tcsMocks)))
	return tcsMocks, nil
}

func (ys *MockYaml) GetUnFilteredMocks(ctx context.Context, testSetID string, afterTime time.Time, beforeTime time.Time, mocksThatHaveMappings map[string]bool, mocksWeNeed map[string]bool) ([]*models.Mock, error) {

	var configMocks = make([]*models.Mock, 0)

	mockName := "mocks"
	if ys.MockName != "" {
		mockName = ys.MockName
	}

	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockName, ys.Format))
	lock.RLock()
	defer lock.RUnlock()

	// Prefer gob binary format when present (mutually exclusive with the
	// yaml/json text formats — short-circuit before falling through).
	gobPath := filepath.Join(path, mockName+".gob")
	if _, err := os.Stat(gobPath); err == nil {
		mocks, err := readGobMocks(gobPath)
		if err != nil {
			return nil, err
		}
		for _, mock := range mocks {
			_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]
			_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
			if isMappedToSpecificTest && !isNeededForCurrentRun {
				continue
			}
			// Unification: lifetime-only routing via DeriveLifetime —
			// the same classifier the YAML path uses. A mock lands in
			// the session/config pool iff DeriveLifetime classified it
			// as Session or Connection. Untagged mocks of the legacy
			// implicit-session kinds (HTTP, Postgres, MySQL, ...) still
			// resolve to Session via DeriveLifetime's kind-fallback
			// branch, so pre-tag recordings keep replaying byte-for-
			// byte identically. metadata["scope"] is NOT consulted.
			mock.DeriveLifetime()
			if mock.TestModeInfo.Lifetime == models.LifetimeSession ||
				mock.TestModeInfo.Lifetime == models.LifetimeConnection {
				configMocks = append(configMocks, mock)
			}
		}
		return pkg.FilterConfigMocks(ctx, ys.Logger, configMocks, afterTime, beforeTime, false), nil
	}

	// Auto-detect format so config mocks recorded in the other format
	// remain visible to replay.
	reader, err := yaml.NewMockReaderAny(ctx, ys.Logger, path, mockName, ys.Format)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			unfiltered := pkg.FilterConfigMocks(ctx, ys.Logger, configMocks, afterTime, beforeTime, false)
			return unfiltered, nil
		}
		utils.LogError(ys.Logger, err, "failed to read the mocks from config file", zap.String("session", filepath.Base(path)))
		return nil, err
	}
	defer reader.Close()

	readerIsJSON := reader.Format() == yaml.FormatJSON

	for {
		var mocks []*models.Mock
		if readerIsJSON {
			jsonDoc, err := reader.ReadNextDocJSON()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the file documents. error: %v", err.Error())
			}
			mocks, err = DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{jsonDoc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from json doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}
		} else {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the file documents. error: %v", err.Error())
			}
			mocks, err = DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}
		}

		for _, mock := range mocks {
			_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]
			_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
			if isMappedToSpecificTest && !isNeededForCurrentRun {
				continue
			}
			// Unification (Phase 3): Lifetime-only routing. A mock lands
			// in the session/config pool iff DeriveLifetime classified
			// it as Session or Connection. Old kind-switch behaviour is
			// preserved byte-for-byte for pre-tag recordings because
			// DeriveLifetime's compat fallback maps the same kind list
			// to LifetimeSession.
			mock.DeriveLifetime()
			if mock.TestModeInfo.Lifetime == models.LifetimeSession ||
				mock.TestModeInfo.Lifetime == models.LifetimeConnection {
				configMocks = append(configMocks, mock)
			}
		}
	}

	// See FilterTcsMocks call above: the disk loader runs lax; the
	// agent-level filter enforces strictness based on config.
	unfiltered := pkg.FilterConfigMocks(ctx, ys.Logger, configMocks, afterTime, beforeTime, false)

	return unfiltered, nil
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
	// Legitimate names with a '..' substring (e.g. "v1..v2", "team..a")
	// are allowed as long as no path element equals "." or "..". We
	// check that by enforcing: no separator, no volume qualifier,
	// not absolute, not "." or ".." verbatim, and stable under
	// filepath.Clean.
	//
	// The VolumeName check is the Windows-only escape Copilot
	// flagged on keploy#4045 review round 26: `filepath.IsAbs("C:")`
	// returns false, `Clean("C:") == "C:"`, and there are no
	// separators, but `filepath.Join(base, "C:")` on Windows
	// absorbs the volume qualifier and drops the base, so a
	// re-record request with testSetID="C:" would turn os.Remove
	// into a delete at the root of drive C: on a Windows runner.
	// filepath.VolumeName returns the drive / UNC prefix when the
	// path carries one, and is empty on the legitimate path.
	// filepath.VolumeName is Windows-specific at runtime and returns
	// "" for "C:" on Linux, so we ALSO explicitly reject any ID
	// containing ':' — a legitimate test-set name has no reason
	// to carry one, and this makes the Linux build reject the
	// same strings the Windows runtime would. strings.HasPrefix
	// catches UNC-style ("\\\\server") and extended-length
	// ("\\\\?\\C:") prefixes for the same cross-platform reason.
	if testSetID == "" ||
		testSetID == "." ||
		testSetID == ".." ||
		strings.ContainsAny(testSetID, "/\\:") ||
		filepath.VolumeName(testSetID) != "" ||
		filepath.IsAbs(testSetID) ||
		filepath.Clean(testSetID) != testSetID {
		return fmt.Errorf("rejecting DeleteMocksForSet: testSetID %q must be a non-empty single-segment name (no separators, no drive/volume prefix, not '.' or '..') under the mocks output directory", testSetID)
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

func (ys *MockYaml) GetCurrMockID() int64 {
	return atomic.LoadInt64(&ys.idCounter)
}

func (ys *MockYaml) ResetCounterID() {
	atomic.StoreInt64(&ys.idCounter, -1)
}
