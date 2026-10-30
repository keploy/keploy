package mockdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// MockFileDoc is one document of a set's mock file as a replay of the set
// reads it (GetTestSetMocks): read, parsed and decoded by the same code, so
// what a caller says of a document is what the replay does with it.
type MockFileDoc struct {
	// Name and Kind are the document's own when it parses. Of a YAML
	// document cut short (MockSkipCutShort) they are read from its whole
	// lines; of an NDJSON line cut short, from the line when its JSON parses.
	// Of a document that does not parse they are empty.
	Name string
	Kind models.Kind
	// Doc is a YAML file's document as parsed (of one cut short, its whole
	// lines); nil for an NDJSON file, or when it does not parse.
	Doc *yaml.NetworkTrafficDoc
	// Raw is the document as read, and Offset is where in the file it
	// starts. Len is how many of Raw's bytes are the document: all of them
	// but for a document before an unfinished separator, whose separator
	// line is not its. Of a document cut short the replay decodes none.
	Raw    []byte
	Offset int64
	Len    int
	// Mocks is what the replay decodes the document to: none when it skips
	// the document (Skip) or cannot read it (Err).
	Mocks []*models.Mock
	// Skip says why the replay serves no mock from the document; "" when it
	// does, or cannot read it (Err).
	Skip models.MockSkip
	// Cause is, of an NDJSON line cut short whose JSON parses, why its spec
	// does not decode; nil otherwise.
	Cause error
	// Err is why the replay cannot read the document, which fails the read of
	// the whole set: it does not parse, or does not decode. MockFileReader
	// reads on past it.
	Err error
}

// MockFileReader reads a set's YAML or NDJSON mock file document by
// document, as a replay of the set does. Unlike the replay, which fails the
// set at the first document it cannot read, it reads on and returns each
// (MockFileDoc.Err), so a caller can report every document of a hand-edited
// file the replay would refuse or skip.
type MockFileReader struct {
	r      *yaml.MockReader
	logger *zap.Logger
}

// ErrGobMockFile is NewMockFileReader's error for a set whose mocks are in
// a gob file (name.gob), which a replay reads before a YAML or NDJSON one
// and which has no documents: the reader does not read another file the
// replay never reads.
var ErrGobMockFile = errors.New("a replay reads the set's mocks from its gob mock file, which has no documents")

// NewMockFileReader opens the mock file name (no extension) in dir that a
// replay of the set reads, with preferred the configured format: the file
// in that format, else the one in the other (yaml.NewMockReaderAny). When
// dir has name.gob, which a replay reads before either, the error wraps
// ErrGobMockFile and names it (see MockYaml.SkippedMocks for what a replay
// skips of one). logger takes what the replay's read logs for each document
// it skips; nil discards it. Otherwise the error is the open's (wrapping
// fs.ErrNotExist when there is no such file).
func NewMockFileReader(ctx context.Context, logger *zap.Logger, dir, name string, preferred yaml.Format) (*MockFileReader, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if gobPath := filepath.Join(dir, name+".gob"); gobFileReadFirst(gobPath) {
		return nil, fmt.Errorf("%w: %s", ErrGobMockFile, gobPath)
	}
	r, err := yaml.NewMockReaderAny(ctx, logger, dir, name, preferred)
	if err != nil {
		return nil, err
	}
	return &MockFileReader{r: r, logger: logger}, nil
}

// Next is the file's next document; the error is io.EOF after the last, or
// the error reading the file (not a document's: see MockFileDoc.Err). The
// start of a separator the file ends in, which is no document, is not
// returned.
func (fr *MockFileReader) Next() (MockFileDoc, error) {
	for {
		d, readErr, decodeErr := nextDoc(fr.r, fr.logger, false)
		if d.raw == nil {
			if readErr == nil {
				readErr = io.EOF
			}
			return MockFileDoc{}, readErr
		}
		out := MockFileDoc{Name: d.name, Kind: d.kind, Doc: d.doc, Raw: d.raw, Offset: d.off, Len: d.n, Mocks: d.mocks}
		switch {
		case readErr != nil && d.parseErr != nil:
			out.Err = fmt.Errorf("%w: %w", readErr, d.parseErr)
		case readErr != nil:
			out.Err = readErr
		case decodeErr != nil:
			out.Err = decodeErr
		case d.incomplete:
			out.Skip, out.Cause = models.MockSkipCutShort, d.cutErr
		case d.separator:
			continue // the start of a separator, ending the file: no document
		case len(d.mocks) == 0:
			out.Skip = skipReason(d.kind, fr.r.Format() == yaml.FormatJSON)
		}
		return out, nil
	}
}

