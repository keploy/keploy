package mockdb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/utils"
	yamlLib "gopkg.in/yaml.v3"
)

// A recording that rolls over to a new test set (record.Roller) must leave
// every set replayable on its own: a test's mapping names only mocks of its
// own set. The mocks a set needs from another -- the session and connection
// state a later set inherits, a test in flight at the cut whose later calls
// were filed in the next set -- are copied into it, the document byte for
// byte, under the same name. These two calls are that copy: ReadMockDocs takes
// documents out of one set's mock file by name, AppendMockDocs adds documents
// to another's. Neither decodes a mock.

// ErrMockDocsGob is returned for a test set whose mocks are in a gob file:
// a gob file holds no documents to copy.
var ErrMockDocsGob = errors.New("the test set's mocks are in a gob file, which holds no documents to copy")

// ReadMockDocs returns the documents of testSetID's mock file whose top-level
// name is in names, each as the file holds it, and the file's format
// ("yaml" or "json"). A name the file does not hold is not in the result; a
// document the file holds only in part (one still being appended) is not
// either. It reads the file once, front to back, and stops once it has every
// name; only the documents it returns are kept.
func (ys *MockYaml) ReadMockDocs(ctx context.Context, testSetID string, names map[string]struct{}) (map[string][]byte, string, error) {
	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.mockFileName()
	if _, err := os.Stat(filepath.Join(mockPath, mockFileName+".gob")); err == nil {
		return nil, "", fmt.Errorf("%w: %s", ErrMockDocsGob, testSetID)
	}
	if len(names) == 0 {
		return map[string][]byte{}, "", nil
	}
	reader, err := yaml.NewMockReaderAny(ctx, ys.Logger, mockPath, mockFileName, ys.Format)
	if err != nil {
		return nil, "", fmt.Errorf("open the mock file of test set %s to copy mocks out of it: %w", testSetID, err)
	}
	// A read-only file: its close has nothing to report.
	defer func() { _ = reader.Close() }()
	format := reader.Format()
	out := make(map[string][]byte, len(names))
	for len(out) < len(names) {
		doc, err := reader.ReadNextDocument()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read the mock file of test set %s to copy mocks out of it: %w", testSetID, err)
		}
		if reader.Unterminated() {
			// The file's last document, still being written: no
			// document of it is whole yet.
			break
		}
		name := mockDocName(doc, format)
		if _, want := names[name]; !want {
			continue
		}
		if _, dup := out[name]; dup {
			continue
		}
		out[name] = doc
	}
	return out, format.FileExtension(), nil
}

// AppendMockDocs appends docs, documents of format ("yaml" or "json") as
// ReadMockDocs returns them or the mock store wrote them (models.HandMockDoc),
// to testSetID's mock file, in order, and returns the bytes it added. The file
// takes them as InsertMock takes a mock: under the same lock, with the same
// separator, and created (with keploy's version comment) if the set has none.
// A set whose mock file is of another format, or a gob file, is refused.
func (ys *MockYaml) AppendMockDocs(ctx context.Context, testSetID, format string, docs [][]byte) (int64, error) {
	if len(docs) == 0 {
		return 0, nil
	}
	mockPath := filepath.Join(ys.MockPath, testSetID)
	mockFileName := ys.mockFileName()
	useGob, effFormat, err := ys.insertFormat(ctx, mockPath, mockFileName)
	if err != nil {
		return 0, err
	}
	if useGob {
		return 0, fmt.Errorf("%w: %s", ErrMockDocsGob, testSetID)
	}
	if effFormat.FileExtension() != format {
		return 0, fmt.Errorf("the mocks to copy into test set %s are %s documents, and its mock file is %s", testSetID, format, effFormat.FileExtension())
	}
	lock := getMockFileLock(mockFileLockKey(mockPath, mockFileName, effFormat))
	lock.Lock()
	defer lock.Unlock()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	isFileEmpty, err := yaml.CreateRecordingFileF(ctx, ys.Logger, ys.MockPath, mockPath, mockFileName, effFormat)
	if err != nil {
		return 0, err
	}
	filePath := filepath.Join(mockPath, mockFileName+"."+effFormat.FileExtension())
	file, err := ys.openMockFile(filePath, os.O_WRONLY|os.O_APPEND, os.ModePerm)
	if err != nil {
		return 0, fmt.Errorf("failed to open mock file for append: %w", err)
	}
	w := bufio.NewWriter(file)
	var n int64
	write := func(b []byte) error {
		k, err := w.Write(b)
		n += int64(k)
		return err
	}
	for _, doc := range docs {
		if effFormat == yaml.FormatJSON {
			if err = write(bytes.TrimRight(doc, "\n")); err == nil {
				err = write([]byte{'\n'})
			}
		} else {
			if isFileEmpty {
				if v := utils.GetVersionAsComment(); v != "" {
					err = write([]byte(v))
				}
			} else {
				err = write([]byte("---\n"))
			}
			if err == nil {
				err = write(doc)
			}
			if err == nil && !bytes.HasSuffix(doc, []byte{'\n'}) {
				err = write([]byte{'\n'})
			}
		}
		if err != nil {
			break
		}
		isFileEmpty = false
	}
	if ferr := w.Flush(); err == nil {
		err = ferr
	}
	if cerr := closeMockFile(file); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("failed to append copied mocks to test set %s: %w", testSetID, err)
	}
	return n, nil
}

func (ys *MockYaml) mockFileName() string {
	if ys.MockName == "" {
		return "mocks"
	}
	return ys.MockName
}

// mockDocName is the top-level name of a mock document, "" when it has none
// that can be read. It reads only the document's head: keploy writes a mock's
// version, kind and name before its spec.
func mockDocName(doc []byte, format yaml.Format) string {
	if format == yaml.FormatJSON {
		dec := json.NewDecoder(bytes.NewReader(doc))
		if t, err := dec.Token(); err != nil || t != json.Delim('{') {
			return ""
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return ""
			}
			if key == "name" {
				var name string
				if dec.Decode(&name) != nil {
					return ""
				}
				return name
			}
			var skip json.RawMessage
			if dec.Decode(&skip) != nil {
				return ""
			}
		}
		return ""
	}
	for rest := doc; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		switch {
		case bytes.HasPrefix(line, []byte("name:")):
			var head struct {
				Name string `yaml:"name"`
			}
			if yamlLib.Unmarshal(line, &head) != nil {
				return ""
			}
			return head.Name
		case bytes.HasPrefix(line, []byte("spec:")):
			return ""
		}
	}
	return ""
}
