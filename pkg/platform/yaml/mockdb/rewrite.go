package mockdb

import (
	"bufio"
	"bytes"
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

// fileDoc is one document of a yaml/json mock file, read and decoded.
type fileDoc struct {
	// mocks is the document's mock; none when the decoders skipped it.
	mocks []*models.Mock
	// raw is the document as read (see yaml.MockReader.ReadNextDocBytes).
	raw []byte
	// name and kind are the document's own, when it parsed: for a document
	// the decoders skipped, they say what was skipped.
	name string
	kind models.Kind
	// verbatim says a rewrite copies raw rather than re-encoding mocks:
	//   - the decoders skipped the document (an enterprise-only kind with no
	//     mapper registered, a kind, a connection-failure field or value this
	//     keploy does not know): what this keploy cannot decode it cannot
	//     judge, and dropping it would delete, from the user's recording,
	//     mocks that a newer keploy (or keploy enterprise) wrote and would
	//     replay;
	//   - it is a connection failure (copiedVerbatim): nothing in this keploy
	//     changes one (the prune keeps it, see pruneKeeps, and no noise is
	//     learned for it), and the copy keeps it as the keploy that recorded
	//     it wrote it;
	//   - it is the file's incomplete last document (see nextDoc).
	verbatim bool
	// unterminated says raw is the file's last document and ends part-way
	// through a line.
	unterminated bool
	// incomplete says it is the file's last document, ending part-way
	// through a line, and the read skipped it (see nextDoc). A rewrite does
	// not copy it: it moves it aside (mockFileRewriter.copyDoc).
	incomplete bool
}

// nextDoc reads and decodes the reader's next document. readErr is io.EOF
// after the last document, or the error reading or parsing one; decodeErr is
// the error decoding a parsed one. Either fails the whole test set, except for
// the file's last document when the file ends part-way through a line
// (yaml.MockReader.Unterminated): a write that was interrupted (the recorder
// killed, the disk full; appendMock writes a large document in pieces) leaves
// exactly that, and losing that one mock must not fail every test in the set.
//
// Such a YAML document is never served. That it parses, or even decodes,
// proves nothing: yaml.v3 reads almost any prefix of a document, and what it
// reads is the recorded mock with everything after the cut missing (a status
// code of 2, a URL cut short, a time with no clock). It is skipped, with one
// WARN naming the mock file, and a rewrite moves it aside. The one
// exception is a file that stops inside the separator before the next
// document (its last line is "-" or "--"): the document before the separator
// is complete, and is read as any other. An NDJSON line is a whole JSON object
// or does not parse, so a last line with no newline is read when it parses and
// decodes, and skipped, the same way, when it does not.
//
// rewrite says the caller rewrites the file (the prune, the noise write-back):
// it reports what it keeps as written at Debug, as a read of the set already
// reported it. logger is the caller's, naming the mock file.
func nextDoc(reader *yaml.MockReader, logger *zap.Logger, rewrite bool) (d fileDoc, readErr, decodeErr error) {
	if reader.Format() == yaml.FormatJSON {
		doc, raw, err := reader.ReadNextDocJSONBytes()
		if raw == nil {
			return d, err, nil
		}
		d.raw, d.unterminated = raw, reader.Unterminated()
		if d.unterminated && (err != nil || !jsonDocDecodes(doc)) {
			d.verbatim, d.incomplete = true, true
			if err == nil {
				d.name, d.kind = doc.Name, doc.Kind
			}
			reportIncompleteTail(logger, rewrite, yaml.FormatJSON, d.name)
			return d, nil, nil
		}
		if err != nil {
			return d, err, nil
		}
		d.name, d.kind = doc.Name, doc.Kind
		d.mocks, decodeErr = decodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, logger, rewrite)
	} else {
		doc, raw, err := reader.ReadNextDocBytes()
		if raw == nil {
			return d, err, nil
		}
		d.raw, d.unterminated = raw, reader.Unterminated()
		if d.unterminated {
			lastLine := bytes.LastIndexByte(raw, '\n') + 1
			if !separatorPrefix(raw[lastLine:]) {
				d.verbatim, d.incomplete = true, true
				// Named from its whole lines only: the unfinished one may be
				// the name, cut short.
				if whole, ok := parseYAMLDoc(raw[:lastLine]); ok {
					d.name, d.kind = whole.Name, whole.Kind
				}
				reportIncompleteTail(logger, rewrite, yaml.FormatYAML, d.name)
				return d, nil, nil
			}
			// The document before the unfinished separator is complete.
			whole, ok := parseYAMLDoc(raw[:lastLine])
			if !ok {
				if len(bytes.TrimSpace(raw[:lastLine])) == 0 {
					// Nothing but the start of a separator: no document.
					d.verbatim = true
					return d, nil, nil
				}
				return d, errors.New("failed to decode the mock document before the unfinished separator at the end of the file"), nil
			}
			doc, err = whole, nil
		}
		if err != nil {
			return d, err, nil
		}
		d.name, d.kind = doc.Name, doc.Kind
		d.mocks, decodeErr = decodeMocks([]*yaml.NetworkTrafficDoc{doc}, logger, rewrite)
	}
	if decodeErr != nil {
		return fileDoc{}, nil, decodeErr
	}
	d.verbatim = len(d.mocks) == 0 || copiedVerbatim(d.mocks)
	return d, nil, nil
}

