package yaml

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/safeyaml"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type IndexMode string

const (
	ModeDir  IndexMode = "dir"
	ModeFile IndexMode = "file"
)

// Ignored folders
const (
	FolderReports     = "reports"
	FolderTestReports = "testReports"
	FolderSchema      = "schema"
	// FolderAPITests holds the V1 flow's API-test surface — chained CRUD
	// tests authored via `keploy test-gen`, each in its own per-resource
	// subdirectory shaped like an OSS test-set. Reserved here so the
	// test-set scanner never mistakes it for a recorded test-set.
	FolderAPITests = "api-tests"
	// MockStagingSuffix marks the directory `keploy mock record` captures into
	// before promoting it over the real set. Reserved: a dir with this suffix is
	// a transient staging set (normally promoted or discarded within one record),
	// and the enumerators below skip it so a leftover from a hard kill is never
	// listed as a phantom test-set. The single source of truth — the mock service
	// builds the staging name from it too.
	MockStagingSuffix = ".keploy-staging"
)

// NetworkTrafficDoc stores the request-response data of a network call (ingress or egress)
type NetworkTrafficDoc struct {
	Version models.Version `json:"version" yaml:"version"`
	Kind    models.Kind    `json:"kind" yaml:"kind"`
	Name    string         `json:"name" yaml:"name"`
	Spec    yamlLib.Node   `json:"spec" yaml:"spec"`
	// Async, when present, is the async-egress engine's per-mock bookkeeping
	// (lane, order, anchor, poll/duration) — a kind-agnostic top-level block,
	// kept out of the per-kind spec's parser Metadata. See models.AsyncMeta.
	Async        *models.AsyncMeta   `json:"async,omitempty" yaml:"async,omitempty"`
	Noise        *DocNoise           `json:"noise,omitempty" yaml:"noise,omitempty"`
	LastUpdated  *models.LastUpdated `json:"last_updated,omitempty" yaml:"last_updated,omitempty"`
	Curl         string              `json:"curl" yaml:"curl,omitempty"`
	ConnectionID string              `json:"connectionId" yaml:"connectionId,omitempty"`
	Start        string              `json:"start,omitempty" yaml:"start,omitempty"`

	// Variables, when present, names the places of the call that hold a test
	// variable's value (models.MockVariables): a kind-agnostic top-level block,
	// like noise, kept off the per-kind spec.
	Variables models.MockVariables `json:"variables,omitempty" yaml:"variables,omitempty"`

	// Real, on a test case, is the mock.real destinations its request used in
	// its recording (models.TestCase.Real); written and read only under the
	// proven switch.
	Real []string `json:"real,omitempty" yaml:"real,omitempty"`
}

// DocNoise is the unified on-disk representation of a mock's noise, written under
// the single `noise:` key.
//
//   - Req   — request-body field paths to ignore during schema/strict matching
//     (e.g. "body.tier_type"). A plain list of paths: the strict path only ever
//     honoured "ignore this whole field", so per-path regex values are not kept.
//   - Value — exact-match value regexes written by the enterprise obfuscator
//     (the former top-level `noise:` list, models.Mock.Noise).
//
// For backward compatibility the custom unmarshalers also accept the OLD shape
// where `noise:` was a bare string list — that decodes into Value. New writes
// only emit this unified mapping.
type DocNoise struct {
	Req   []string `json:"req,omitempty" yaml:"req,omitempty"`
	Value []string `json:"value,omitempty" yaml:"value,omitempty"`
}

// NewDocNoise builds the unified noise block for encoding from the in-memory
// model fields: the obfuscator value-regex list (models.Mock.Noise) and the
// kind-agnostic schema-noise map (models.MockSpec.ReqBodyNoise). Only the field
// PATHS of the schema-noise map are persisted — the per-path regex values are
// intentionally dropped (unused on the strict path). Returns nil when there is
// nothing to write so `omitempty` drops the `noise:` key entirely. Req paths are
// sorted for deterministic output.
func NewDocNoise(value []string, reqBodyNoise map[string][]string) *DocNoise {
	var req []string
	if len(reqBodyNoise) > 0 {
		req = make([]string, 0, len(reqBodyNoise))
		for path := range reqBodyNoise {
			req = append(req, path)
		}
		sort.Strings(req)
	}
	if len(req) == 0 && len(value) == 0 {
		return nil
	}
	return &DocNoise{Req: req, Value: value}
}

