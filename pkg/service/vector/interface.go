package vector

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

// VectorDBService defines the interface for interacting with vector databases
type VectorDBService interface {
	// Initialize sets up the vector database connection and configuration
	Initialize(ctx context.Context) error

	// Close closes the vector database connection
	Close(ctx context.Context) error

	// CreateCollection creates a new collection in the vector database
	CreateCollection(ctx context.Context, name string, dimension int) error

	// DeleteCollection deletes a collection from the vector database
	DeleteCollection(ctx context.Context, name string) error

	// ListCollections lists all collections in the vector database
	ListCollections(ctx context.Context) ([]string, error)

	// UpsertEmbedding inserts or updates an embedding in the vector database
	UpsertEmbedding(ctx context.Context, collection string, embedding *models.Embedding) error

	// UpsertEmbeddings inserts or updates multiple embeddings in the vector database
	UpsertEmbeddings(ctx context.Context, collection string, embeddings []*models.Embedding) error

	// DeleteEmbedding deletes an embedding from the vector database
	DeleteEmbedding(ctx context.Context, collection string, id string) error

	// DeleteEmbeddings deletes multiple embeddings from the vector database
	DeleteEmbeddings(ctx context.Context, collection string, ids []string) error

	// GetEmbedding retrieves an embedding from the vector database
	GetEmbedding(ctx context.Context, collection string, id string) (*models.Embedding, error)

	// QueryEmbeddings performs a similarity search on the vector database
	QueryEmbeddings(ctx context.Context, collection string, query *models.VectorQuery) (*models.VectorQueryResult, error)
}

// EmbeddingService defines the interface for generating embeddings
type EmbeddingService interface {
	// Initialize sets up the embedding service
	Initialize(ctx context.Context) error

	// Close closes the embedding service
	Close(ctx context.Context) error

	// GetEmbedding generates an embedding for the given text
	GetEmbedding(ctx context.Context, text string) ([]float32, error)

	// GetEmbeddings generates embeddings for the given texts
	GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error)
}

// RAGService defines the interface for the RAG (Retrieval Augmented Generation) service
type RAGService interface {
	// Initialize sets up the RAG service
	Initialize(ctx context.Context) error

	// Close closes the RAG service
	Close(ctx context.Context) error

	// IndexMock indexes a mock for retrieval
	IndexMock(ctx context.Context, mock *models.Mock) error

	// IndexMocks indexes multiple mocks for retrieval
	IndexMocks(ctx context.Context, mocks []*models.Mock) error

	// RetrieveContext retrieves relevant context for a query
	RetrieveContext(ctx context.Context, query string, options map[string]interface{}) (*models.VectorQueryResult, error)

	// UpdateContext updates the context for a mock
	UpdateContext(ctx context.Context, mock *models.Mock) error

	// DeleteContext deletes the context for a mock
	DeleteContext(ctx context.Context, mockID string) error

	// RefreshIndex refreshes the entire index
	RefreshIndex(ctx context.Context) error
} 