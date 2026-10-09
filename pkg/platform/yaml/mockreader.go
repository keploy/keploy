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
	// unterminated is whether the document last read ended at the end of
	// the file part-way through a line (see Unterminated).
	unterminated bool
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
	r.unterminated = false
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
					r.unterminated = true
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

		// At io.EOF, line is the file's last line when the file does not end
		// with a newline (and "" when it does). That line is still part of
		// the document, so it is handled like any other before EOF ends it.
		line, err := r.reader.ReadString('\n')
		r.lineNum++
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read line %d: %w", r.lineNum, err)
		}
		atEOF := err == io.EOF

		trimmedLine := strings.TrimSpace(line)

		if trimmedLine == "---" {
			if buffer.Len() == 0 {
				continue
			}
			return buffer.Bytes(), nil
		}

		if !(isFirstDoc && buffer.Len() == 0 && strings.HasPrefix(trimmedLine, "#")) {
			buffer.WriteString(line)
			r.unterminated = atEOF && line != ""
		}

		if atEOF {
			r.done = true
			if buffer.Len() > 0 {
				return buffer.Bytes(), nil
			}
			return nil, io.EOF
		}
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
	doc, _, err := r.ReadNextDocBytes()
	return doc, err
}

// ReadNextDocBytes is ReadNextDoc that also returns the document's bytes as
// they were read (a YAML document without its "---" separator, or one NDJSON
// line without its newline), for a caller that has to write a document back
// unchanged. The last YAML document of a file that does not end with a
// newline ends without one. A document that does not parse is returned as
// read, with the error.
func (r *MockReader) ReadNextDocBytes() (*NetworkTrafficDoc, []byte, error) {
	data, err := r.ReadNextDocument()
	if err != nil {
		return nil, nil, err
	}

	if len(bytes.TrimSpace(data)) == 0 {
		return r.ReadNextDocBytes()
	}

	if r.format == FormatJSON {
		doc, err := UnmarshalDoc(FormatJSON, data)
		if err != nil {
			return nil, data, fmt.Errorf("failed to decode JSON at line %d: %w", r.lineNum, err)
		}
		return doc, data, nil
	}

	var doc NetworkTrafficDoc
	if err := yamlLib.Unmarshal(data, &doc); err != nil {
		return nil, data, fmt.Errorf("failed to decode YAML at line %d: %w", r.lineNum, err)
	}

	return &doc, data, nil
}

// ReadNextDocJSON reads the next NDJSON line and returns it as a
// NetworkTrafficDocJSON with the spec kept as a json.RawMessage. Only valid
// when the reader's format is JSON; panics (via error) otherwise.
//
// This is the read-side companion to EncodeMockJSON: the entire round-trip
// for a JSON mocks file can now stay on encoding/json with no yaml.Node
// allocation or gopkg.in/yaml.v3 emit/parse.
func (r *MockReader) ReadNextDocJSON() (*NetworkTrafficDocJSON, error) {
	doc, _, err := r.ReadNextDocJSONBytes()
	return doc, err
}

// ReadNextDocJSONBytes is ReadNextDocJSON that also returns the line as it
// was read, without its newline, also when it does not parse (see
// ReadNextDocBytes).
func (r *MockReader) ReadNextDocJSONBytes() (*NetworkTrafficDocJSON, []byte, error) {
	if r.format != FormatJSON {
		return nil, nil, fmt.Errorf("mockreader: ReadNextDocJSON called on %s reader", r.format)
	}
	data, err := r.ReadNextDocument()
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return r.ReadNextDocJSONBytes()
	}
	var doc NetworkTrafficDocJSON
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, data, fmt.Errorf("failed to decode JSON at line %d: %w", r.lineNum, err)
	}
	return &doc, data, nil
}

// Unterminated reports whether the document last read is the file's last and
// its last line has no newline: the file ends part-way through a line, as a
// write that was interrupted (the recorder killed, the disk full) leaves it, or
// was saved without a final newline. Such a document may be incomplete.
func (r *MockReader) Unterminated() bool {
	return r.unterminated
}

// Path returns the path of the file the reader reads.
func (r *MockReader) Path() string {
	return r.path
}

// Close closes the file.
func (r *MockReader) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