// ValueNoise returns the obfuscator value-regex list (models.Mock.Noise). nil-safe.
func (n *DocNoise) ValueNoise() []string {
	if n == nil {
		return nil
	}
	return n.Value
}

// pathsToNoiseMap turns a list of field paths into the canonical schema-noise map
// shape (path -> empty regex list), the only form the strict/detection path honours.
func pathsToNoiseMap(paths []string) map[string][]string {
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string][]string, len(paths))
	for _, p := range paths {
		out[p] = []string{}
	}
	return out
}

// ResolveReqBodyNoise returns the request-body schema noise for a decoded doc,
// taken from the unified noise.req list. Returns nil when it is absent.
func ResolveReqBodyNoise(noise *DocNoise) map[string][]string {
	if noise != nil && len(noise.Req) > 0 {
		return pathsToNoiseMap(noise.Req)
	}
	return nil
}

// UnmarshalYAML accepts both the new mapping shape ({req: [...], value: [...]})
// and the legacy bare string list (which becomes Value), so old mocks keep
// decoding.
func (n *DocNoise) UnmarshalYAML(value *yamlLib.Node) error {
	if value == nil || value.Tag == "!!null" {
		return nil
	}
	if value.Kind == yamlLib.SequenceNode {
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		n.Value = list
		return nil
	}
	// Alias type to avoid recursing back into this method.
	type rawDocNoise DocNoise
	var raw rawDocNoise
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*n = DocNoise(raw)
	return nil
}

// UnmarshalJSON mirrors UnmarshalYAML: it accepts the new object shape and the
// legacy JSON array (-> Value).
func (n *DocNoise) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '[' {
		var list []string
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		n.Value = list
		return nil
	}
	type rawDocNoise DocNoise
	var raw rawDocNoise
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return err
	}
	*n = DocNoise(raw)
	return nil
}

// ctxReader wraps an io.Reader with a context for cancellation support
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (n int, err error) {
	select {
	case <-cr.ctx.Done():
		return 0, cr.ctx.Err()
	default:
		return cr.r.Read(p)
	}
}

// ctxWriter wraps an io.Writer with a context for cancellation support
type ctxWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (cw *ctxWriter) Write(p []byte) (n int, err error) {
	for len(p) > 0 {
		var written int
		written, err = cw.writer.Write(p)
		n += written
		if err != nil {
			return n, err
		}
		p = p[written:]
	}
	return n, nil
}

func WriteFile(ctx context.Context, logger *zap.Logger, path, fileName string, docData []byte, isAppend bool) error {
	return WriteFileF(ctx, logger, path, fileName, docData, isAppend, FormatYAML)
}

