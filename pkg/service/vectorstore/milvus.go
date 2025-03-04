package vectorstore

import (
	"context"
	"fmt"
	"time"

	"github.com/milvus-io/milvus-sdk-go/v2/client"
	"github.com/milvus-io/milvus-sdk-go/v2/entity"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

type MilvusStore struct {
	client  client.Client
	logger  *zap.Logger
	config  *MilvusConfig
	timeout time.Duration
}

type MilvusConfig struct {
	Host           string
	Port           int
	CollectionName string
	Dimension      int
	IndexType      string
	MetricType     string
}

func NewMilvusStore(ctx context.Context, logger *zap.Logger, config *MilvusConfig) (*MilvusStore, error) {
	// Set default timeout
	timeout := 10 * time.Second

	// Connect to Milvus
	addr := fmt.Sprintf("%s:%d", config.Host, config.Port)
	c, err := client.NewGrpcClient(ctx, addr)
	if err != nil {
		utils.LogError(logger, err, "failed to connect to Milvus")
		return nil, err
	}

	store := &MilvusStore{
		client:  c,
		logger:  logger,
		config:  config,
		timeout: timeout,
	}

	// Initialize collection
	err = store.initializeCollection(ctx)
	if err != nil {
		return nil, err
	}

	return store, nil
}

func (m *MilvusStore) initializeCollection(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	// Check if collection exists
	exists, err := m.client.HasCollection(ctx, m.config.CollectionName)
	if err != nil {
		utils.LogError(m.logger, err, "failed to check if collection exists")
		return err
	}

	// Create collection if it doesn't exist
	if !exists {
		schema := &entity.Schema{
			CollectionName: m.config.CollectionName,
			Fields: []*entity.Field{
				{Name: "id", DataType: entity.FieldTypeInt64, PrimaryKey: true, AutoID: true},
				{Name: "path", DataType: entity.FieldTypeVarChar, TypeParams: map[string]string{"max_length": "512"}},
				{Name: "language", DataType: entity.FieldTypeVarChar, TypeParams: map[string]string{"max_length": "32"}},
				{Name: "content", DataType: entity.FieldTypeVarChar, TypeParams: map[string]string{"max_length": "65535"}},
				{Name: "embedding", DataType: entity.FieldTypeFloatVector, TypeParams: map[string]string{"dim": fmt.Sprintf("%d", m.config.Dimension)}},
				{Name: "timestamp", DataType: entity.FieldTypeInt64},
			},
		}

		createCtx, cancel := context.WithTimeout(ctx, m.timeout)
		defer cancel()
		err = m.client.CreateCollection(createCtx, schema, int32(1))
		if err != nil {
			utils.LogError(m.logger, err, "failed to create collection")
			return err
		}

		// Create index on the vector field
		idx, err := entity.NewIndexIvfFlat(entity.L2, 1024)
		if err != nil {
			utils.LogError(m.logger, err, "failed to create index")
			return err
		}

		indexCtx, cancel := context.WithTimeout(ctx, m.timeout)
		defer cancel()
		err = m.client.CreateIndex(indexCtx, m.config.CollectionName, "embedding", idx, false)
		if err != nil {
			utils.LogError(m.logger, err, "failed to create index")
			return err
		}
	}

	return nil
}

// IndexCode indexes code snippets in Milvus
func (m *MilvusStore) IndexCode(ctx context.Context, path string, language string, content string, embedding []float32) error {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	// Load the collection
	err := m.client.LoadCollection(ctx, m.config.CollectionName, false)
	if err != nil {
		utils.LogError(m.logger, err, "failed to load collection")
		return err
	}
	defer m.client.ReleaseCollection(ctx, m.config.CollectionName)

	// Prepare data to insert
	columns := []entity.Column{
		entity.NewColumnVarChar("path", []string{path}),
		entity.NewColumnVarChar("language", []string{language}),
		entity.NewColumnVarChar("content", []string{content}),
		entity.NewColumnFloatVector("embedding", m.config.Dimension, [][]float32{embedding}),
		entity.NewColumnInt64("timestamp", []int64{time.Now().Unix()}),
	}

	// Insert data
	_, err = m.client.Insert(ctx, m.config.CollectionName, "", columns...)
	if err != nil {
		utils.LogError(m.logger, err, "failed to insert data into Milvus")
		return err
	}

	return nil
}

// SearchSimilarCode searches for similar code snippets
func (m *MilvusStore) SearchSimilarCode(ctx context.Context, embedding []float32, topK int) ([]CodeSearchResult, error) {
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	// Load the collection
	err := m.client.LoadCollection(ctx, m.config.CollectionName, false)
	if err != nil {
		utils.LogError(m.logger, err, "failed to load collection")
		return nil, err
	}
	defer m.client.ReleaseCollection(ctx, m.config.CollectionName)

	// Search parameters
	sp, err := entity.NewIndexFlatSearchParam()
	if err != nil {
		utils.LogError(m.logger, err, "failed to create search parameters")
		return nil, err
	}

	// Execute search
	results, err := m.client.Search(
		ctx,
		m.config.CollectionName,
		[]string{},
		"",
		[]string{"path", "language", "content"},
		[]entity.Vector{entity.FloatVector(embedding)},
		"embedding",
		entity.L2,
		topK,
		sp,
	)

	if err != nil {
		utils.LogError(m.logger, err, "failed to search Milvus")
		return nil, err
	}

	// Process results
	searchResults := make([]CodeSearchResult, 0, len(results))
	for i := 0; i < len(results); i++ {
		pathCol, ok := results[i].Fields[0].(*entity.ColumnVarChar)
		if !ok || pathCol.Len() == 0 {
			continue
		}
		paths := pathCol.Data()

		langCol, ok := results[i].Fields[1].(*entity.ColumnVarChar)
		if !ok || langCol.Len() == 0 {
			continue
		}
		languages := langCol.Data()

		contentCol, ok := results[i].Fields[2].(*entity.ColumnVarChar)
		if !ok || contentCol.Len() == 0 {
			continue
		}
		contents := contentCol.Data()

		for j := 0; j < len(paths); j++ {
			searchResults = append(searchResults, CodeSearchResult{
				Path:     paths[j],
				Language: languages[j],
				Content:  contents[j],
				Score:    results[i].Scores[j],
			})
		}
	}

	return searchResults, nil
}

// CodeSearchResult represents a search result
type CodeSearchResult struct {
	Path     string
	Language string
	Content  string
	Score    float32
}
