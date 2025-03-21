package models

// Embedding represents a vector embedding for a code snippet
type Embedding struct {
	ID        string    `json:"id" bson:"id"`
	Vector    []float32 `json:"vector" bson:"vector"`
	Content   string    `json:"content" bson:"content"`
	Metadata  Metadata  `json:"metadata" bson:"metadata"`
	CreatedAt int64     `json:"created_at" bson:"created_at"`
	UpdatedAt int64     `json:"updated_at" bson:"updated_at"`
}

// Metadata contains additional information about the embedding
type Metadata struct {
	Source     string            `json:"source" bson:"source"`
	Kind       Kind              `json:"kind" bson:"kind"`
	MockID     string            `json:"mock_id" bson:"mock_id"`
	Labels     map[string]string `json:"labels" bson:"labels"`
	FileType   string            `json:"file_type" bson:"file_type"`
	LineStart  int               `json:"line_start" bson:"line_start"`
	LineEnd    int               `json:"line_end" bson:"line_end"`
	SourcePath string            `json:"source_path" bson:"source_path"`
}

// SearchResult represents the result of a vector similarity search
type SearchResult struct {
	ID        string   `json:"id" bson:"id"`
	Content   string   `json:"content" bson:"content"`
	Metadata  Metadata `json:"metadata" bson:"metadata"`
	Score     float32  `json:"score" bson:"score"`
	Distance  float32  `json:"distance" bson:"distance"`
	CreatedAt int64    `json:"created_at" bson:"created_at"`
}

// VectorQuery represents a query for the vector database
type VectorQuery struct {
	Query            string            `json:"query"`
	QueryVector      []float32         `json:"query_vector,omitempty"`
	TopK             int               `json:"top_k"`
	Filters          map[string]string `json:"filters,omitempty"`
	IncludeMetadata  bool              `json:"include_metadata"`
	IncludeVectors   bool              `json:"include_vectors"`
	MinScore         float32           `json:"min_score,omitempty"`
	NamespaceFilters []string          `json:"namespace_filters,omitempty"`
}

// VectorQueryResult represents the results of a vector query
type VectorQueryResult struct {
	Results      []SearchResult `json:"results"`
	TotalResults int            `json:"total_results"`
	QueryTime    int64          `json:"query_time_ms"`
} 