func WriteFileF(ctx context.Context, logger *zap.Logger, path, fileName string, docData []byte, isAppend bool, format Format) error {
	filePath := filepath.Join(path, fileName+"."+format.FileExtension())

	if isAppend {
		isFileEmpty, err := CreateFileF(ctx, logger, path, fileName, format)
		if err != nil {
			utils.LogError(logger, err, "failed to create file", zap.String("path directory", path), zap.String("file", fileName))
			return err
		}
		var sep []byte
		if !isFileEmpty {
			if format == FormatJSON {
				sep = []byte("\n") // NDJSON: newline separator
			} else {
				sep = []byte("---\n") // YAML: document separator
			}
		}
		docData = append(sep, docData...)
		file, err := os.OpenFile(filePath, os.O_WRONLY|os.O_APPEND, fs.ModePerm)
		if err != nil {
			utils.LogError(logger, err, "failed to open file for writing", zap.String("file", filePath))
			return err
		}
		defer func() {
			if err := file.Close(); err != nil {
				utils.LogError(logger, err, "failed to close file", zap.String("file", filePath))
			}
		}()

		cw := &ctxWriter{ctx: ctx, writer: file}
		if _, err = cw.Write(docData); err != nil {
			if err == ctx.Err() {
				return nil // Ignore context cancellation error
			}
			utils.LogError(logger, err, "failed to write the document", zap.String("file name", fileName))
			return err
		}
		return nil
	}

	// Non-append (truncate-and-replace): write to a temp file in the same dir and
	// atomically rename it over the destination, so a concurrent or volume-lagging
	// reader (report-upload, localTestsPassed, reportdb.GetReport) never observes a
	// zero-length or mid-document file. The previous O_TRUNC + streaming write left
	// exactly that window — on overlay/NFS/container-mounted volumes a reader could
	// catch the file empty or half-written and either hard-fail or silently decode a
	// partial document. The mock rewrites in mockdb replace their files the
	// same way. The destination is never created ahead of the rename (only
	// its directory, fileDirF): a new file comes into being whole, with no
	// empty file before it for a reader to find.
	_, exists, err := fileDirF(ctx, logger, path, fileName, format)
	if err != nil {
		utils.LogError(logger, err, "failed to create file", zap.String("path directory", path), zap.String("file", fileName))
		return err
	}
	tmpFile, err := newReplaceTemp(path, fileName, replaceTempPerm(exists))
	if err != nil {
		utils.LogError(logger, err, "failed to create temp file for atomic write", zap.String("path directory", path), zap.String("file", fileName))
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	cw := &ctxWriter{ctx: ctx, writer: tmpFile}
	if _, err = cw.Write(docData); err != nil {
		_ = tmpFile.Close()
		if err == ctx.Err() {
			return nil // Ignore context cancellation error
		}
		utils.LogError(logger, err, "failed to write the document", zap.String("file name", fileName))
		return err
	}
	// Flush to stable storage before the rename so the replaced file is whole even
	// across a crash, then preserve the existing file's mode across the replace.
	if err = tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err = tmpFile.Close(); err != nil {
		return err
	}
	// The replaced file's mode is kept (replaceTempPerm). A new file keeps
	// the temp file's, which was created as the file itself would have been
	// (0o644 less the umask).
	if info, statErr := os.Stat(filePath); statErr == nil {
		if err = os.Chmod(tmpPath, info.Mode().Perm()); err != nil {
			return err
		}
	}
	if err = ReplaceFile(logger, tmpPath, filePath); err != nil {
		utils.LogError(logger, err, "failed to atomically replace the file", zap.String("file", filePath))
		return err
	}
	cleanup = false
	return nil
}

// replaceTempPerm is the mode WriteFileF creates its temp file with. While an
// existing file is replaced, 0o600: the content is no more readable than the
// file it replaces at any moment, and the file's own mode is given to the
// temp file just before the rename. A new file's temp file is created as the
// file itself would be, 0o644 less the umask, and keeps that mode.
func replaceTempPerm(exists bool) os.FileMode {
	if exists {
		return 0o600
	}
	return 0o644
}

// newReplaceTemp creates WriteFileF's temp file: createTempFile, a variable
// so a test can see the file it creates.
var newReplaceTemp = createTempFile

// createTempFile creates a new file in dir for WriteFileF to write and rename
// over fileName: os.CreateTemp's name (fileName, a random number, ".tmp"),
// with perm as os.OpenFile takes it, so the umask applies as it would to the
// file itself. os.CreateTemp's 0o600 would otherwise become the mode of a
// file that is new. Like os.CreateTemp it refuses a fileName holding a path
// separator; unlike it, an empty dir is the working directory, where the file
// it replaces is, not os.TempDir().
func createTempFile(dir, fileName string, perm os.FileMode) (*os.File, error) {
	if strings.ContainsAny(fileName, "/"+string(os.PathSeparator)) {
		return nil, &fs.PathError{Op: "createtemp", Path: fileName, Err: errors.New("pattern contains path separator")}
	}
	for try := 0; try < 10000; try++ {
		name := filepath.Join(dir, fileName+"."+strconv.FormatUint(uint64(rand.Uint32()), 10)+".tmp")
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, &fs.PathError{Op: "createtemp", Path: filepath.Join(dir, fileName+".*.tmp"), Err: fs.ErrExist}
}

// ReplaceFile puts src in place of dst. A rename atomically replaces an
// existing target; when it fails over one (a read-only dst on Windows, or a
// scanner holding a file open), the fallback moves dst aside, renames src
// into place and removes the aside copy. If src still cannot be put in place,
// dst is moved back, so a failed replace leaves the original where it was;
// deleting it before the retry, as this fallback once did, lost both files
// when the retry failed too.
//
// An error means dst was not replaced. Once src is in place the replace has
// succeeded: an aside copy that cannot then be removed (on Windows, one
// another process holds open without sharing delete) is logged as a warning
// naming it, not returned, since callers clean up after a failed replace and
// would delete the file just put in place. WriteFile, the test case writer in
// testdb and the yaml/json mock rewrites in mockdb (the prune and the noise
// write-back) use it.
func ReplaceFile(logger *zap.Logger, src, dst string) error {
	return ReplaceFileWith(logger, os.Rename, src, dst)
}

// ReplaceFileWith is ReplaceFile with every rename made through rename, so a
// test can make the platform refuse one (a file held open, a read-only
// target) and check what a replace leaves behind.
func ReplaceFileWith(logger *zap.Logger, rename func(src, dst string) error, src, dst string) error {
	renameErr := rename(src, dst)
	if renameErr == nil {
		return nil
	}
	if _, statErr := os.Stat(dst); statErr != nil {
		if os.IsNotExist(statErr) {
			return renameErr
		}
		return fmt.Errorf("failed to stat target after rename error: %v; initial rename error: %w", statErr, renameErr)
	}

	aside, err := asidePath(dst)
	if err != nil {
		return fmt.Errorf("failed to name the target's aside copy for replace: %v; initial rename error: %w", err, renameErr)
	}
	if err := rename(dst, aside); err != nil {
		return fmt.Errorf("failed to move target aside for replace: %v; initial rename error: %w", err, renameErr)
	}
	if retryErr := rename(src, dst); retryErr != nil {
		if restoreErr := rename(aside, dst); restoreErr != nil {
			return fmt.Errorf("failed to replace file (%v), and failed to restore the original from %s: %v; initial rename error: %w", retryErr, aside, restoreErr, renameErr)
		}
		return fmt.Errorf("failed to replace file after moving the existing target aside, original restored: %v; initial rename error: %w", retryErr, renameErr)
	}
	if err := os.Remove(aside); err != nil && !os.IsNotExist(err) {
		if logger == nil {
			logger = zap.L()
		}
		logger.Warn("replaced the file, but could not remove its previous version, which is left beside it; delete it once nothing holds it open",
			zap.String("file", dst), zap.String("previousVersion", aside), zap.Error(err))
	}
	return nil
}

// asidePath returns an unused name beside dst for ReplaceFile to move dst to.
func asidePath(dst string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".replaced.*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	// Free the name for the rename: a rename that fails over an existing file
	// would fail onto it.
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func ReadFile(ctx context.Context, logger *zap.Logger, path, name string) ([]byte, error) {
	return ReadFileF(ctx, logger, path, name, FormatYAML)
}

func ReadFileF(ctx context.Context, logger *zap.Logger, path, name string, format Format) ([]byte, error) {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}

	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file", zap.String("file", filePath))
		}
	}()

	cr := &ctxReader{
		ctx: ctx,
		r:   file,
	}

	data, err := io.ReadAll(cr)
	if err != nil {
		if err == ctx.Err() {
			return nil, err // Ignore context cancellation error
		}
		return nil, fmt.Errorf("failed to read the file: %v", err)
	}
	return data, nil
}

