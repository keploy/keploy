package mockdb

import (
	"bufio"
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// A test set's mock file is rewritten in place after a replay: the prune
// (UpdateMocks) drops the mocks no test consumed, and the noise write-back
// (PersistMockNoise) folds learned request-body noise into the mocks. Both
// used to read and decode the whole file, then write it back. A mock's YAML
// parse tree is ~10x its text, so on a 335 MB recording that peaked at 7 GiB
// and was OOM-killed — taking the recording, which the caller had not yet
// persisted anywhere else, with it.
//
// Both now stream: each mock is read, decoded, judged and (if it stays)
// written to a sibling temp file before the next one is read, so a rewrite
// holds one mock at a time whatever the file's size. The temp file replaces
// the original only once every mock is written, so a rewrite that fails, is
// cancelled or dies part way leaves the original as it was.

// nextMocks reads and decodes the reader's next document: no mock for a
// document the decoders skip (an enterprise-only kind with no mapper
// registered), one otherwise. readErr is io.EOF after the last document, or
// the error reading or parsing a document; decodeErr is the error decoding a
// parsed document into a mock.
func nextMocks(reader *yaml.MockReader, logger *zap.Logger) (mocks []*models.Mock, readErr, decodeErr error) {
	if reader.Format() == yaml.FormatJSON {
		doc, err := reader.ReadNextDocJSON()
		if err != nil {
			return nil, err, nil
		}
		mocks, err = DecodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, logger)
		return mocks, nil, err
	}
	doc, err := reader.ReadNextDoc()
	if err != nil {
		return nil, err, nil
	}
	mocks, err = DecodeMocks([]*yaml.NetworkTrafficDoc{doc}, logger)
	return mocks, nil, err
}

// pruneKeeps reports whether the prune keeps mock. A kept, consumed mock
// also takes the request-body noise the replay learned for it.
func pruneKeeps(mock *models.Mock, mockNames map[string]models.MockState, pruneBefore, startupCutoffTime time.Time) bool {
	if mock.Spec.Metadata["type"] == "config" {
		return true
	}
	if st, ok := mockNames[mock.Name]; ok {
		// Persist any request-body noise detected during schema-based
		// auto-replay matching (config.Test.SchemaNoiseDetection) onto the
		// disk-read mock before it is re-written. Stored uniformly on the
		// kind-agnostic MockSpec.ReqBodyNoise for every parser (HTTP included).
		if len(st.ReqBodyNoise) > 0 {
			mock.Spec.ReqBodyNoise = mergeReqBodyNoise(mock.Spec.ReqBodyNoise, st.ReqBodyNoise)
		}
		return true
	}
	// Preserve mocks written after replay start.
	if !mock.Spec.ReqTimestampMock.IsZero() && mock.Spec.ReqTimestampMock.After(pruneBefore) {
		return true
	}
	// Keep startup/init mocks: every mock recorded before startupCutoffTime
	// (app boot up to and including the first StartupMockTestCaseWindow test
	// cases) is connection-level or app-init traffic (DNS, TLS, DB handshake,
	// config fetch, etc.) plus the outbound calls of those early tests. In
	// multi-test-set replays without app restart, these won't be consumed in
	// later test-sets but are still needed for app startup on future replays.
	return !startupCutoffTime.IsZero() && !mock.Spec.ReqTimestampMock.IsZero() &&
		mock.Spec.ReqTimestampMock.Before(startupCutoffTime)
}

// mockFileRewriter writes the replacement of a yaml/json mock file one mock
// at a time, encoded exactly as the recorder-side rewrite always encoded it,
// into a temp file beside it. Nothing touches the original until commit.
type mockFileRewriter struct {
	logger     *zap.Logger
	dir        string
	fileName   string
	format     yaml.Format
	targetPath string

	tmp     *os.File
	tmpPath string
	w       *bufio.Writer
	jsonEnc *json.Encoder
	written int
	done    bool
}

func newMockFileRewriter(logger *zap.Logger, dir, fileName string, format yaml.Format) *mockFileRewriter {
	return &mockFileRewriter{
		logger:     logger,
		dir:        dir,
		fileName:   fileName,
		format:     format,
		targetPath: filepath.Join(dir, fileName+"."+format.FileExtension()),
	}
}

