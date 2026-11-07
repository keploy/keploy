// Trimmed from github.com/zricethezav/gitleaks/v8 v8.28.0, package detect.
// MIT License, Copyright (c) 2019 Zachary Rice: see LICENSE in this directory.

package secretscan

// span is where a match lies: its lines and columns, and the byte range of
// the line or lines that hold it.
type span struct {
	startLine      int
	endLine        int
	startColumn    int
	endColumn      int
	startLineIndex int
	endLineIndex   int
}

// location returns where matchIndex lies in raw, whose newlines are at
// newlineIndices. Lines count from 0, columns from 1.
func location(newlineIndices [][]int, raw string, matchIndex []int) span {
	var (
		prevNewLine int
		loc         span
		lineSet     bool
		lastLine    int
	)

	start := matchIndex[0]
	end := matchIndex[1]

	loc.startLineIndex = 0

	// A text with no newline is one line that ends at its end.
	if len(newlineIndices) == 0 {
		newlineIndices = [][]int{
			{len(raw), len(raw) + 1},
		}
	}

	for lineNum, pair := range newlineIndices {
		lastLine = lineNum
		newLineByteIndex := pair[0]
		if prevNewLine <= start && start < newLineByteIndex {
			lineSet = true
			loc.startLine = lineNum
			loc.endLine = lineNum
			loc.startColumn = (start - prevNewLine) + 1 // +1 because counting starts at 1
			loc.startLineIndex = prevNewLine
			loc.endLineIndex = newLineByteIndex
		}
		if prevNewLine < end && end <= newLineByteIndex {
			loc.endLine = lineNum
			loc.endColumn = (end - prevNewLine)
			loc.endLineIndex = newLineByteIndex
		}

		prevNewLine = pair[0]
	}

	// The match starts after the last newline.
	if !lineSet {
		loc.startColumn = (start - prevNewLine) + 1 // +1 because counting starts at 1
		loc.endColumn = (end - prevNewLine)
		loc.startLine = lastLine + 1
		loc.endLine = lastLine + 1

		i := 0
		for end+i < len(raw) {
			if raw[end+i] == '\n' {
				break
			}
			if raw[end+i] == '\r' {
				break
			}
			i++
		}
		loc.endLineIndex = end + i
	}
	return loc
}