// ReadFileAny reads a persisted artifact file, preferring `preferred`'s
// extension but transparently falling back to the other format if a file of
// that extension exists instead. Returns the bytes and the format that was
// actually read — so the caller can decode with the matching unmarshaller.
//
// This is the read-side mechanism that makes replay backward-compatible when
// users switch StorageFormat (or when old YAML recordings are replayed by a
// JSON-defaulted binary).
func ReadFileAny(ctx context.Context, logger *zap.Logger, path, name string, preferred Format) ([]byte, Format, error) {
	other := otherFormat(preferred)
	for _, f := range [2]Format{preferred, other} {
		filePath := filepath.Join(path, name+"."+f.FileExtension())
		if _, statErr := os.Stat(filePath); statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, "", statErr
		}
		data, err := ReadFileF(ctx, logger, path, name, f)
		if err != nil {
			return nil, "", err
		}
		return data, f, nil
	}
	return nil, "", fs.ErrNotExist
}

// ReadFileAnyBounded is ReadFileAny within a size bound and without blocking:
// the file must be a regular file (not a FIFO, a socket, or a symlink to a
// device such as /dev/zero) of at most limit bytes, read through safeyaml
// rather than to EOF. It is for a file keploy reads out of the repository it
// runs in -- a run's report -- which a cloned repo could otherwise point at a
// device to run the command out of memory, or a FIFO to block it. A file of the
// preferred format that is not a regular file is refused, not passed over for
// the other format, matching the CLI's config lookup.
func ReadFileAnyBounded(ctx context.Context, path, name string, preferred Format, limit int64) ([]byte, Format, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	other := otherFormat(preferred)
	for _, f := range [2]Format{preferred, other} {
		filePath := filepath.Join(path, name+"."+f.FileExtension())
		data, err := safeyaml.ReadFile(filePath, limit)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return data, f, nil
	}
	return nil, "", fs.ErrNotExist
}

func CreateYamlFile(ctx context.Context, Logger *zap.Logger, path string, fileName string) (bool, error) {
	return CreateFileF(ctx, Logger, path, fileName, FormatYAML)
}

