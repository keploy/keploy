// Package mockdb provides a mock database implementation.
package mockdb

import (
	"bufio"
	"context"
	"encoding/gob"
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
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// mockFormatGob is the on-disk extension for the binary gob mock
// format, enabled via KEPLOY_MOCK_FORMAT=gob. The format is a stream
// of gob-encoded *models.Mock (appendable without rewrite). Readers
// auto-detect by checking mocks.gob first, falling back to mocks.yaml.
const mockFormatGob = "gob"

func useGobMockFormat() bool {
	return os.Getenv("KEPLOY_MOCK_FORMAT") == mockFormatGob
}

type MockYaml struct {
	MockPath  string
	MockName  string
	Logger    *zap.Logger
	idCounter int64

	// Async gob writer: background goroutine drains gobQueue and
	// encodes to a persistent *os.File + bufio + gob.Encoder. Parser
	// goroutines never block on disk or gob encoding. Sync fallback
	// activates when the queue is full so no mock is dropped.
	gobOnce      sync.Once
	gobQueue     chan gobWriteJob
	gobStop      chan struct{}
	gobDone      chan struct{}
	gobMu        sync.Mutex
	gobFilePath  string
	gobFile      *os.File
	gobBufw      *bufio.Writer
	gobEnc       *gob.Encoder
	gobOverflows atomic.Uint64
}

type gobWriteJob struct {
	mock     *models.Mock
	testSet  string
	filename string
}

const mockFileLockStripeCount = 256

var mockFileLockStripes [mockFileLockStripeCount]sync.RWMutex

func New(Logger *zap.Logger, mockPath string, mockName string) *MockYaml {
	return &MockYaml{
		MockPath:  mockPath,
		MockName:  mockName,
		Logger:    Logger,
		idCounter: -1,
	}
}

func mockFileLockKey(path, fileName string) string {
	fullPath := filepath.Join(path, fileName+".yaml")
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

func (ys *MockYaml) writeMocksAtomically(path, fileName string, mocks []*models.Mock) error {
	targetPath := filepath.Join(path, fileName+".yaml")
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
	lock := getMockFileLock(mockFileLockKey(path, mockFileName))
	lock.Lock()
	defer lock.Unlock()

	ys.Logger.Debug("pruning unused mocks",
		zap.Any("consumedMocks", mockNames),
		zap.String("testSetID", testSetID),
		zap.String("path", filepath.Join(path, mockFileName+".yaml")),
		zap.Time("pruneBefore", pruneBefore))

	// Read the mocks from the yaml file
	mockPath, err := yaml.ValidatePath(filepath.Join(path, mockFileName+".yaml"))
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read mocks due to inaccessible path", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}
	if _, err := os.Stat(mockPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		utils.LogError(ys.Logger, err, "failed to find the mocks yaml file")
		return err
	}
	reader, err := yaml.NewMockReader(ctx, ys.Logger, path, mockFileName)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}
	defer reader.Close()

	var mockYamls []*yaml.NetworkTrafficDoc
	for {
		doc, err := reader.ReadNextDoc()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to decode the yaml file documents", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
			return fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
		}
		mockYamls = append(mockYamls, doc)
	}
	mocks, err := DecodeMocks(mockYamls, ys.Logger)
	if err != nil {
		return err
	}

	newMocks := make([]*models.Mock, 0, len(mocks))
	prunedCount := 0
	for _, mock := range mocks {
		if mock.Spec.Metadata["type"] == "config" {
			newMocks = append(newMocks, mock)
			continue
		}
		if _, ok := mockNames[mock.Name]; ok {
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
	}

	if err := ys.writeMocksAtomically(path, mockFileName, newMocks); err != nil {
		return err
	}

	ys.Logger.Debug("pruned mocks successfully",
		zap.String("testSetID", testSetID),
		zap.Int("total", len(mocks)),
		zap.Int("kept", len(newMocks)),
		zap.Int("pruned", prunedCount),
		zap.Time("pruneBefore", pruneBefore))

	return nil
}

func (ys *MockYaml) InsertMock(ctx context.Context, mock *models.Mock, testSetID string) error {
	mock.Name = fmt.Sprint("mock-", ys.getNextID())
	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}

	// Binary gob format: async path — InsertMock enqueues, a single
	// background goroutine owns the open file + encoder. pprof showed
	// yaml.v3 encoding at 28-30% of record client CPU; gob round-trips
	// all interface{} Message fields via the gob.Register calls in
	// pkg/models/*. Sync fallback on queue-full so mocks never drop.
	if useGobMockFormat() {
		return ys.insertMockGob(ctx, mock, mockPath, mockFileName)
	}

	mockYaml, err := EncodeMock(mock, ys.Logger)
	if err != nil {
		return err
	}
	lock := getMockFileLock(mockFileLockKey(mockPath, mockFileName))
	lock.Lock()
	defer lock.Unlock()

	// Stream YAML directly to the file instead of marshaling to []byte first.
	isFileEmpty, err := yaml.CreateYamlFile(ctx, ys.Logger, mockPath, mockFileName)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to create yaml file", zap.String("path directory", mockPath), zap.String("yaml", mockFileName))
		return err
	}

	yamlFilePath := filepath.Join(mockPath, mockFileName+".yaml")
	file, err := os.OpenFile(yamlFilePath, os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to open mock file for append: %w", err)
	}
	defer file.Close()

	writer := bufio.NewWriter(file)

	if isFileEmpty {
		if version := utils.GetVersionAsComment(); version != "" {
			if _, err := writer.WriteString(version); err != nil {
				return fmt.Errorf("failed to write version comment: %w", err)
			}
		}
	} else {
		if _, err := writer.WriteString("---\n"); err != nil {
			return fmt.Errorf("failed to write document separator: %w", err)
		}
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	encoder := yamlLib.NewEncoder(writer)
	if err := encoder.Encode(&mockYaml); err != nil {
		return fmt.Errorf("failed to encode mock yaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("failed to close yaml encoder: %w", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush mock writer: %w", err)
	}

	return nil
}

