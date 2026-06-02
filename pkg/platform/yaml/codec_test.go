package yaml

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func testLogger() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

// buildHTTPDoc creates a NetworkTrafficDoc with an HTTP spec for testing.
func buildHTTPDoc() *NetworkTrafficDoc {
	doc := &NetworkTrafficDoc{
		Version: models.V1Beta1,
		Kind:    models.HTTP,
		Name:    "test-1",
		Curl:    "curl http://localhost:8080/api",
	}
	spec := models.HTTPSchema{
		Request: models.HTTPReq{
			Method: "GET",
			URL:    "http://localhost:8080/api",
			Header: map[string]string{"Content-Type": "application/json"},
			Body:   `{"key":"value"}`,
		},
		Response: models.HTTPResp{
			StatusCode:    200,
			StatusMessage: "OK",
			Header:        map[string]string{"Content-Type": "application/json"},
			Body:          `{"result":"ok"}`,
		},
		Created: 1700000000,
	}
	if err := doc.Spec.Encode(spec); err != nil {
		panic(err)
	}
	return doc
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		input    string
		expected Format
	}{
		{"json", FormatJSON},
		{"JSON", FormatJSON},
		{"yaml", FormatYAML},
		{"YAML", FormatYAML},
		{"", FormatYAML},
		{"xml", FormatYAML},
	}
	for _, tt := range tests {
		got := ParseFormat(tt.input)
		if got != tt.expected {
			t.Errorf("ParseFormat(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestFileExtension(t *testing.T) {
	if FormatYAML.FileExtension() != "yaml" {
		t.Error("FormatYAML.FileExtension() != yaml")
	}
	if FormatJSON.FileExtension() != "json" {
		t.Error("FormatJSON.FileExtension() != json")
	}
}

// TestMarshalDocJSON verifies MarshalDoc produces valid JSON.
func TestMarshalDocJSON(t *testing.T) {
	doc := buildHTTPDoc()
	data, err := MarshalDoc(FormatJSON, doc)
	if err != nil {
		t.Fatalf("MarshalDoc(JSON) error: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("MarshalDoc(JSON) produced invalid JSON: %s", data)
	}

	var parsed NetworkTrafficDocJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to re-parse JSON doc: %v", err)
	}
	if parsed.Version != models.V1Beta1 {
		t.Errorf("version = %q, want %q", parsed.Version, models.V1Beta1)
	}
	if parsed.Kind != models.HTTP {
		t.Errorf("kind = %q, want %q", parsed.Kind, models.HTTP)
	}
	if parsed.Name != "test-1" {
		t.Errorf("name = %q, want %q", parsed.Name, "test-1")
	}
	if len(parsed.Spec) == 0 {
		t.Error("spec is empty")
	}
}

// TestMarshalDocYAML verifies MarshalDoc still works for YAML.
func TestMarshalDocYAML(t *testing.T) {
	doc := buildHTTPDoc()
	data, err := MarshalDoc(FormatYAML, doc)
	if err != nil {
		t.Fatalf("MarshalDoc(YAML) error: %v", err)
	}
	var roundTrip NetworkTrafficDoc
	if err := yamlLib.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("Failed to re-parse YAML doc: %v", err)
	}
	if roundTrip.Name != "test-1" {
		t.Errorf("name = %q, want %q", roundTrip.Name, "test-1")
	}
}

// TestUnmarshalDocJSON verifies JSON bytes can be decoded to NetworkTrafficDoc.
func TestUnmarshalDocJSON(t *testing.T) {
	original := buildHTTPDoc()
	data, err := MarshalDoc(FormatJSON, original)
	if err != nil {
		t.Fatalf("MarshalDoc error: %v", err)
	}

	decoded, err := UnmarshalDoc(FormatJSON, data)
	if err != nil {
		t.Fatalf("UnmarshalDoc error: %v", err)
	}
	if decoded.Version != original.Version {
		t.Errorf("version = %q, want %q", decoded.Version, original.Version)
	}
	if decoded.Kind != original.Kind {
		t.Errorf("kind = %q, want %q", decoded.Kind, original.Kind)
	}
	if decoded.Name != original.Name {
		t.Errorf("name = %q, want %q", decoded.Name, original.Name)
	}
}

// TestJSONRoundTripHTTPSpec checks that HTTP spec data survives JSON round-trip.
func TestJSONRoundTripHTTPSpec(t *testing.T) {
	original := buildHTTPDoc()

	data, err := MarshalDoc(FormatJSON, original)
	if err != nil {
		t.Fatalf("MarshalDoc error: %v", err)
	}

	decoded, err := UnmarshalDoc(FormatJSON, data)
	if err != nil {
		t.Fatalf("UnmarshalDoc error: %v", err)
	}

	// Decode spec from the round-tripped document
	var httpSpec models.HTTPSchema
	if err := decoded.Spec.Decode(&httpSpec); err != nil {
		t.Fatalf("Spec.Decode error: %v", err)
	}

	if httpSpec.Request.URL != "http://localhost:8080/api" {
		t.Errorf("request URL = %q", httpSpec.Request.URL)
	}
	if string(httpSpec.Request.Method) != "GET" {
		t.Errorf("request method = %q", httpSpec.Request.Method)
	}
	if httpSpec.Response.StatusCode != 200 {
		t.Errorf("response status = %d", httpSpec.Response.StatusCode)
	}
	if httpSpec.Response.Body != `{"result":"ok"}` {
		t.Errorf("response body = %q", httpSpec.Response.Body)
	}
	if httpSpec.Created != 1700000000 {
		t.Errorf("created = %d, want 1700000000", httpSpec.Created)
	}
}

// TestMarshalGenericJSON tests MarshalGeneric with a struct that has json tags.
func TestMarshalGenericJSON(t *testing.T) {
	report := models.TestReport{
		Version: models.V1Beta1,
		Name:    "test-set-0-report",
		Status:  "PASSED",
		Success: 5,
		Total:   5,
		TestSet: "test-set-0",
	}
	data, err := MarshalGeneric(FormatJSON, &report)
	if err != nil {
		t.Fatalf("MarshalGeneric error: %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("invalid JSON: %s", data)
	}

	var decoded models.TestReport
	if err := UnmarshalGeneric(FormatJSON, data, &decoded); err != nil {
		t.Fatalf("UnmarshalGeneric error: %v", err)
	}
	if decoded.Name != report.Name {
		t.Errorf("name = %q, want %q", decoded.Name, report.Name)
	}
	if decoded.Success != 5 {
		t.Errorf("success = %d, want 5", decoded.Success)
	}
}

// TestMarshalGenericYAML tests MarshalGeneric roundtrip with YAML.
func TestMarshalGenericYAML(t *testing.T) {
	mapping := models.Mapping{
		Version:   string(models.V1Beta1),
		Kind:      models.MappingKind,
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{
			{ID: "test-1", Mocks: []models.MockEntry{{Name: "mock-0"}}},
		},
	}
	data, err := MarshalGeneric(FormatYAML, &mapping)
	if err != nil {
		t.Fatalf("MarshalGeneric YAML error: %v", err)
	}

	var decoded models.Mapping
	if err := UnmarshalGeneric(FormatYAML, data, &decoded); err != nil {
		t.Fatalf("UnmarshalGeneric YAML error: %v", err)
	}
	if decoded.TestSetID != "test-set-0" {
		t.Errorf("testSetID = %q, want test-set-0", decoded.TestSetID)
	}
}

// TestEncodeDocToJSONStreaming verifies EncodeDocTo writes NDJSON-compatible
// output: one compact JSON object followed by '\n', round-trippable via
// UnmarshalDoc(FormatJSON).
func TestEncodeDocToJSONStreaming(t *testing.T) {
	doc := buildHTTPDoc()

	var buf bytes.Buffer
	if err := EncodeDocTo(&buf, FormatJSON, doc); err != nil {
		t.Fatalf("EncodeDocTo(JSON): %v", err)
	}

	out := buf.Bytes()
	if len(out) == 0 || out[len(out)-1] != '\n' {
		t.Fatalf("JSON stream should end with '\\n' for NDJSON; got %q", out)
	}
	// A single-document NDJSON stream has exactly one newline at the end.
	if nl := bytes.Count(out, []byte{'\n'}); nl != 1 {
		t.Errorf("expected 1 newline in single-doc NDJSON, got %d", nl)
	}
	if !json.Valid(bytes.TrimRight(out, "\n")) {
		t.Fatalf("payload is not valid JSON: %s", out)
	}

	decoded, err := UnmarshalDoc(FormatJSON, bytes.TrimRight(out, "\n"))
	if err != nil {
		t.Fatalf("UnmarshalDoc: %v", err)
	}
	if decoded.Name != "test-1" || decoded.Kind != models.HTTP {
		t.Errorf("decoded doc = %+v", decoded)
	}
}

// TestEncodeDocToYAMLStreaming verifies EncodeDocTo writes a single YAML
// document that re-parses cleanly.
func TestEncodeDocToYAMLStreaming(t *testing.T) {
	doc := buildHTTPDoc()

	var buf bytes.Buffer
	if err := EncodeDocTo(&buf, FormatYAML, doc); err != nil {
		t.Fatalf("EncodeDocTo(YAML): %v", err)
	}

	var roundTrip NetworkTrafficDoc
	if err := yamlLib.Unmarshal(buf.Bytes(), &roundTrip); err != nil {
		t.Fatalf("yaml.Unmarshal of streamed output: %v", err)
	}
	if roundTrip.Name != "test-1" {
		t.Errorf("name = %q", roundTrip.Name)
	}
}

// TestMockReaderNDJSON tests reading NDJSON mock files.
func TestMockReaderNDJSON(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "mockreader_json_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Build two mock docs and write as NDJSON
	doc1 := buildHTTPDoc()
	doc1.Name = "mock-0"
	doc2 := buildHTTPDoc()
	doc2.Name = "mock-1"

	line1, err := MarshalDoc(FormatJSON, doc1)
	if err != nil {
		t.Fatalf("MarshalDoc 1: %v", err)
	}
	line2, err := MarshalDoc(FormatJSON, doc2)
	if err != nil {
		t.Fatalf("MarshalDoc 2: %v", err)
	}

	content := append(line1, '\n')
	content = append(content, line2...)
	content = append(content, '\n')

	mockFile := filepath.Join(tempDir, "mocks.json")
	if err := os.WriteFile(mockFile, content, 0644); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}

	reader, err := NewMockReaderF(ctx, logger, tempDir, "mocks", FormatJSON)
	if err != nil {
		t.Fatalf("NewMockReaderF: %v", err)
	}
	defer reader.Close()

	names := []string{}
	for {
		doc, err := reader.ReadNextDoc()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ReadNextDoc: %v", err)
		}
		names = append(names, doc.Name)
	}

	if len(names) != 2 {
		t.Fatalf("got %d docs, want 2", len(names))
	}
	if names[0] != "mock-0" || names[1] != "mock-1" {
		t.Errorf("names = %v, want [mock-0 mock-1]", names)
	}
}