func CreateFileF(ctx context.Context, Logger *zap.Logger, path string, fileName string, format Format) (bool, error) {
	filePath, exists, err := fileDirF(ctx, Logger, path, fileName, format)
	if err != nil || exists {
		return false, err
	}
	file, err := os.OpenFile(filePath, os.O_CREATE, 0o644)
	if err != nil {
		utils.LogError(Logger, err, "failed to create the file", zap.String("path directory", path), zap.String("file", fileName))
		return false, err
	}
	if err := file.Close(); err != nil {
		utils.LogError(Logger, err, "failed to close the file", zap.String("path directory", path), zap.String("file", fileName))
		return false, err
	}
	return true, nil
}

// fileDirF is CreateFileF short of creating the file: it validates the file's
// path and, when the file does not exist, makes its directory. exists reports
// whether the file was there.
func fileDirF(ctx context.Context, Logger *zap.Logger, path string, fileName string, format Format) (filePath string, exists bool, err error) {
	filePath, err = ValidatePath(filepath.Join(path, fileName+"."+format.FileExtension()))
	if err != nil {
		utils.LogError(Logger, err, "failed to validate the file path", zap.String("path directory", path), zap.String("file", fileName))
		return "", false, err
	}

	if _, err := os.Stat(filePath); err != nil {
		if !os.IsNotExist(err) {
			utils.LogError(Logger, err,
				"failed to stat file — check filesystem permissions and that the configured keploy path is readable/writable by this process",
				zap.String("path directory", path),
				zap.String("file", fileName))
			return "", false, err
		}
		// Honour context cancellation/deadline before doing any
		// filesystem mutations. Previously the check was
		// `ctx.Err() == nil || ctx.Err() == context.Canceled`,
		// which both let cancelled contexts proceed (defeating the
		// cancellation contract) and masked DeadlineExceeded by
		// returning the surrounding os.Stat error instead of
		// ctx.Err().
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, ctxErr
		}
		// 0o755/0o644 rather than the historical 0o777 — nothing
		// in the keploy tree needs world-writable perms and the
		// stricter modes match the rest of the new testdb code.
		if err := os.MkdirAll(filepath.Join(path), 0o755); err != nil {
			utils.LogError(Logger, err, "failed to create a directory for the file", zap.String("path directory", path), zap.String("file", fileName))
			return "", false, err
		}
		return filePath, false, nil
	}
	return filePath, true, nil
}

// openIndexDir opens the folder ReadSessionIndicesF / ReadSessionIndicesAny
// list. A folder that does not exist yet is empty (empty is true, err nil):
// the first recording, or a reports folder no run has written. Any other
// failure to open it -- permission denied, an I/O error, a symlink loop, a
// path that is not a directory -- is returned: the folder may well hold test
// sets, and reading it as empty makes `keploy record` pick test-set-0 again
// and record into the user's existing test-set-0.
func openIndexDir(path string, logger *zap.Logger) (*os.File, bool, error) {
	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err == nil {
		return dir, false, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		logger.Debug("folder does not exist yet; nothing to list", zap.String("path", path))
		return nil, true, nil
	}
	return nil, false, err
}

// listedDir reports whether v, an entry of the folder at path that
// ReadSessionIndicesF / ReadSessionIndicesAny list in ModeDir, is one of its
// folders: a test set, or a test run's reports. A link to a folder -- a
// symbolic link, or a junction on Windows -- is refused, neither listed nor
// passed over: keploy writes into these folders (record, the replay's
// denoise, normalize, templatize, a run's reports), often under sudo, and a
// link, which a cloned repository can carry, would have it write wherever
// the link points. Passed over, the link's own name could be handed out as
// the next test set's, and keploy record would write into the set it points
// to. A link keploy cannot follow is refused too: keploy cannot rule out
// that it points to a folder. A link to a file is no folder, like a file.
func listedDir(path string, v fs.DirEntry) (bool, error) {
	if v.IsDir() {
		return true, nil
	}
	// Anything else but a regular file may be a link: a symbolic link, or on
	// Windows a junction, which the listing reports as irregular, not as a
	// link. os.Stat follows both, and tells a FIFO or a socket for no folder.
	if v.Type().IsRegular() {
		return false, nil
	}
	p := filepath.Join(path, v.Name())
	info, err := os.Stat(p)
	if err == nil && !info.IsDir() {
		return false, nil
	}
	const why = "keploy does not follow a link among the folders it writes into (test sets, test runs)"
	if err != nil {
		if _, lerr := os.Lstat(p); errors.Is(lerr, fs.ErrNotExist) {
			// Gone since the listing: no entry at all.
			return false, nil
		}
		return false, fmt.Errorf("%s is a link keploy cannot follow (%w), and %s: remove it, or replace the link with a real folder", p, err, why)
	}
	target := ""
	if t, rerr := os.Readlink(p); rerr == nil {
		target = " (" + t + ")"
	}
	return false, fmt.Errorf("%s is a link to a folder%s, and %s: replace the link with a real folder holding what it points to", p, target, why)
}

