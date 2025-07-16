package embed

import (
	"fmt"
	"sort"
	"strings"
)

type Chunker interface {
	Chunk(content string, tokenLimit int) (map[int]string, error)
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

// Chunk splits code into chunks based on token limits and points of interest.
func (cc *CodeChunker) Chunk(code string, tokenLimit int) (map[int]string, error) {
	parser, err := NewCodeParser(cc.fileExtension)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize code parser for extension %s: %w", cc.fileExtension, err)
	}
	defer parser.Close()

	lines := strings.Split(code, "\n")
	if len(lines) == 0 || (len(lines) == 1 && lines[0] == "") {
		return map[int]string{}, nil
	}

	poiLinesMap, err := parser.GetLinesForPointsOfInterest(code, cc.fileExtension)
	if err != nil {
		return nil, fmt.Errorf("failed to get lines for points of interest (ext: %s): %w", cc.fileExtension, err)
	}
	commentLinesMap, err := parser.GetLinesForComments(code, cc.fileExtension)
	if err != nil {
		return nil, fmt.Errorf("failed to get lines for comments (ext: %s): %w", cc.fileExtension, err)
	}

	var rawBreakpoints []int
	for _, lineNums := range poiLinesMap {
		rawBreakpoints = append(rawBreakpoints, lineNums...)
	}
	sort.Ints(rawBreakpoints)

	var commentLines []int
	for _, lineNums := range commentLinesMap {
		commentLines = append(commentLines, lineNums...)
	}
	sort.Ints(commentLines)

	isCommentLine := make(map[int]bool)
	for _, cl := range commentLines {
		isCommentLine[cl] = true
	}

	adjustedBreakpointsSet := make(map[int]struct{})
	for _, bp := range rawBreakpoints {
		actualPrecedingCommentStart := -1
		lineBeforeBp := bp - 1
		if lineBeforeBp >= 0 && isCommentLine[lineBeforeBp] {
			currentLeadingComment := lineBeforeBp
			for currentLeadingComment >= 0 && isCommentLine[currentLeadingComment] {
				actualPrecedingCommentStart = currentLeadingComment
				currentLeadingComment--
			}
		}

		if actualPrecedingCommentStart != -1 {
			adjustedBreakpointsSet[actualPrecedingCommentStart] = struct{}{}
		} else {
			adjustedBreakpointsSet[bp] = struct{}{}
		}
	}

	finalBreakpoints := make([]int, 0, len(adjustedBreakpointsSet))
	for bp := range adjustedBreakpointsSet {
		finalBreakpoints = append(finalBreakpoints, bp)
	}
	sort.Ints(finalBreakpoints)

	chunks := make(map[int]string)
	chunkNumber := 1
	currentChunkLines := []string{}
	currentTokenCount := 0
	currentLineIndex := 0    // 0-indexed, iterating through `lines`
	chunkStartLineIndex := 0 // 0-indexed, start of current chunk in `lines`

	for currentLineIndex < len(lines) {
		line := lines[currentLineIndex]
		lineTokenCount, err := CountTokens(line+"\n", cc.encodingName)
		if err != nil {
			return nil, fmt.Errorf("error counting tokens for line %d (ext: %s, encoding: %s): %w", currentLineIndex, cc.fileExtension, cc.encodingName, err)
		}

		// Case 1: Current line itself is too large for a new chunk
		if len(currentChunkLines) == 0 && lineTokenCount > tokenLimit {
			if strings.TrimSpace(line) != "" {
				chunks[chunkNumber] = line
				chunkNumber++
			}
			chunkStartLineIndex = currentLineIndex + 1
			currentLineIndex++
			continue
		}

		// Case 2: Adding current line exceeds token limit for the accumulated chunk
		if currentTokenCount+lineTokenCount > tokenLimit && len(currentChunkLines) > 0 {
			potentialStopLine := chunkStartLineIndex
			foundInternalBreakpoint := false
			for i := len(finalBreakpoints) - 1; i >= 0; i-- {
				bp := finalBreakpoints[i]
				if bp > chunkStartLineIndex && bp < currentLineIndex {
					potentialStopLine = bp
					foundInternalBreakpoint = true
					break
				}
				if bp <= chunkStartLineIndex {
					break
				}
			}

			var linesForChunk []string
			if foundInternalBreakpoint {
				linesForChunk = lines[chunkStartLineIndex:potentialStopLine]
				chunkStartLineIndex = potentialStopLine
			} else {
				linesForChunk = lines[chunkStartLineIndex:currentLineIndex]
				chunkStartLineIndex = currentLineIndex
			}

			chunkContent := strings.Join(linesForChunk, "\n")
			if strings.TrimSpace(chunkContent) != "" {
				chunks[chunkNumber] = chunkContent
				chunkNumber++
			}

			currentChunkLines = []string{}
			currentTokenCount = 0
			continue
		}

		// Case 3: Add line to current chunk
		currentChunkLines = append(currentChunkLines, line)
		currentTokenCount += lineTokenCount
		currentLineIndex++
	}

	if len(currentChunkLines) > 0 {
		chunkContent := strings.Join(currentChunkLines, "\n")
		if strings.TrimSpace(chunkContent) != "" {
			chunks[chunkNumber] = chunkContent
		}
	}

	return chunks, nil
}

// GetChunk retrieves a specific chunk by its number.
func (cc *CodeChunker) GetChunk(chunkedCodebase map[int]string, chunkNumber int) (string, bool) {
	chunk, ok := chunkedCodebase[chunkNumber]
	return chunk, ok
}
