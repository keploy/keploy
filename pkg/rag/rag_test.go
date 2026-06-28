package rag

import (
	"context"
	"io/ioutil"
	"os"
	"testing"
)

func TestRAGSystem(t *testing.T) {
	// Skip if services are not running
	if os.Getenv("SKIP_INTEGRATION_TESTS") != "" {
		t.Skip("Skipping integration tests")
	}

	// Create test file
	testCode := `package main

func TestFunction() error {
    return nil
}

func AnotherTestFunction(input string) (string, error) {
    if input == "" {
        return "", fmt.Errorf("empty input")
    }
    return input, nil
}`

	testFile := "test_temp.go"
	err := ioutil.WriteFile(testFile, []byte(testCode), 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}
	defer os.Remove(testFile)

	// Initialize RAG system
	ragSystem, err := NewRAGSystem("http://localhost:8000", "http://localhost:8080")
	if err != nil {
		t.Skipf("Cannot connect to services: %v", err)
	}
	defer ragSystem.Close()

	// Test indexing
	err = ragSystem.IndexGoFile(testFile)
	if err != nil {
		t.Errorf("Failed to index file: %v", err)
	}

	// Test search
	results, err := ragSystem.SearchSimilarCode("func Test", 5)
	if err != nil {
		t.Errorf("Failed to search: %v", err)
	}

	t.Logf("Found %d results", len(results))
}

func TestEmbeddingClient(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION_TESTS") != "" {
		t.Skip("Skipping integration tests")
	}

	embedding, err := GetEmbeddingHTTP("http://localhost:8080", "test function")
	if err != nil {
		t.Skipf("Cannot connect to embedding service: %v", err)
	}

	if len(embedding) == 0 {
		t.Error("Expected non-empty embedding")
	}

	t.Logf("Embedding dimension: %d", len(embedding))
}

func TestRAGTestEnhancer(t *testing.T) {
	if os.Getenv("SKIP_INTEGRATION_TESTS") != "" {
		t.Skip("Skipping integration tests")
	}

	ragSystem, err := NewRAGSystem("http://localhost:8000", "http://localhost:8080")
	if err != nil {
		t.Skipf("Cannot connect to services: %v", err)
	}
	defer ragSystem.Close()

	enhancer := NewRAGTestEnhancer(ragSystem)

	testCode := "func ExampleFunction() error { return nil }"
	suggestions, err := enhancer.EnhanceTestGeneration(context.TODO(), testCode)
	if err != nil {
		t.Errorf("Failed to generate suggestions: %v", err)
	}

	if suggestions == "" {
		t.Error("Expected non-empty suggestions")
	}

	t.Logf("Generated suggestions length: %d", len(suggestions))
}
