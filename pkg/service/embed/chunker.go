package embed

import (
	"fmt"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/pkoukk/tiktoken-go"
)

type Chunker interface {
	Chunk(parser *CodeParser, rootNode *sitter.Node, content string, tokenLimit int) (map[int]string, error)
	GetChunk(chunkedContent map[int]string, chunkNumber int) (string, bool)
}

func PrintChunks(chunks map[int]string) {
	keys := make([]int, 0, len(chunks))
	for k := range chunks {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	for _, k := range keys {
		chunkCode := chunks[k]
		fmt.Printf("Chunk %d:\n", k)
		fmt.Println("=" + strings.Repeat("=", 39))
		fmt.Println(chunkCode)
		fmt.Println("=" + strings.Repeat("=", 39))
	}
}

// ConsolidateChunksIntoFile joins chunks into a single string.
func ConsolidateChunksIntoFile(chunks map[int]string) string {
	keys := make([]int, 0, len(chunks))
	for k := range chunks {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var builder strings.Builder
	for i, k := range keys {
		if i > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(chunks[k])
	}
	return builder.String()
}

// CountLinesInConsolidated counts lines in a consolidated string.
func CountLinesInConsolidated(consolidatedChunks string) int {
	if consolidatedChunks == "" {
		return 0
	}
	return strings.Count(consolidatedChunks, "\n") + 1
}

type CodeChunker struct {
	fileExtension string
	encodingName  string
}

func NewCodeChunker(fileExtension string, encodingName string) *CodeChunker {
	if encodingName == "" {
		encodingName = "cl100k_base"
	}
	return &CodeChunker{
		fileExtension: fileExtension,
		encodingName:  encodingName,
	}
}

// Chunk splits code into chunks based on token limits and semantic blocks (points of interest).
func (cc *CodeChunker) Chunk(parser *CodeParser, rootNode *sitter.Node, code string, tokenLimit int) (map[int]string, error) {
	if parser == nil {
		return nil, fmt.Errorf("code parser is nil")
	}
	if rootNode == nil {
		return nil, fmt.Errorf("root AST node is nil")
	}

	if code == "" {
		return map[int]string{}, nil
	}

	pointsOfInterest, err := parser.ExtractPointsOfInterest(rootNode, cc.fileExtension)
	if err != nil {
		return nil, fmt.Errorf("failed to get points of interest (ext: %s): %w", cc.fileExtension, err)
	}

	// Sort POIs by their start line to process the file in order.
	sort.Slice(pointsOfInterest, func(i, j int) bool {
		return pointsOfInterest[i].Node.StartPoint().Row < pointsOfInterest[j].Node.StartPoint().Row
	})

	chunks := make(map[int]string)
	chunkNumber := 1

	// Process each function/method as a block.
	for _, poi := range pointsOfInterest {
		// Process the POI block itself.
		blockContent := poi.Node.Content([]byte(code))
		tokenCount, err := CountTokens(blockContent, cc.encodingName)
		if err != nil {
			return nil, fmt.Errorf("error counting tokens for a block: %w", err)
		}

		if tokenCount <= tokenLimit {
			// The entire block fits into a single chunk.
			if strings.TrimSpace(blockContent) != "" {
				chunks[chunkNumber] = blockContent
				chunkNumber++
			}
		} else {
			// The block is too large; split it by token count.
			functionName := parser.ExtractSymbolName(poi.Node, []byte(code))
			splitOversizedBlock(blockContent, tokenLimit, cc.encodingName, &chunks, &chunkNumber, functionName)
		}
	}

	return chunks, nil
}

// splitOversizedBlock handles the case where a single semantic block (like a function)
// is larger than the token limit. It splits the block into chunks based on token count.
func splitOversizedBlock(blockContent string, tokenLimit int, encodingName string, chunks *map[int]string, chunkNumber *int, functionName string) {
	tke, err := tiktoken.EncodingForModel(encodingName)
	if err != nil {
		tke, _ = tiktoken.GetEncoding("cl100k_base")
	}

	allTokens := tke.Encode(blockContent, nil, nil)
	partNumber := 1

	for start := 0; start < len(allTokens); {
		end := start + tokenLimit
		if end > len(allTokens) {
			end = len(allTokens)
		}

		chunkTokens := allTokens[start:end]
		chunkContentBytes := tke.Decode(chunkTokens)
		chunkContent := string(chunkContentBytes)

		isLast := end == len(allTokens)

		var header string
		if partNumber == 1 {
			header = fmt.Sprintf("Function %s (start):\n", functionName)
		} else if isLast {
			header = fmt.Sprintf("Function %s (end):\n", functionName)
		} else {
			header = fmt.Sprintf("Function %s (part %d):\n", functionName, partNumber)
		}

		(*chunks)[*chunkNumber] = header + chunkContent
		*chunkNumber++
		partNumber++

		start = end
	}
}

// GetChunk retrieves a specific chunk by its number.
func (cc *CodeChunker) GetChunk(chunkedCodebase map[int]string, chunkNumber int) (string, bool) {
	chunk, ok := chunkedCodebase[chunkNumber]
	return chunk, ok
}