// insertMockGob enqueues the mock for async encoding. One background
// goroutine owns the open file + encoder; parsers never block on
// disk. Queue-full falls back to synchronous write so mocks are never
// dropped — tracked via gobOverflows for observability.
func (ys *MockYaml) insertMockGob(ctx context.Context, mock *models.Mock, mockPath, mockFileName string) error {
	ys.ensureGobWriter(ctx)
	job := gobWriteJob{mock: mock, testSet: mockPath, filename: mockFileName}
	select {
	case ys.gobQueue <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		ys.gobOverflows.Add(1)
		return ys.gobWriteSync(ctx, mock, mockPath, mockFileName)
	}
}

func (ys *MockYaml) ensureGobWriter(ctx context.Context) {
	ys.gobOnce.Do(func() {
		ys.gobQueue = make(chan gobWriteJob, 4096)
		ys.gobStop = make(chan struct{})
		ys.gobDone = make(chan struct{})
		go ys.gobWriterLoop(ctx)
	})
}

func (ys *MockYaml) gobWriterLoop(ctx context.Context) {
	defer close(ys.gobDone)
	for {
		select {
		case job, ok := <-ys.gobQueue:
			if !ok {
				ys.gobFlushAndClose()
				return
			}
			if err := ys.gobWriteOne(job); err != nil {
				utils.LogError(ys.Logger, err, "async gob mock writer failed — continuing",
					zap.String("testSet", job.testSet), zap.String("mockName", job.mock.Name))
			}
		case <-ctx.Done():
			ys.drainAndClose()
			return
		case <-ys.gobStop:
			ys.drainAndClose()
			return
		}
	}
}

func (ys *MockYaml) drainAndClose() {
	for {
		select {
		case job := <-ys.gobQueue:
			_ = ys.gobWriteOne(job)
		default:
			ys.gobFlushAndClose()
			return
		}
	}
}