// open creates the temp file on the first write, so a rewrite that keeps
// nothing never creates one.
func (rw *mockFileRewriter) open() error {
	if err := os.MkdirAll(rw.dir, 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(rw.dir, rw.fileName+".*.tmp")
	if err != nil {
		return err
	}
	rw.tmp, rw.tmpPath = tmp, tmp.Name()
	rw.w = bufio.NewWriter(tmp)
	if rw.format == yaml.FormatJSON {
		rw.jsonEnc = json.NewEncoder(rw.w)
		return nil
	}
	if version := utils.GetVersionAsComment(); version != "" {
		if _, err := rw.w.WriteString(version); err != nil {
			return err
		}
	}
	return nil
}

func (rw *mockFileRewriter) write(mock *models.Mock) error {
	if rw.tmp == nil {
		if err := rw.open(); err != nil {
			return err
		}
	}
	if rw.format == yaml.FormatJSON {
		// NDJSON: one JSON object per line. The JSON write path is fully
		// yaml-free — EncodeMockJSON covers every kind that keploy records
		// (HTTP, DNS, Generic, Redis, Kafka, HTTP/2, gRPC, PostgresV2, MySQL,
		// Mongo). An unexpected kind is an error rather than a silent
		// fallback through yaml.Node.
		jsonDoc, handled, err := EncodeMockJSON(mock, rw.logger)
		if err != nil {
			return err
		}
		if !handled {
			return fmt.Errorf("mockdb: unsupported mock kind %q for JSON format", mock.Kind)
		}
		// json.Encoder appends the trailing newline.
		if err := rw.jsonEnc.Encode(jsonDoc); err != nil {
			return err
		}
		rw.written++
		return nil
	}
	if rw.written > 0 {
		if _, err := rw.w.WriteString("---\n"); err != nil {
			return err
		}
	}
	mockYaml, err := EncodeMock(mock, rw.logger)
	if err != nil {
		return err
	}
	data, err := yamlLib.Marshal(&mockYaml)
	if err != nil {
		return err
	}
	if _, err := rw.w.Write(data); err != nil {
		return err
	}
	rw.written++
	return nil
}

// commit puts the rewritten file in place of the original. A rewrite that
// kept no mock removes the original instead, as a prune that keeps nothing
// always has.
func (rw *mockFileRewriter) commit() error {
	if rw.done {
		return errors.New("mockdb: rewrite already finished")
	}
	if rw.tmp == nil {
		rw.done = true
		if err := os.Remove(rw.targetPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := rw.w.Flush(); err != nil {
		return err
	}
	if err := rw.tmp.Sync(); err != nil {
		return err
	}
	err := rw.tmp.Close()
	rw.tmp = nil
	if err != nil {
		return err
	}
	fileMode, err := resolveMockFileMode(rw.targetPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(rw.tmpPath, fileMode); err != nil {
		return err
	}
	if err := replaceFile(rw.logger, rw.tmpPath, rw.targetPath); err != nil {
		return err
	}
	rw.done = true
	return nil
}

// discard drops a rewrite that was not committed, leaving the original as it
// was. A no-op after commit, so callers defer it.
func (rw *mockFileRewriter) discard() {
	if rw.done {
		return
	}
	rw.done = true
	if rw.tmp != nil {
		_ = rw.tmp.Close()
	}
	if rw.tmpPath != "" {
		_ = os.Remove(rw.tmpPath)
	}
}

// forEachGobMock decodes a mocks.gob file one mock at a time and hands each
// to fn. The async writer holds one *gob.Encoder alive for the whole
// session, so the on-disk file is a single continuous gob stream — mirrored
// here with one *gob.Decoder that keeps the type table live across Decode
// calls. Mid-stream ErrUnexpectedEOF is treated as end-of-data (partial write
// from a crashed writer — we lose the tail mock, not the batch).
//
// Constraint: because the encoder session owns the type table, you cannot
// usefully append to an existing mocks.gob from a fresh encoder — the new
// encoder's type table will conflict. Readers that need to merge multiple
// sessions must read each file independently.
func forEachGobMock(path string, fn func(*models.Mock) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	// Verify the magic header. Files recorded before v1 did not emit
	// a header; we reject them with a clear error rather than decoding
	// a garbled Mock struct. Bump gobMockMagic to v2 when the on-disk
	// format changes in a breaking way.
	magic := make([]byte, len(gobMockMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return fmt.Errorf("read gob mock magic: %w (file may be truncated or not a keploy gob mock)", err)
	}
	if string(magic) != gobMockMagic {
		return fmt.Errorf("gob mock file %s: unrecognized magic %q (want %q) — the file was written by a different keploy version", path, magic, gobMockMagic)
	}
	dec := gob.NewDecoder(br)
	for {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return fmt.Errorf("decode gob mock: %w", err)
		}
		// Lifetime and LifetimeDerived are RUNTIME-ONLY (models.Mock tags them
		// json:"-" bson:"-" and says persisting them would create a second
		// source of truth). encoding/gob ignores struct tags, so a gob file
		// carries whatever the recorder stamped — and DeriveLifetime then
		// short-circuits on the reloaded flag instead of re-deriving.
		//
		// The effect is that the SAME recording replays into a different tier
		// depending on the storage format: gob keeps the recorder's per-test
		// tag, while yaml drops it and the lax kind-fallback promotes the mock
		// to session. A call two tests depend on is then consumed by the first
		// and missing for the second — a CPU-optimisation knob silently
		// changing replay semantics.
		//
		// Clearing on READ (not just on write) also repairs the recordings
		// already on disk.
		var zeroLifetime models.Lifetime
		m.TestModeInfo.Lifetime = zeroLifetime
		m.TestModeInfo.LifetimeDerived = false
		if err := fn(&m); err != nil {
			return err
		}
	}
}

// gobFileRewriter writes the replacement of a mocks.gob one mock at a time
// into a temp file beside it — a fresh single-encoder stream with the magic
// header, since gob does not support append — and renames it over the
// original on commit. Nothing touches the original until then.
type gobFileRewriter struct {
	gobPath string
	tmp     *os.File
	tmpPath string
	bw      *bufio.Writer
	enc     *gob.Encoder
	done    bool
}

// newGobFileRewriter creates the temp file with mocks.gob's permissions.
// os.CreateTemp creates its file 0600, so without the chmod the rewrite would
// quietly narrow mocks.gob from whatever mode the record writer produced
// (typically 0644 via umask 0022) down to owner-only, which breaks replay for
// any other user/process on the box. Stat before CreateTemp: if the source
// file is gone, fall back to the same mode the gob writer uses when it opens
// mocks.gob fresh (0644) so we do not introduce a new mode-inheritance path.
func newGobFileRewriter(gobPath string) (*gobFileRewriter, error) {
	dir := filepath.Dir(gobPath)
	base := filepath.Base(gobPath)
	var originalMode os.FileMode = 0644
	if info, statErr := os.Stat(gobPath); statErr == nil {
		originalMode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, base+".rewrite.*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create gob rewrite tmp: %w", err)
	}
	rw := &gobFileRewriter{gobPath: gobPath, tmp: tmp, tmpPath: tmp.Name()}
	// Must happen before any concurrent reader observes the renamed file.
	if err := os.Chmod(rw.tmpPath, originalMode); err != nil {
		rw.discard()
		return nil, fmt.Errorf("chmod gob rewrite tmp to %o: %w", originalMode, err)
	}
	rw.bw = bufio.NewWriterSize(tmp, 256*1024)
	if _, err := rw.bw.WriteString(gobMockMagic); err != nil {
		rw.discard()
		return nil, fmt.Errorf("write gob magic to rewrite tmp: %w", err)
	}
	rw.enc = gob.NewEncoder(rw.bw)
	return rw, nil
}

func (rw *gobFileRewriter) write(mock *models.Mock) error {
	if err := rw.enc.Encode(mock); err != nil {
		return fmt.Errorf("encode mock during gob rewrite: %w", err)
	}
	return nil
}

// commit renames the rewritten stream over mocks.gob. os.Rename on the same
// filesystem is atomic, so a concurrent reader either sees the full old file
// or the full new one.
func (rw *gobFileRewriter) commit() error {
	if rw.done {
		return errors.New("mockdb: gob rewrite already finished")
	}
	if err := rw.bw.Flush(); err != nil {
		return fmt.Errorf("flush gob rewrite tmp: %w", err)
	}
	if err := rw.tmp.Sync(); err != nil {
		return fmt.Errorf("sync gob rewrite tmp: %w", err)
	}
	err := rw.tmp.Close()
	rw.tmp = nil
	if err != nil {
		return fmt.Errorf("close gob rewrite tmp: %w", err)
	}
	if err := os.Rename(rw.tmpPath, rw.gobPath); err != nil {
		return fmt.Errorf("rename gob rewrite tmp over %s: %w", rw.gobPath, err)
	}
	rw.done = true
	return nil
}

// discard drops a rewrite that was not committed. A no-op after commit.
func (rw *gobFileRewriter) discard() {
	if rw.done {
		return
	}
	rw.done = true
	if rw.tmp != nil {
		_ = rw.tmp.Close()
	}
	_ = os.Remove(rw.tmpPath)
}

// errStopScan ends a decode-only pass early once it has its answer.
var errStopScan = errors.New("mockdb: scan stopped")

// ctxCheckEvery is how many gob mocks pass between context checks. A
// yaml/json rewrite needs none of its own: its reader checks the context on
// every line. The gob stream has no such reader.
const ctxCheckEvery = 1024

func ctxErrEvery(ctx context.Context, i int) error {
	if i%ctxCheckEvery == 0 {
		return ctx.Err()
	}
	return nil
}