// separatorPrefix reports whether line, the unfinished last line of a mock
// file, is the start of the "---" separator keploy writes, at the start of a
// line, before the next document.
func separatorPrefix(line []byte) bool {
	return len(line) > 0 && len(line) < 3 && bytes.HasPrefix([]byte("---"), line)
}

// parseYAMLDoc parses one YAML mock document; ok is false when it does not
// parse or is empty.
func parseYAMLDoc(data []byte) (*yaml.NetworkTrafficDoc, bool) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, false
	}
	var doc yaml.NetworkTrafficDoc
	if yamlLib.Unmarshal(data, &doc) != nil {
		return nil, false
	}
	return &doc, true
}

// jsonDocDecodes reports, without logging, whether the decoders take an NDJSON
// document (skipping it counts: that is not an error).
func jsonDocDecodes(doc *yaml.NetworkTrafficDocJSON) bool {
	_, err := decodeMocksJSON([]*yaml.NetworkTrafficDocJSON{doc}, zap.NewNop(), true)
	return err == nil
}

// reportIncompleteTail says that the mock file's last mock does not end with
// a newline and was not read (see nextDoc): at WARN on a read of the set, at
// Debug on a rewrite, which copies it as written.
func reportIncompleteTail(logger *zap.Logger, rewrite bool, format yaml.Format, name string) {
	if rewrite {
		logger.Debug("the mock file's last mock does not end with a newline; the rewrite moves it aside", zap.String("mock", name))
		return
	}
	if format == yaml.FormatJSON {
		// A whole NDJSON line that decodes is read (nextDoc): this one does
		// not, and a newline would not make it.
		logger.Warn("the last line of the mock file does not end with a newline and is not a whole mock, probably because a write was interrupted (the recorder stopped, or the disk filled), and was not read",
			zap.String("mock", name),
			zap.String("next_step", "re-record the test set"))
		return
	}
	logger.Warn("the last mock in the mock file does not end with a newline, probably because a write was interrupted (the recorder stopped, or the disk filled), and was not read",
		zap.String("mock", name),
		zap.String("next_step", "re-record the test set; if you saved this file by hand, add a final newline"))
}

// copiedVerbatim reports whether a decoded document is one a rewrite copies
// as read rather than re-encodes (see fileDoc.verbatim).
func copiedVerbatim(mocks []*models.Mock) bool {
	return len(mocks) == 1 && mocks[0].Kind == models.ConnectionFailure
}

