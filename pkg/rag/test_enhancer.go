package rag

import (
	"context"
	"fmt"
	"strings"
)

type RAGTestEnhancer struct {
	ragSystem *RAGSystem
}

func NewRAGTestEnhancer(ragSystem *RAGSystem) *RAGTestEnhancer {
	return &RAGTestEnhancer{
		ragSystem: ragSystem,
	}
}

func (rte *RAGTestEnhancer) EnhanceTestGeneration(ctx context.Context, targetCode string) (string, error) {
	// Search for similar code patterns
	similarCode, err := rte.ragSystem.SearchSimilarCode(targetCode, 3)
	if err != nil {
		return "", err
	}

	// Build enhanced context
	enhancedContext := rte.buildContextFromSimilarCode(similarCode)

	// Generate enhanced test suggestions
	testSuggestions := rte.generateTestSuggestions(targetCode, enhancedContext)

	return testSuggestions, nil
}

func (rte *RAGTestEnhancer) buildContextFromSimilarCode(similarCode []CodeMatch) string {
	if len(similarCode) == 0 {
		return "No similar code patterns found."
	}

	var context strings.Builder
	context.WriteString("Similar code patterns found:\n\n")

	for i, match := range similarCode {
		if i >= 3 { // Limit to top 3 matches
			break
		}

		funcName := "unknown"
		filePath := "unknown"

		if match.Metadata != nil {
			if fn, ok := match.Metadata["function_name"]; ok {
				funcName = fmt.Sprintf("%v", fn)
			}
			if fp, ok := match.Metadata["file_path"]; ok {
				filePath = fmt.Sprintf("%v", fp)
			}
		}

		context.WriteString(fmt.Sprintf("--- Pattern %d (Function: %s, File: %s, Similarity: %.2f) ---\n",
			i+1, funcName, filePath, 1.0-match.Distance))
		context.WriteString(match.Code)
		context.WriteString("\n\n")
	}

	return context.String()
}

func (rte *RAGTestEnhancer) generateTestSuggestions(targetCode, context string) string {
	return fmt.Sprintf(`
// RAG-Enhanced Test Suggestions
// Generated based on similar code patterns in the codebase

/*
Target Code Analysis:
%s

%s
*/

// Suggested Test Cases Based on Similar Patterns:
func TestTargetFunction(t *testing.T) {
    // Test Case 1: Happy Path
    // Based on similar patterns, test the normal execution flow
    
    // Test Case 2: Error Handling
    // Based on similar error patterns found in the codebase
    
    // Test Case 3: Edge Cases
    // Based on edge cases handled in similar functions
    
    // Test Case 4: Boundary Conditions
    // Test input validation and boundary values
    
    // Test Case 5: Integration Tests
    // Test interaction with external dependencies
}

// Mock Suggestions:
// Based on similar code patterns, consider mocking:
// - HTTP clients
// - Database connections  
// - External service calls
// - File system operations

// Performance Test Suggestions:
// Based on similar functions, consider testing:
// - Response time under load
// - Memory usage patterns
// - Concurrent access scenarios
`, targetCode, context)
}

func (rte *RAGTestEnhancer) GetTestSuggestionsForFunction(funcName string, topK int) ([]string, error) {
	// Search for similar functions
	results, err := rte.ragSystem.SearchSimilarCode(fmt.Sprintf("func %s", funcName), topK)
	if err != nil {
		return nil, err
	}

	var suggestions []string
	for _, match := range results {
		if match.Metadata != nil {
			if fn, ok := match.Metadata["function_name"]; ok {
				suggestion := fmt.Sprintf("Consider testing patterns similar to function '%v' in '%v'",
					fn, match.Metadata["file_path"])
				suggestions = append(suggestions, suggestion)
			}
		}
	}

	return suggestions, nil
}
