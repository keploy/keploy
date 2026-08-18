package yaml

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"go.keploy.io/server/v3/pkg/models"
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
	isFileEmpty, err := CreateFileF(ctx, logger, path, fileName, format)
	if err != nil {
		utils.LogError(logger, err, "failed to create file", zap.String("path directory", path), zap.String("file", fileName))
		return err
	}
	filePath := filepath.Join(path, fileName+"."+format.FileExtension())

	if isAppend {
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
	// partial document. Mirrors mockdb.writeMocksAtomically / testdb.upsert.
	tmpFile, err := os.CreateTemp(path, fileName+".*.tmp")
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
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(filePath); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err = os.Chmod(tmpPath, mode); err != nil {
		return err
	}
	if err = atomicReplaceFile(tmpPath, filePath); err != nil {
		utils.LogError(logger, err, "failed to atomically replace the file", zap.String("file", filePath))
		return err
	}
	cleanup = false
	return nil
}

// atomicReplaceFile renames src over dst. POSIX rename atomically replaces an
// existing target; on Windows rename fails when dst already exists, so fall back
// to remove-then-rename. Mirrors mockdb.replaceFile so reports/config/mappings
// get the same crash- and reader-safe replace the mock/testcase writers have.
func atomicReplaceFile(src, dst string) error {
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

func CreateYamlFile(ctx context.Context, Logger *zap.Logger, path string, fileName string) (bool, error) {
	return CreateFileF(ctx, Logger, path, fileName, FormatYAML)
}

func CreateFileF(ctx context.Context, Logger *zap.Logger, path string, fileName string, format Format) (bool, error) {
	filePath, err := ValidatePath(filepath.Join(path, fileName+"."+format.FileExtension()))
	if err != nil {
		utils.LogError(Logger, err, "failed to validate the file path", zap.String("path directory", path), zap.String("file", fileName))
		return false, err
	}

	if _, err := os.Stat(filePath); err != nil {
		if !os.IsNotExist(err) {
			utils.LogError(Logger, err,
				"failed to stat file — check filesystem permissions and that the configured keploy path is readable/writable by this process",
				zap.String("path directory", path),
				zap.String("file", fileName))
			return false, err
		}
		// Honour context cancellation/deadline before doing any
		// filesystem mutations. Previously the check was
		// `ctx.Err() == nil || ctx.Err() == context.Canceled`,
		// which both let cancelled contexts proceed (defeating the
		// cancellation contract) and masked DeadlineExceeded by
		// returning the surrounding os.Stat error instead of
		// ctx.Err().
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		// 0o755/0o644 rather than the historical 0o777 — nothing
		// in the keploy tree needs world-writable perms and the
		// stricter modes match the rest of the new testdb code.
		if err := os.MkdirAll(filepath.Join(path), 0o755); err != nil {
			utils.LogError(Logger, err, "failed to create a directory for the file", zap.String("path directory", path), zap.String("file", fileName))
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
	return false, nil
}

func ReadSessionIndices(ctx context.Context, path string, logger *zap.Logger, mode IndexMode) ([]string, error) {
	return ReadSessionIndicesF(ctx, path, logger, mode, FormatYAML)
}

func ReadSessionIndicesF(ctx context.Context, path string, logger *zap.Logger, mode IndexMode, format Format) ([]string, error) {
	var indices []string

	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return indices, nil
	}

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

		name := v.Name()

		switch mode {
		case ModeDir:
			if v.IsDir() {
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

	dir, err := ReadDir(path, fs.FileMode(os.O_RDONLY))
	if err != nil {
		logger.Debug("creating a folder for the keploy generated testcases", zap.Error(err))
		return indices, nil
	}

	files, err := dir.ReadDir(0)
	if err != nil {
		return indices, err
	}

	seen := make(map[string]struct{})
	for _, v := range files {
		if v.Name() == FolderReports || v.Name() == FolderTestReports || v.Name() == FolderSchema {
			continue
		}

		name := v.Name()

		switch mode {
		case ModeDir:
			if v.IsDir() {
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