// pruneKeeps reports whether the prune keeps mock. A kept, consumed mock
// also takes the request-body noise the replay learned for it.
func pruneKeeps(mock *models.Mock, mockNames map[string]models.MockState, pruneBefore, startupCutoffTime time.Time) bool {
	if mock.Spec.Metadata["type"] == "config" {
		return true
	}
	// The prune drops what no test consumed. Nothing consumes a connection
	// failure at replay yet, so "not consumed" says nothing about whether one
	// is needed, and pruning it would delete it from a recording made by a
	// keploy that replays it. (The yaml/json prune copies these documents
	// before asking, see nextDoc; the gob prune asks here.)
	if mock.Kind == models.ConnectionFailure {
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
	// incomplete is the source's incomplete last document, which commit
	// moves aside (see copyDoc).
	incomplete []byte
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

// copyDoc puts a document nextDoc read, and the rewrite does not re-encode
// (fileDoc.verbatim), in the replacement as it was read, ending with a
// newline, so the replacement always ends with one and a later append starts
// on a line of its own. Two documents at the end of a file that does not end
// with a newline are not copied:
//   - the last document the read skipped (fileDoc.incomplete) is held back,
//     and commit moves it aside;
//   - the start of a separator ("-", "--") a write stopped in is dropped: the
//     document before it is whole, and is copied without it.
func (rw *mockFileRewriter) copyDoc(d fileDoc) error {
	if d.incomplete {
		rw.incomplete = d.raw
		return nil
	}
	raw := d.raw
	if d.unterminated && rw.format != yaml.FormatJSON {
		// A YAML document the read took ends inside a separator (nextDoc).
		raw = raw[:bytes.LastIndexByte(raw, '\n')+1]
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	return rw.writeRaw(raw)
}

// writeRaw copies one document into the replacement unchanged: one line of
// NDJSON (which the reader hands back without its newline), or one YAML
// document between separators (with the newline of its every line).
func (rw *mockFileRewriter) writeRaw(doc []byte) error {
	if rw.tmp == nil {
		if err := rw.open(); err != nil {
			return err
		}
	}
	if rw.format != yaml.FormatJSON && rw.written > 0 {
		if _, err := rw.w.WriteString("---\n"); err != nil {
			return err
		}
	}
	if _, err := rw.w.Write(doc); err != nil {
		return err
	}
	if rw.format == yaml.FormatJSON {
		if err := rw.w.WriteByte('\n'); err != nil {
			return err
		}
	}
	rw.written++
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
// always has. The source's incomplete last document, which the rewrite did
// not copy (copyDoc), is first written to a sibling file,
// <file>.incomplete-<unix nanos>, which nothing reads, and one WARN names
// both; when that copy or the replace fails, the copy is removed and the
// original left as it was.
func (rw *mockFileRewriter) commit() error {
	if rw.done {
		return errors.New("mockdb: rewrite already finished")
	}
	var aside string
	if rw.incomplete != nil {
		aside = fmt.Sprintf("%s.incomplete-%d", rw.targetPath, time.Now().UnixNano())
		if err := writeAsideFile(aside, rw.incomplete, 0o644); err != nil {
			_ = os.Remove(aside)
			return fmt.Errorf("failed to move the incomplete last mock of %s aside: %w", rw.targetPath, err)
		}
	}
	if err := rw.replace(); err != nil {
		if aside != "" {
			_ = os.Remove(aside)
		}
		return err
	}
	if aside != "" {
		nextStep := "the moved-aside line is not a mock this keploy reads: delete it, or re-record the test set"
		if rw.format != yaml.FormatJSON {
			nextStep = "if the moved-aside text is a whole mock you saved by hand, append it back to the mock file, after a --- line, with a final newline; otherwise delete it, or re-record the test set"
		}
		rw.logger.Warn("the mock file's last mock did not end with a newline (an interrupted write, or a mock saved by hand without its final newline), so it was not read; the rewrite moved it aside",
			zap.String("mock_file", rw.targetPath),
			zap.String("moved_to", aside),
			zap.String("next_step", nextStep))
	}
	return nil
}

// writeAsideFile is os.WriteFile, replaceable by tests (a copy that cannot be
// written whole: the disk is full).
var writeAsideFile = os.WriteFile

// replace puts the rewritten file in place of the original, or removes the
// original when the rewrite kept nothing.
func (rw *mockFileRewriter) replace() error {
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
	_, err := forEachGobMockCut(path, fn)
	return err
}

// forEachGobMockCut is forEachGobMock that also says whether the stream ended
// part-way through a mock (cut): the file was cut, as an interrupted write
// leaves it, and that last mock was not read.
func forEachGobMockCut(path string, fn func(*models.Mock) error) (cut bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	// Verify the magic header. Files recorded before v1 did not emit
	// a header; we reject them with a clear error rather than decoding
	// a garbled Mock struct. Bump gobMockMagic to v2 when the on-disk
	// format changes in a breaking way.
	magic := make([]byte, len(gobMockMagic))
	if _, err := io.ReadFull(br, magic); err != nil {
		return false, fmt.Errorf("read gob mock magic: %w (file may be truncated or not a keploy gob mock)", err)
	}
	if string(magic) != gobMockMagic {
		return false, fmt.Errorf("gob mock file %s: unrecognized magic %q (want %q) — the file was written by a different keploy version", path, magic, gobMockMagic)
	}
	dec := gob.NewDecoder(br)
	for {
		var m models.Mock
		if err := dec.Decode(&m); err != nil {
			if err == io.EOF {
				return false, nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				return true, nil
			}
			return false, fmt.Errorf("decode gob mock: %w", err)
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
			return false, err
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