func (ys *MockYaml) gobWriteOne(job gobWriteJob) error {
	ys.gobMu.Lock()
	defer ys.gobMu.Unlock()
	want := filepath.Join(job.testSet, job.filename+".gob")
	if ys.gobFilePath != want || ys.gobFile == nil {
		if err := ys.gobReopenLocked(job.testSet, job.filename); err != nil {
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
	f, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return fmt.Errorf("open gob mock file: %w", err)
	}
	ys.gobFile = f
	// 256 KB buffer holds dozens of mocks before a syscall; bufio
	// autoflushes at fill. Shutdown drains explicitly.
	ys.gobBufw = bufio.NewWriterSize(f, 256*1024)
	ys.gobEnc = gob.NewEncoder(ys.gobBufw)
	ys.gobFilePath = filePath
	return nil
}

func (ys *MockYaml) gobFlushAndClose() {
	ys.gobMu.Lock()
	defer ys.gobMu.Unlock()
	if ys.gobBufw != nil {
		_ = ys.gobBufw.Flush()
	}
	if ys.gobFile != nil {
		_ = ys.gobFile.Close()
	}
	ys.gobFile = nil
	ys.gobBufw = nil
	ys.gobEnc = nil
}

// gobWriteSync is the sync fallback when the async queue is full.
// Holds the async writer mutex so our write doesn't interleave with
// the background goroutine's output mid-encoding.
func (ys *MockYaml) gobWriteSync(ctx context.Context, mock *models.Mock, mockPath, mockFileName string) error {
	if err := os.MkdirAll(mockPath, 0o777); err != nil {
		return fmt.Errorf("failed to create mock directory: %w", err)
	}
	gobFilePath := filepath.Join(mockPath, mockFileName+".gob")
	ys.gobMu.Lock()
	defer ys.gobMu.Unlock()
	if ys.gobBufw != nil {
		_ = ys.gobBufw.Flush()
	}
	f, err := os.OpenFile(gobFilePath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return fmt.Errorf("failed to open gob mock file: %w", err)
	}
	defer f.Close()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	w := bufio.NewWriter(f)
	enc := gob.NewEncoder(w)
	if err := enc.Encode(mock); err != nil {
		return fmt.Errorf("failed to encode mock gob: %w", err)
	}
	return w.Flush()
}

// Close drains the async gob writer and flushes the file. Safe to
// call multiple times. Call at record shutdown so all queued mocks
// are on disk before the process exits.
func (ys *MockYaml) Close() error {
	if ys.gobStop == nil {
		return nil
	}
	select {
	case <-ys.gobStop:
	default:
		close(ys.gobStop)
	}
	select {
	case <-ys.gobDone:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("timed out waiting for gob writer to flush")
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
	dec := gob.NewDecoder(bufio.NewReader(f))
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
	lock := getMockFileLock(mockFileLockKey(path, mockFileName))
	lock.RLock()
	defer lock.RUnlock()

	// Prefer gob binary format when present (low-latency record output).
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
			if mock.Spec.Metadata["type"] == "config" {
				continue
			}
			switch mock.Kind {
			case "Generic", "Postgres", "PostgresV2", "Http", "Http2", "Redis", "MySQL", "DNS":
				tcsMocks = append(tcsMocks, mock)
			}
		}
		return pkg.FilterTcsMocks(ctx, ys.Logger, tcsMocks, afterTime, beforeTime), nil
	}

	mockPath, err := yaml.ValidatePath(path + "/" + mockFileName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		// Use buffered reader for memory-efficient reading of large mock files
		reader, err := yaml.NewMockReader(ctx, ys.Logger, path, mockFileName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from yaml file", zap.String("session", filepath.Base(path)), zap.String("path", mockPath))
			return nil, err
		}
		defer reader.Close()

		hasContent := false
		for {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
			}
			hasContent = true

			// Decode each YAML document into models.Mock as it is read.
			mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}

			for _, mock := range mocks {
				_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]

				_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
				if isMappedToSpecificTest && !isNeededForCurrentRun {
					continue
				}
				// Unification (Phase 3): resolve the mock's typed
				// Lifetime once via DeriveLifetime — which reads
				// Spec.Metadata["type"] first and falls back to the
				// legacy kind-switch only for pre-tag recordings
				// (logged via LegacyKindFallbackFires). Routing into
				// the per-test (tcsMocks) pool is then purely
				// Lifetime-driven. LifetimePerTest lands here; Session
				// and Connection land in the unfiltered/config pool
				// returned by the sibling GetUnFilteredMocks below.
				mock.DeriveLifetime()
				if mock.TestModeInfo.Lifetime == models.LifetimePerTest {
					tcsMocks = append(tcsMocks, mock)
				}
			}
		}

		if !hasContent {
			utils.LogError(ys.Logger, nil, "failed to read the mocks from yaml file", zap.String("session", filepath.Base(path)), zap.String("path", mockPath))
			return nil, fmt.Errorf("failed to get mocks, empty file")
		}
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
	lock := getMockFileLock(mockFileLockKey(path, mockName))
	lock.RLock()
	defer lock.RUnlock()

	// Prefer gob binary format when present.
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
			isConfig := mock.Spec.Metadata["type"] == "config"
			isUnfilteredKind := false
			switch mock.Kind {
			case "Generic", "Postgres", "PostgresV2", "Http", "Http2", "Redis", "MySQL", "DNS":
				isUnfilteredKind = true
			}
			if isConfig || !isUnfilteredKind {
				configMocks = append(configMocks, mock)
			}
		}
		return pkg.FilterConfigMocks(ctx, ys.Logger, configMocks, afterTime, beforeTime), nil
	}

	mockPath, err := yaml.ValidatePath(path + "/" + mockName + ".yaml")
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(mockPath); err == nil {
		// Use buffered reader for memory-efficient reading of large mock files
		reader, err := yaml.NewMockReader(ctx, ys.Logger, path, mockName)
		if err != nil {
			utils.LogError(ys.Logger, err, "failed to read the mocks from config yaml", zap.String("session", filepath.Base(path)))
			return nil, err
		}
		defer reader.Close()

		for {
			doc, err := reader.ReadNextDoc()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("failed to decode the yaml file documents. error: %v", err.Error())
			}

			// Decode each YAML document into models.Mock as it is read.
			mocks, err := DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, ys.Logger)
			if err != nil {
				utils.LogError(ys.Logger, err, "failed to decode the config mocks from yaml doc", zap.String("session", filepath.Base(path)))
				return nil, err
			}

			for _, mock := range mocks {
				_, isMappedToSpecificTest := mocksThatHaveMappings[mock.Name]

				_, isNeededForCurrentRun := mocksWeNeed[mock.Name]
				if isMappedToSpecificTest && !isNeededForCurrentRun {
					continue
				}
				// Unification (Phase 3): Lifetime-only routing. A mock
				// lands in the session/config pool iff DeriveLifetime
				// classified it as Session or Connection. Old kind-
				// switch behaviour is preserved byte-for-byte for pre-
				// tag recordings because DeriveLifetime's compat
				// fallback maps the same kind list to LifetimeSession.
				mock.DeriveLifetime()
				if mock.TestModeInfo.Lifetime == models.LifetimeSession ||
					mock.TestModeInfo.Lifetime == models.LifetimeConnection {
					configMocks = append(configMocks, mock)
				}
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
	mockFileName := "mocks"
	if ys.MockName != "" {
		mockFileName = ys.MockName
	}
	path := filepath.Join(ys.MockPath, testSetID)

	// Read the mocks from the yaml file
	mockPath, err := yaml.ValidatePath(filepath.Join(path, mockFileName+".yaml"))
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to read mocks due to inaccessible path", zap.String("at_path", filepath.Join(path, mockFileName+".yaml")))
		return err
	}

	// Delete all contents of the mocks directory
	err = os.RemoveAll(mockPath)
	if err != nil {
		utils.LogError(ys.Logger, err, "failed to delete old mocks", zap.String("path", mockPath))
		return err
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
