package yaml

import (
	"bufio"
	"bytes"
	"context"
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

		trimmedLine := strings.TrimSpace(line)

		if trimmedLine == "---" {
			if buffer.Len() == 0 {
				continue
			}
			return buffer.Bytes(), nil
		}

		if isFirstDoc && buffer.Len() == 0 && strings.HasPrefix(trimmedLine, "#") {
			continue
		}

		buffer.WriteString(line)
	}
}

// ReadNextDoc reads and decodes the next document.
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

// Close closes the file.
func (r *MockReader) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