// TestMockReaderNDJSONEmptyLines tests that empty lines in NDJSON are skipped.
func TestMockReaderNDJSONEmptyLines(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "mockreader_json_empty_test")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	doc := buildHTTPDoc()
	doc.Name = "mock-solo"
	line, err := MarshalDoc(FormatJSON, doc)
	if err != nil {
		t.Fatal(err)
	}

	// Write with leading/trailing empty lines
	content := []byte("\n\n")
	content = append(content, line...)
	content = append(content, []byte("\n\n\n")...)

	if err := os.WriteFile(filepath.Join(tempDir, "mocks.json"), content, 0644); err != nil {
		t.Fatal(err)
	}

	reader, err := NewMockReaderF(ctx, logger, tempDir, "mocks", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	doc1, err := reader.ReadNextDoc()
	if err != nil {
		t.Fatalf("ReadNextDoc: %v", err)
	}
	if doc1.Name != "mock-solo" {
		t.Errorf("name = %q, want mock-solo", doc1.Name)
	}

	_, err = reader.ReadNextDoc()
	if err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

// TestWriteFileReadFileJSON tests the format-aware file I/O round-trip.
func TestWriteFileReadFileJSON(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "writefile_json_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	content := []byte(`{"hello":"world"}`)
	if err := WriteFileF(ctx, logger, tempDir, "testfile", content, false, FormatJSON); err != nil {
		t.Fatalf("WriteFileF: %v", err)
	}

	// Verify file has .json extension
	if _, err := os.Stat(filepath.Join(tempDir, "testfile.json")); err != nil {
		t.Fatalf("Expected testfile.json to exist: %v", err)
	}

	data, err := ReadFileF(ctx, logger, tempDir, "testfile", FormatJSON)
	if err != nil {
		t.Fatalf("ReadFileF: %v", err)
	}
	if string(data) != `{"hello":"world"}` {
		t.Errorf("got %q", string(data))
	}
}

// TestWriteFileAppendNDJSON tests appending multiple documents in JSON format.
func TestWriteFileAppendNDJSON(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "appendfile_json_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	doc1 := []byte(`{"name":"mock-0"}`)
	doc2 := []byte(`{"name":"mock-1"}`)

	if err := WriteFileF(ctx, logger, tempDir, "mocks", doc1, true, FormatJSON); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileF(ctx, logger, tempDir, "mocks", doc2, true, FormatJSON); err != nil {
		t.Fatal(err)
	}

	data, err := ReadFileF(ctx, logger, tempDir, "mocks", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}

	// Should be NDJSON: first doc, newline, second doc
	lines := 0
	for _, b := range data {
		if b == '\n' {
			lines++
		}
	}
	// First write: just the data (empty file, no separator). Second write: \n + data.
	// So we expect at least 1 newline separator between docs.
	if lines < 1 {
		t.Errorf("expected at least 1 newline in NDJSON, got content: %q", string(data))
	}
}

// TestFileExistsF tests format-aware file existence check.
func TestFileExistsF(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "fileexists_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)
	logger := testLogger()

	// Create a .json file
	if err := os.WriteFile(filepath.Join(tempDir, "test.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	exists, err := FileExistsF(ctx, logger, tempDir, "test", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Error("expected test.json to exist")
	}

	exists, err = FileExistsF(ctx, logger, tempDir, "test", FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("expected test.yaml to NOT exist")
	}
}

// TestReadFileAnyPrefersPreferred verifies ReadFileAny picks the preferred
// format when both files exist.
func TestReadFileAnyPrefersPreferred(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "readfile_any_prefer_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	if err := os.WriteFile(filepath.Join(tempDir, "f.yaml"), []byte("y: 1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "f.json"), []byte(`{"j":1}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Prefer JSON -> should return .json contents
	data, detected, err := ReadFileAny(ctx, logger, tempDir, "f", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if detected != FormatJSON {
		t.Errorf("detected = %v, want FormatJSON", detected)
	}
	if string(data) != `{"j":1}` {
		t.Errorf("data = %q", data)
	}

	// Prefer YAML -> should return .yaml contents
	data, detected, err = ReadFileAny(ctx, logger, tempDir, "f", FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	if detected != FormatYAML {
		t.Errorf("detected = %v, want FormatYAML", detected)
	}
	if string(data) != "y: 1" {
		t.Errorf("data = %q", data)
	}
}

// TestReadFileAnyFallsBack verifies ReadFileAny falls back to the other
// format when the preferred one is missing — this is the backward-compat
// path for replay after a StorageFormat switch.
func TestReadFileAnyFallsBack(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "readfile_any_fallback_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Only a .yaml file exists — caller prefers JSON but must get YAML back.
	if err := os.WriteFile(filepath.Join(tempDir, "legacy.yaml"), []byte("hello: world"), 0644); err != nil {
		t.Fatal(err)
	}

	data, detected, err := ReadFileAny(ctx, logger, tempDir, "legacy", FormatJSON)
	if err != nil {
		t.Fatalf("expected fallback, got err: %v", err)
	}
	if detected != FormatYAML {
		t.Errorf("detected = %v, want FormatYAML (fallback)", detected)
	}
	if string(data) != "hello: world" {
		t.Errorf("data = %q", data)
	}
}

// TestReadFileAnyMissingBoth verifies ReadFileAny returns fs.ErrNotExist
// when neither format's file is present.
func TestReadFileAnyMissingBoth(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "readfile_any_missing_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	_, _, err = ReadFileAny(ctx, logger, tempDir, "nope", FormatJSON)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got %v", err)
	}
}

// TestFileExistsAnyDetection verifies FileExistsAny reports the correct
// detected format regardless of what the caller prefers.
func TestFileExistsAnyDetection(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "fileexists_any_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	if err := os.WriteFile(filepath.Join(tempDir, "m.yaml"), []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	// Caller prefers JSON; only YAML exists. Must still report exists=true
	// and detected=FormatYAML so writer can preserve the file's format.
	exists, detected, err := FileExistsAny(ctx, logger, tempDir, "m", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatal("expected exists=true")
	}
	if detected != FormatYAML {
		t.Errorf("detected = %v, want FormatYAML", detected)
	}
}

// TestNewMockReaderAnyFallsBack verifies NewMockReaderAny opens a YAML
// mocks file even when the caller prefers JSON (and vice versa).
func TestNewMockReaderAnyFallsBack(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "mockreader_any_fallback")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Write mocks.yaml (legacy format).
	doc := buildHTTPDoc()
	doc.Name = "mock-legacy"
	yamlBytes, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "mocks.yaml"), yamlBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// Caller prefers JSON but mocks.yaml is the only file present.
	// NewMockReaderAny must fall back to YAML and return a reader that
	// decodes YAML documents correctly.
	reader, err := NewMockReaderAny(ctx, logger, tempDir, "mocks", FormatJSON)
	if err != nil {
		t.Fatalf("NewMockReaderAny fallback: %v", err)
	}
	defer reader.Close()

	got, err := reader.ReadNextDoc()
	if err != nil {
		t.Fatalf("ReadNextDoc: %v", err)
	}
	if got.Name != "mock-legacy" {
		t.Errorf("name = %q, want mock-legacy", got.Name)
	}
	if _, err := reader.ReadNextDoc(); err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

// TestReadSessionIndicesAny verifies the format-agnostic scan accepts
// both .yaml and .json files and deduplicates by basename.
func TestReadSessionIndicesAny(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "sessionindices_any_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Mix of formats + one collision (test-2 exists in both formats).
	for _, name := range []string{
		"test-1.yaml",
		"test-2.yaml",
		"test-2.json",
		"test-3.json",
		"unrelated.txt",
	} {
		if err := os.WriteFile(filepath.Join(tempDir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := ReadSessionIndicesAny(ctx, tempDir, logger, ModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 distinct basenames (test-1, test-2, test-3), got %d: %v", len(got), got)
	}
}

// TestReadNextDocJSONReturnsRawMessageSpec verifies that on a JSON mocks
// file, ReadNextDocJSON returns a NetworkTrafficDocJSON whose Spec is still
// json.RawMessage — i.e. we've bypassed the yaml.Node bridge entirely on
// the JSON read hot path.
func TestReadNextDocJSONReturnsRawMessageSpec(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "mockreader_json_native_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Build two docs and write as NDJSON via the JSON write-side helper.
	doc := buildHTTPDoc()
	doc.Name = "mock-json-native"
	line, err := MarshalDoc(FormatJSON, doc)
	if err != nil {
		t.Fatal(err)
	}
	content := append(line, '\n')
	if err := os.WriteFile(filepath.Join(tempDir, "mocks.json"), content, 0644); err != nil {
		t.Fatal(err)
	}

	reader, err := NewMockReaderF(ctx, logger, tempDir, "mocks", FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	jd, err := reader.ReadNextDocJSON()
	if err != nil {
		t.Fatalf("ReadNextDocJSON: %v", err)
	}
	if jd.Name != "mock-json-native" {
		t.Errorf("name = %q", jd.Name)
	}
	if len(jd.Spec) == 0 {
		t.Fatalf("expected Spec to carry a json.RawMessage body, got empty")
	}
	if !json.Valid(jd.Spec) {
		t.Errorf("Spec is not valid JSON: %s", jd.Spec)
	}
	if _, err := reader.ReadNextDocJSON(); err != io.EOF {
		t.Errorf("expected EOF, got %v", err)
	}
}

// TestReadNextDocJSONRejectsYAMLFormat verifies that ReadNextDocJSON is a
// programming-error guard — it must error when called on a YAML reader so
// callers can't accidentally skip the yaml path.
func TestReadNextDocJSONRejectsYAMLFormat(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "mockreader_yaml_reject")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Write a valid YAML mocks file.
	doc := buildHTTPDoc()
	yamlBytes, err := yamlLib.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "mocks.yaml"), yamlBytes, 0644); err != nil {
		t.Fatal(err)
	}

	reader, err := NewMockReaderF(ctx, logger, tempDir, "mocks", FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if _, err := reader.ReadNextDocJSON(); err == nil {
		t.Fatal("expected error when ReadNextDocJSON is called on a YAML reader")
	}
}

// TestReadSessionIndicesF tests format-aware file listing.
func TestReadSessionIndicesF(t *testing.T) {
	ctx := context.Background()
	logger := testLogger()

	tempDir, err := os.MkdirTemp("", "sessionindices_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	// Create mixed files
	for _, name := range []string{"test-1.yaml", "test-2.json", "test-3.json", "mocks.yaml"} {
		if err := os.WriteFile(filepath.Join(tempDir, name), []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// JSON mode should only see .json files
	jsonIndices, err := ReadSessionIndicesF(ctx, tempDir, logger, ModeFile, FormatJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(jsonIndices) != 2 {
		t.Errorf("JSON indices count = %d, want 2, got %v", len(jsonIndices), jsonIndices)
	}

	// YAML mode should only see .yaml files
	yamlIndices, err := ReadSessionIndicesF(ctx, tempDir, logger, ModeFile, FormatYAML)
	if err != nil {
		t.Fatal(err)
	}
	if len(yamlIndices) != 2 {
		t.Errorf("YAML indices count = %d, want 2, got %v", len(yamlIndices), yamlIndices)
	}
}
