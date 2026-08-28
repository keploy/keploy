package yaml

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestMockReaderIndentedYAMLSeparator guards against treating an indented
// '---' inside a block scalar as a document separator. A YAML document
// separator is only a separator at column 0; keploy writes prompts and LLM
// bodies that contain indented '---' lines, so the reader must not split on
// them (issue #4477).
func TestMockReaderIndentedYAMLSeparator(t *testing.T) {
	ctx := context.Background()
	logger := getTestLogger()

	tempDir, err := os.MkdirTemp("", "mockreader_indented_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// The first document ends with a block scalar whose body contains an
	// indented '---' (a prompt), followed by a real column-0 separator.
	content := "version: api.keploy.io/v1beta1\n" +
		"kind: Http\n" +
		"name: mock-with-indented-separator\n" +
		"spec:\n" +
		"    req:\n" +
		"        method: POST\n" +
		"        body: |+\n" +
		"            you are a helpful assistant\n" +
		"            ---\n" +
		"            llm prompt body with an indented separator\n" +
		"---\n" +
		"version: api.keploy.io/v1beta1\n" +
		"kind: Http\n" +
		"name: mock-2\n" +
		"spec:\n" +
		"    req:\n" +
		"        method: GET\n"

	if err := os.WriteFile(filepath.Join(tempDir, "mocks.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write mocks.yaml: %v", err)
	}

	reader, err := NewMockReaderF(ctx, logger, tempDir, "mocks", FormatYAML)
	if err != nil {
		t.Fatalf("NewMockReaderF: %v", err)
	}

	var names []string
	for {
		doc, err := reader.ReadNextDoc()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Failed to read mock: %v", err)
		}
		names = append(names, doc.Name)
	}

	if len(names) != 2 {
		t.Fatalf("Expected 2 documents (indented '---' must stay inside the block scalar), got %d: %v", len(names), names)
	}
	if names[0] != "mock-with-indented-separator" || names[1] != "mock-2" {
		t.Fatalf("Unexpected document names: %v", names)
	}
}