func ReadSessionIndices(ctx context.Context, path string, logger *zap.Logger, mode IndexMode) ([]string, error) {
	return ReadSessionIndicesF(ctx, path, logger, mode, FormatYAML)
}

func ReadSessionIndicesF(ctx context.Context, path string, logger *zap.Logger, mode IndexMode, format Format) ([]string, error) {
	var indices []string

	dir, empty, err := openIndexDir(path, logger)
	if empty || err != nil {
		return indices, err
	}
	defer func() { _ = dir.Close() }()

	files, err := dir.ReadDir(0)
	if err != nil {
		return indices, err
	}

	ext := "." + format.FileExtension()
	for _, v := range files {
		// Skip ignored folders
		if v.Name() == FolderReports || v.Name() == FolderTestReports || v.Name() == FolderSchema || v.Name() == FolderAPITests {
			continue
		}
		// A leftover mock-record staging directory is not a test-set.
		if strings.HasSuffix(v.Name(), MockStagingSuffix) {
			continue
		}

		name := v.Name()

		switch mode {
		case ModeDir:
			isDir, err := listedDir(path, v)
			if err != nil {
				return nil, err
			}
			if isDir {
				indices = append(indices, name)
			}
		case ModeFile:
			fileExt := filepath.Ext(name)
			if fileExt != ext {
				continue
			}
			name = name[:len(name)-len(fileExt)]
			indices = append(indices, name)
		}
	}

	return indices, nil
}

// ReadSessionIndicesAny is the format-agnostic variant of
// ReadSessionIndicesF. In ModeFile it accepts both .yaml and .json files
// (deduplicating by stripped base name) so callers discover test/report
// files regardless of the format they were recorded in.
func ReadSessionIndicesAny(ctx context.Context, path string, logger *zap.Logger, mode IndexMode) ([]string, error) {
	var indices []string

	dir, empty, err := openIndexDir(path, logger)
	if empty || err != nil {
		return indices, err
	}
	defer func() { _ = dir.Close() }()

	files, err := dir.ReadDir(0)
	if err != nil {
		return indices, err
	}

	seen := make(map[string]struct{})
	for _, v := range files {
		if v.Name() == FolderReports || v.Name() == FolderTestReports || v.Name() == FolderSchema {
			continue
		}
		// A leftover mock-record staging directory is not a test-set.
		if strings.HasSuffix(v.Name(), MockStagingSuffix) {
			continue
		}

		name := v.Name()

		switch mode {
		case ModeDir:
			isDir, err := listedDir(path, v)
			if err != nil {
				return nil, err
			}
			if isDir {
				if _, ok := seen[name]; !ok {
					seen[name] = struct{}{}
					indices = append(indices, name)
				}
			}
		case ModeFile:
			fileExt := filepath.Ext(name)
			if fileExt != ".yaml" && fileExt != ".json" {
				continue
			}
			base := name[:len(name)-len(fileExt)]
			if _, ok := seen[base]; ok {
				continue
			}
			seen[base] = struct{}{}
			indices = append(indices, base)
		}
	}

	return indices, nil
}

func DeleteFile(_ context.Context, logger *zap.Logger, path, name string) error {
	return DeleteFileF(nil, logger, path, name, FormatYAML)
}

func DeleteFileF(_ context.Context, logger *zap.Logger, path, name string, format Format) error {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	err := os.Remove(filePath)
	if err != nil {
		utils.LogError(logger, err, "failed to delete the file", zap.String("file", filePath))
		return fmt.Errorf("failed to delete the file: %v", err)
	}
	return nil
}

func DeleteDir(_ context.Context, logger *zap.Logger, path string) error {
	err := os.RemoveAll(path)
	if err != nil {
		utils.LogError(logger, err, "failed to delete the directory", zap.String("path", path))
		return fmt.Errorf("failed to delete the directory: %v", err)
	}
	return nil
}