// skipReason is why the decoders (decodeMocks, decodeMocksJSON) took no mock
// from a document of kind that parsed and decoded without error: they skip
// only a document with no kind, a kind they have no decoder for, and a
// connection failure they cannot replay. Of an NDJSON document (json), a
// kind with a "-" in it, or one a registered mapper decodes, is one
// decodeMocksJSON skips in every build: only the YAML decoder reads it, and
// a "-" kind only with keploy enterprise's mapper.
func skipReason(kind models.Kind, json bool) models.MockSkip {
	switch {
	case kind == "":
		return models.MockSkipNoKind
	case kind == models.ConnectionFailure:
		return models.MockSkipConnFailure
	case json && (strings.Contains(string(kind), "-") || hasMapperForKind(kind)):
		return models.MockSkipYAMLOnlyKind
	}
	return models.MockSkipUnknownKind
}

// Format is the format of the file it opened.
func (fr *MockFileReader) Format() yaml.Format { return fr.r.Format() }

// Path is the mock file's path.
func (fr *MockFileReader) Path() string { return fr.r.Path() }

// Close closes the file.
func (fr *MockFileReader) Close() error { return fr.r.Close() }

// SkippedMocks is the mock file of the set testSetID that a replay reads
// (GetTestSetMocks: a mocks.gob first, else the YAML or NDJSON one), and by
// name each named document of it the read skips, with why: of two such
// documents with one name, the last, as TestSetMocks.Skipped lists it. A gob
// file skips only a connection failure this keploy cannot replay. With no
// mock file, "" and none; nil when the file cannot be read.
func (ys *MockYaml) SkippedMocks(ctx context.Context, testSetID string) (string, map[string]models.SkippedMock) {
	mockFileName := ys.MockName
	if mockFileName == "" {
		mockFileName = "mocks"
	}
	path := filepath.Join(ys.MockPath, testSetID)
	lock := getMockFileLock(mockFileLockKey(path, mockFileName, ys.Format))
	lock.RLock()
	defer lock.RUnlock()
	out := map[string]models.SkippedMock{}
	if gobPath := filepath.Join(path, mockFileName+".gob"); gobFileReadFirst(gobPath) {
		mocks, _, err := readGobMocksCut(gobPath)
		if err != nil {
			return gobPath, nil
		}
		for _, mock := range mocks {
			if mock.Kind == models.ConnectionFailure && mock.Name != "" && !connFailureSupported(mock, zap.NewNop(), false) {
				out[mock.Name] = models.SkippedMock{Name: mock.Name, Kind: mock.Kind, Why: models.MockSkipConnFailure}
			}
		}
		return gobPath, out
	}
	fr, err := NewMockFileReader(ctx, nil, path, mockFileName, ys.Format)
	if errors.Is(err, fs.ErrNotExist) {
		return "", out // no mock file: the read skips nothing
	}
	if errors.Is(err, ErrGobMockFile) {
		return filepath.Join(path, mockFileName+".gob"), nil // written since the check above
	}
	if err != nil {
		return mockFileTried(path, mockFileName, ys.Format), nil
	}
	defer func() { _ = fr.Close() }()
	for {
		d, err := fr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return fr.Path(), nil
			}
			return fr.Path(), out
		}
		if d.Skip != "" && d.Name != "" {
			sk := models.SkippedMock{Name: d.Name, Kind: d.Kind, Why: d.Skip}
			if d.Cause != nil {
				sk.Cause = d.Cause.Error()
			}
			out[d.Name] = sk
		}
	}
}

// mockFileTried is the YAML or NDJSON file of the set's mocks in dir that
// yaml.NewMockReaderAny opens, or fails to: the first of the preferred
// format's and the other's that its stat does not say is missing.
func mockFileTried(dir, name string, preferred yaml.Format) string {
	other := yaml.FormatJSON
	if preferred == yaml.FormatJSON {
		other = yaml.FormatYAML
	}
	for _, f := range [2]yaml.Format{preferred, other} {
		p := filepath.Join(dir, name+"."+f.FileExtension())
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			return p
		}
	}
	return ""
}

// gobFileReadFirst says a replay of the set reads the gob mock file at path
// rather than a YAML or NDJSON one, as readMockPools decides it: the path
// stats.
func gobFileReadFirst(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
