package yaml

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// MockReader provides line-by-line reading with "---" as the document delimiter.
// It reads line by line and accumulates until delimiter for memory-efficient streaming.
// For JSON format, it reads NDJSON (one JSON object per line).
type MockReader struct {
	file    *os.File
	reader  *bufio.Reader
	ctx     context.Context
	logger  *zap.Logger
	path    string
	lineNum int
	done    bool
	format  Format
}

// NewMockReader creates a reader that accumulates lines until "---" delimiter.
func NewMockReader(ctx context.Context, logger *zap.Logger, path, name string) (*MockReader, error) {
	return NewMockReaderF(ctx, logger, path, name, FormatYAML)
}

func NewMockReaderF(ctx context.Context, logger *zap.Logger, path, name string, format Format) (*MockReader, error) {
	filePath := filepath.Join(path, name+"."+format.FileExtension())
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open mock file: %w", err)
	}

	return &MockReader{
		file:    file,
		reader:  bufio.NewReader(file),
		ctx:     ctx,
		logger:  logger,
		path:    filePath,
		lineNum: 0,
		done:    false,
		format:  format,
	}, nil
}

// NewMockReaderAny opens a mock file, preferring `preferred`'s extension but
// falling back to the other format if only that variant exists on disk. The
// returned reader is configured to decode the actual file's format, so
// callers that have mocks still recorded as YAML keep working even when
// StorageFormat is set to json (and vice versa).
func NewMockReaderAny(ctx context.Context, logger *zap.Logger, path, name string, preferred Format) (*MockReader, error) {
	other := preferred
	if preferred == FormatJSON {
		other = FormatYAML
	} else {
		other = FormatJSON
	}
	for _, f := range [2]Format{preferred, other} {
		filePath := filepath.Join(path, name+"."+f.FileExtension())
		if _, statErr := os.Stat(filePath); statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, statErr
		}
		return NewMockReaderF(ctx, logger, path, name, f)
	}
	return nil, fmt.Errorf("failed to open mock file: %w", os.ErrNotExist)
}

// ReadNextDocument reads lines until it encounters "---" or EOF (YAML),
// or reads one line for NDJSON (JSON format).
func (r *MockReader) ReadNextDocument() ([]byte, error) {
	if r.done {
		return nil, io.EOF
	}

	if r.format == FormatJSON {
		return r.readNextJSONLine()
	}
	return r.readNextYAMLDocument()
}

func (r *MockReader) readNextJSONLine() ([]byte, error) {
	for {
		select {
		case <-r.ctx.Done():
			return nil, r.ctx.Err()
		default:
		}

		line, err := r.reader.ReadString('\n')
		r.lineNum++

		if err != nil {
			if err == io.EOF {
				r.done = true
				trimmed := strings.TrimSpace(line)
				if len(trimmed) > 0 {
					return []byte(trimmed), nil
				}
				return nil, io.EOF
			}
			return nil, fmt.Errorf("failed to read line %d: %w", r.lineNum, err)
		}

		trimmed := strings.TrimSpace(line)
		if len(trimmed) == 0 {
			continue // skip empty lines
		}
		return []byte(trimmed), nil
	}
}

func (r *MockReader) readNextYAMLDocument() ([]byte, error) {
	var buffer bytes.Buffer
	isFirstDoc := r.lineNum == 0

	for {
		select {
		case <-r.ctx.Done():
			return nil, r.ctx.Err()
		default:
		}

		line, err := r.reader.ReadString('\n')
		r.lineNum++

		if err != nil {
			if err == io.EOF {
				r.done = true
				if buffer.Len() > 0 {
					return buffer.Bytes(), nil
				}
				return nil, io.EOF
			}
			return nil, fmt.Errorf("failed to read line %d: %w", r.lineNum, err)
		}

		// A YAML document separator only separates at column 0. Anchoring the
		// check with TrimSpace also stripped LEADING whitespace, so an indented
		// "---" inside a block scalar -- e.g. a recorded request body that
		// happens to contain markdown, YAML, or front matter -- was read as the
		// start of a new document. The reader then handed the parser a fragment
		// cut mid-scalar and the whole file failed to load, which meant keploy
		// could not read back a mocks.yaml it had itself written.
		//
		// Only trailing whitespace and the line ending may be stripped here.
		separatorCandidate := strings.TrimRight(line, " \t\r\n")

		if separatorCandidate == "---" {
			if buffer.Len() == 0 {
				continue
			}
			return buffer.Bytes(), nil
		}

		// The leading-comment skip genuinely wants a leading-trimmed value, so
		// it keeps its own: an indented comment above the first document is
		// still a comment, whereas an indented "---" is not a separator.
		if isFirstDoc && buffer.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		buffer.WriteString(line)
	}
}

// Format returns the format the reader is decoding. Callers can use this to
// pick between ReadNextDoc (NetworkTrafficDoc with yaml.Node spec) and
// ReadNextDocJSON (NetworkTrafficDocJSON with json.RawMessage spec) and thus
// skip the yaml.Node bridge on the JSON read path.
func (r *MockReader) Format() Format {
	return r.format
}

// ReadNextDoc reads and decodes the next document.
//
// For JSON files this still goes through the yaml.Node bridge (via
// UnmarshalDoc -> jsonDocToYamlDoc) so callers that rely on
// NetworkTrafficDoc.Spec.Decode(&concreteSpec) keep working. Hot paths that
// want to stay yaml-free on JSON files should use ReadNextDocJSON instead.
func (r *MockReader) ReadNextDoc() (*NetworkTrafficDoc, error) {
	data, err := r.ReadNextDocument()
	if err != nil {
		return nil, err
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return r.ReadNextDoc()
	}

	if r.format == FormatJSON {
		doc, err := UnmarshalDoc(FormatJSON, data)
		if err != nil {
			return nil, fmt.Errorf("failed to decode JSON at line %d: %w", r.lineNum, err)
		}
		return doc, nil
	}

	var doc NetworkTrafficDoc
	if err := yamlLib.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to decode YAML at line %d: %w", r.lineNum, err)
	}

	return &doc, nil
}

// ReadNextDocJSON reads the next NDJSON line and returns it as a
// NetworkTrafficDocJSON with the spec kept as a json.RawMessage. Only valid
// when the reader's format is JSON; panics (via error) otherwise.
//
// This is the read-side companion to EncodeMockJSON: the entire round-trip
// for a JSON mocks file can now stay on encoding/json with no yaml.Node
// allocation or gopkg.in/yaml.v3 emit/parse.
func (r *MockReader) ReadNextDocJSON() (*NetworkTrafficDocJSON, error) {
	if r.format != FormatJSON {
		return nil, fmt.Errorf("mockreader: ReadNextDocJSON called on %s reader", r.format)
	}
	data, err := r.ReadNextDocument()
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return r.ReadNextDocJSON()
	}
	var doc NetworkTrafficDocJSON
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to decode JSON at line %d: %w", r.lineNum, err)
	}
	return &doc, nil
}

// Close closes the file.
func (r *MockReader) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
