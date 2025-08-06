package embed

import "context"

type Service interface {
	Start(ctx context.Context) error
	ProcessCode(code string, fileExtension string, tokenLimit int) (map[int]string, error)
	GenerateEmbeddings(ctx context.Context, chunks map[int]string, filePath string) error
	GenerateEmbeddingsForQ(ctx context.Context, contents []string) ([][]float32, error)
	SearchSimilarCode(ctx context.Context, queryEmbedding []float32, limit int) ([]SearchResult, error)
	Converse(ctx context.Context, query string) error
}

type SearchResult struct {
	FilePath string  `json:"file_path"`
	ChunkID  int     `json:"chunk_id"`
	Content  string  `json:"content"`
	Distance float64 `json:"distance"`
}
