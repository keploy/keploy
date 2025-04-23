package vector

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// These tests require a running ChromaDB instance and OpenAI API key.
// To run these tests, set the following environment variables:
// - KEPLOY_VECTOR_DB_ENABLED=true
// - CHROMA_URL=http://localhost:8000
// - OPENAI_API_KEY=your-openai-api-key

func TestRAGIntegration(t *testing.T) {
	// Skip if vector database integration is not enabled
	if os.Getenv("KEPLOY_VECTOR_DB_ENABLED") != "true" {
		t.Skip("Skipping test because KEPLOY_VECTOR_DB_ENABLED is not set to true")
	}

	// Check if OpenAI API key is available
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("Skipping test because OPENAI_API_KEY is not set")
	}

	// Check if ChromaDB URL is available
	if os.Getenv("CHROMA_URL") == "" {
		t.Skip("Skipping test because CHROMA_URL is not set")
	}

	logger := zaptest.NewLogger(t)
	defer logger.Sync()

	// Create the integration service
	ctx := context.Background()
	service := NewIntegrationService(logger)
	err := service.Initialize(ctx)
	assert.NoError(t, err)
	defer service.Close(ctx)

	// Test adding and retrieving a mock
	t.Run("IndexAndRetrieveMock", func(t *testing.T) {
		// Create a test mock
		mock := createTestMock("HTTP")

		// Index the mock
		err := service.IndexMock(ctx, mock)
		assert.NoError(t, err)

		// Give the vector database time to index
		time.Sleep(1 * time.Second)

		// Create a query that should match the mock
		queryText := `GET /api/users HTTP/1.1
Host: example.com
Accept: application/json`

		// Find the mock by vector similarity
		mocks := []*models.Mock{mock}
		idx, found := service.FindMockByVectorSimilarity(ctx, []byte(queryText), mocks, "HTTP")
		assert.True(t, found)
		assert.Equal(t, 0, idx)

		// Clean up
		err = service.DeleteMock(ctx, mock.Name)
		assert.NoError(t, err)
	})

	t.Run("QueryWithNoMatch", func(t *testing.T) {
		// Create a query that should not match any mock
		queryText := "This query should not match any mock in the database."
		
		// Find the mock by vector similarity
		mocks := []*models.Mock{}
		idx, found := service.FindMockByVectorSimilarity(ctx, []byte(queryText), mocks, "Unknown")
		assert.False(t, found)
		assert.Equal(t, -1, idx)
	})

	t.Run("UpdateMock", func(t *testing.T) {
		// Create a test mock
		mock := createTestMock("HTTP")

		// Index the mock
		err := service.IndexMock(ctx, mock)
		assert.NoError(t, err)

		// Give the vector database time to index
		time.Sleep(1 * time.Second)

		// Update the mock
		mock.HTTPReq.Path = "/api/users/123"
		err = service.UpdateMock(ctx, mock)
		assert.NoError(t, err)

		// Give the vector database time to update
		time.Sleep(1 * time.Second)

		// Create a query that should match the updated mock
		queryText := `GET /api/users/123 HTTP/1.1
Host: example.com
Accept: application/json`

		// Find the mock by vector similarity
		mocks := []*models.Mock{mock}
		idx, found := service.FindMockByVectorSimilarity(ctx, []byte(queryText), mocks, "HTTP")
		assert.True(t, found)
		assert.Equal(t, 0, idx)

		// Clean up
		err = service.DeleteMock(ctx, mock.Name)
		assert.NoError(t, err)
	})
}

func TestVectorDBFactory(t *testing.T) {
	// Skip if OpenAI API key is not available
	if os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("Skipping test because OPENAI_API_KEY is not set")
	}

	logger := zaptest.NewLogger(t)
	defer logger.Sync()

	t.Run("CreateOpenAIEmbeddingService", func(t *testing.T) {
		factory := NewFactory(logger, "", "")
		assert.Equal(t, VectorDBTypeChroma, factory.vectorDBType)
		assert.Equal(t, EmbeddingModelTypeOpenAI, factory.embeddingModel)

		ctx := context.Background()
		embeddingService, err := factory.createEmbeddingService(ctx)
		assert.NoError(t, err)
		assert.NotNil(t, embeddingService)

		// Initialize and test the embedding service
		err = embeddingService.Initialize(ctx)
		assert.NoError(t, err)

		// Test generating an embedding
		embedding, err := embeddingService.GetEmbedding(ctx, "Test embedding generation")
		assert.NoError(t, err)
		assert.NotNil(t, embedding)
		assert.Equal(t, DefaultOpenAIDimension, len(embedding))

		// Test batch embedding generation
		texts := []string{"First text", "Second text", "Third text"}
		embeddings, err := embeddingService.GetEmbeddings(ctx, texts)
		assert.NoError(t, err)
		assert.Equal(t, len(texts), len(embeddings))
		for _, emb := range embeddings {
			assert.Equal(t, DefaultOpenAIDimension, len(emb))
		}

		// Close the embedding service
		err = embeddingService.Close(ctx)
		assert.NoError(t, err)
	})
}

// Helper function to create a test mock
func createTestMock(kind string) *models.Mock {
	mockID := uuid.New().String()
	
	mock := &models.Mock{
		Name: mockID,
		Kind: models.Kind(kind),
		Spec: models.MockSpec{
			Created: time.Now().Unix(),
		},
	}
	
	if kind == "HTTP" {
		mock.Spec.HTTPReq = &models.HTTPReq{
			Method: "GET",
			URL:    "http://example.com/api/users",
			Path:   "/api/users",
			Header: map[string][]string{
				"Accept": {"application/json"},
				"Host":   {"example.com"},
			},
		}
		
		mock.Spec.HTTPResp = &models.HTTPResp{
			StatusCode: 200,
			Header: map[string][]string{
				"Content-Type": {"application/json"},
			},
			Body: []byte(`{"users": [{"id": 1, "name": "User 1"}, {"id": 2, "name": "User 2"}]}`),
		}
	}
	
	return mock
} 