package vector

import (
	"context"
	"fmt"
	"os"
	"sync"

	"go.uber.org/zap"
)

const (
	// Vector database types
	VectorDBTypeChroma = "chroma"
	VectorDBTypePinecone = "pinecone"
	
	// Embedding model types
	EmbeddingModelTypeOpenAI = "openai"
)

// Factory creates and manages RAG service components
type Factory struct {
	logger           *zap.Logger
	vectorDBType     string
	embeddingModel   string
	mu               sync.RWMutex
	ragService       RAGService
	vectorDBService  VectorDBService
	embeddingService EmbeddingService
}

// NewFactory creates a new RAG factory
func NewFactory(logger *zap.Logger, vectorDBType, embeddingModel string) *Factory {
	if vectorDBType == "" {
		vectorDBType = VectorDBTypeChroma // Default to ChromaDB
	}
	
	if embeddingModel == "" {
		embeddingModel = EmbeddingModelTypeOpenAI // Default to OpenAI embeddings
	}
	
	return &Factory{
		logger:         logger,
		vectorDBType:   vectorDBType,
		embeddingModel: embeddingModel,
	}
}

// GetRAGService returns the RAG service, creating it if necessary
func (f *Factory) GetRAGService(ctx context.Context) (RAGService, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	
	if f.ragService != nil {
		return f.ragService, nil
	}
	
	// Create the vector database service
	vectorDB, err := f.createVectorDBService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create vector database service: %w", err)
	}
	f.vectorDBService = vectorDB
	
	// Create the embedding service
	embeddingService, err := f.createEmbeddingService(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create embedding service: %w", err)
	}
	f.embeddingService = embeddingService
	
	// Create the RAG service
	ragService := NewRAGService(f.logger, vectorDB, embeddingService)
	err = ragService.Initialize(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize RAG service: %w", err)
	}
	
	f.ragService = ragService
	return f.ragService, nil
}

// Close closes all services
func (f *Factory) Close(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	
	var errors []error
	
	if f.ragService != nil {
		if err := f.ragService.Close(ctx); err != nil {
			errors = append(errors, fmt.Errorf("failed to close RAG service: %w", err))
		}
		f.ragService = nil
	}
	
	f.vectorDBService = nil
	f.embeddingService = nil
	
	if len(errors) > 0 {
		return errors[0]
	}
	
	return nil
}

// createVectorDBService creates a vector database service based on configuration
func (f *Factory) createVectorDBService(ctx context.Context) (VectorDBService, error) {
	switch f.vectorDBType {
	case VectorDBTypeChroma:
		baseURL := os.Getenv("CHROMA_URL")
		return NewChromaDBService(f.logger, baseURL), nil
	case VectorDBTypePinecone:
		return nil, fmt.Errorf("pinecone vector database not yet implemented")
	default:
		return nil, fmt.Errorf("unsupported vector database type: %s", f.vectorDBType)
	}
}

// createEmbeddingService creates an embedding service based on configuration
func (f *Factory) createEmbeddingService(ctx context.Context) (EmbeddingService, error) {
	switch f.embeddingModel {
	case EmbeddingModelTypeOpenAI:
		apiKey := os.Getenv("OPENAI_API_KEY")
		return NewOpenAIEmbeddingService(f.logger, apiKey), nil
	default:
		return nil, fmt.Errorf("unsupported embedding model type: %s", f.embeddingModel)
	}
} 