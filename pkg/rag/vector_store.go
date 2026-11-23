package rag

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os/exec"
)

type RAGSystem struct {
	embeddingService string
	collectionName   string
}

type ChromaResponse struct {
	Status  string                 `json:"status"`
	Message string                 `json:"message,omitempty"`
	Results map[string]interface{} `json:"results,omitempty"`
}

func NewRAGSystem(chromaDBURL, embeddingServiceURL string) (*RAGSystem, error) {
	system := &RAGSystem{
		embeddingService: embeddingServiceURL,
		collectionName:   "keploy_code_snippets",
	}

	// Create collection using Python bridge
	err := system.createCollection()
	if err != nil {
		return nil, fmt.Errorf("failed to create collection: %w", err)
	}

	return system, nil
}

func (rs *RAGSystem) createCollection() error {
	cmd := exec.Command("python", "chroma_bridge.py", "create", "--collection", rs.collectionName)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("python bridge failed: %w", err)
	}

	var response ChromaResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Status == "error" {
		return fmt.Errorf("ChromaDB error: %s", response.Message)
	}

	fmt.Printf("✅ %s\n", response.Message)
	return nil
}

func (rs *RAGSystem) IndexGoFile(filePath string) error {
	content, err := ioutil.ReadFile(filePath)
	if err != nil {
		return err
	}

	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filePath, content, parser.ParseComments)
	if err != nil {
		return err
	}

	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Name != nil {
				funcCode := rs.extractFunctionCode(string(content), fset, x)
				if funcCode != "" {
					if err := rs.indexCodeSnippet(filePath, x.Name.Name, funcCode); err != nil {
						fmt.Printf("Error indexing function %s: %v\n", x.Name.Name, err)
					}
				}
			}
		}
		return true
	})

	return nil
}

func (rs *RAGSystem) extractFunctionCode(content string, fset *token.FileSet, fn *ast.FuncDecl) string {
	start := fset.Position(fn.Pos()).Offset
	end := fset.Position(fn.End()).Offset
	if start >= 0 && end <= len(content) && start < end {
		return content[start:end]
	}
	return ""
}

func (rs *RAGSystem) indexCodeSnippet(filePath, funcName, code string) error {
	id := fmt.Sprintf("%x", md5.Sum([]byte(filePath+funcName)))

	embedding, err := GetEmbeddingHTTP(rs.embeddingService, code)
	if err != nil {
		return fmt.Errorf("failed to get embedding: %w", err)
	}

	data := map[string]interface{}{
		"ids":       []string{id},
		"documents": []string{code},
		"metadatas": []map[string]interface{}{{
			"file_path":     filePath,
			"function_name": funcName,
			"language":      "go",
		}},
		"embeddings": [][]float64{embedding},
	}

	jsonData, _ := json.Marshal(data)

	cmd := exec.Command("python", "chroma_bridge.py", "add", "--collection", rs.collectionName, "--data", string(jsonData))
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("python bridge failed: %w", err)
	}

	var response ChromaResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Status == "error" {
		return fmt.Errorf("ChromaDB error: %s", response.Message)
	}

	return nil
}

func (rs *RAGSystem) SearchSimilarCode(query string, topK int) ([]CodeMatch, error) {
	queryEmbedding, err := GetEmbeddingHTTP(rs.embeddingService, query)
	if err != nil {
		return nil, err
	}

	data := map[string]interface{}{
		"query_embeddings": [][]float64{queryEmbedding},
		"n_results":        topK,
	}

	jsonData, _ := json.Marshal(data)

	cmd := exec.Command("python", "chroma_bridge.py", "query", "--collection", rs.collectionName, "--data", string(jsonData))
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("python bridge failed: %w", err)
	}

	var response ChromaResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Status == "error" {
		return nil, fmt.Errorf("ChromaDB error: %s", response.Message)
	}

	// Parse results
	var matches []CodeMatch
	if results, ok := response.Results["ids"].([]interface{}); ok && len(results) > 0 {
		ids := results[0].([]interface{})
		documents := response.Results["documents"].([]interface{})[0].([]interface{})
		metadatas := response.Results["metadatas"].([]interface{})[0].([]interface{})
		distances := response.Results["distances"].([]interface{})[0].([]interface{})

		for i, id := range ids {
			match := CodeMatch{
				ID:       id.(string),
				Code:     documents[i].(string),
				Metadata: metadatas[i].(map[string]interface{}),
				Distance: distances[i].(float64),
			}
			matches = append(matches, match)
		}
	}

	return matches, nil
}

func (rs *RAGSystem) Close() error {
	return nil
}

type CodeMatch struct {
	ID       string                 `json:"id"`
	Code     string                 `json:"code"`
	Metadata map[string]interface{} `json:"metadata"`
	Distance float64                `json:"distance"`
}
