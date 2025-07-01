package embed

import "context"

type Service interface {
	Start(ctx context.Context) error
	ProcessCode(code string, fileExtension string, tokenLimit int) (map[int]string, error)
	GenerateEmbeddings(chunks map[int]string, filePath string) error
}
