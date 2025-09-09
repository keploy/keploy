package grpc

import (
	"sort"
	"strings"
)

// Hard guards to prevent pathological work on giant or adversarial inputs.
const (
	maxCanonDepth = 64      // reasonable for nested protos
	maxCanonBytes = 1 << 20 // 1 MiB per side; beyond this we bail out
	maxBlocks     = 10_000  // safety for degenerate line/block splits
)

// canonicalizeTopLevelBlocks makes protoscope-like text order-insensitive at *all* levels by:
// - splitting into top-level field blocks
// - recursively canonicalizing the content of each "{...}" block
// - sorting blocks lexicographically
// - joining them back together
func CanonicalizeTopLevelBlocks(s string) string {
	if len(s) > maxCanonBytes {
		// Too large to safely canonicalize – return normalized but otherwise unchanged.
		return normalizeWhitespace(s)
	}
	return canonicalizeRecursive(s, 0)
}

func canonicalizeRecursive(s string, depth int) string {
	if depth > maxCanonDepth {
		return normalizeWhitespace(s)
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return ""
	}

	blocks := splitTopLevelBlocks(trimmed)
	if len(blocks) > maxBlocks {
		// Degenerate input: avoid n^2 sorts/work.
		return normalizeWhitespace(s)
	}
	// Recursively canonicalize inner bodies of each block
	for i := range blocks {
		blocks[i] = canonicalizeBlock(blocks[i], depth)
		blocks[i] = normalizeWhitespace(blocks[i])
	}

	// Order-insensitive among siblings
	sort.Strings(blocks)
	return strings.Join(blocks, "\n")
}

// splitTopLevelBlocks groups lines into top-level "field blocks".
// A new block starts when depth==0 and the line matches ^\s*\d+:
// Bare tokens like "64", "ai64", etc. stay with the preceding block.
func splitTopLevelBlocks(s string) []string {
	lines := strings.Split(s, "\n")
	var blocks []string
	var cur []string
	depth := 0

	isFieldLine := func(line string) bool {
		line = strings.TrimLeft(line, " \t")
		if line == "" {
			return false
		}
		i := 0
		for i < len(line) && line[i] >= '0' && line[i] <= '9' {
			i++
		}
		return i > 0 && i < len(line) && line[i] == ':'
	}

	flush := func() {
		if len(cur) == 0 {
			return
		}
		blk := strings.TrimRight(strings.Join(cur, "\n"), "\n")
		if blk != "" {
			blocks = append(blocks, blk)
		}
		cur = cur[:0]
	}

	for i, ln := range lines {
		// If we see a new top-level field start, flush previous block
		if depth == 0 && isFieldLine(ln) && len(cur) > 0 {
			flush()
		}
		cur = append(cur, ln)
		depth += braceDeltaIgnoringStrings(ln)
		if i == len(lines)-1 {
			flush()
		}
		if len(blocks) > maxBlocks {
			// Stop early; upstream will bail out.
			return blocks
		}
	}
	return blocks
}

// canonicalizeBlock finds the outermost "{...}" of this block (if any),
// recursively canonicalizes the inside, and reassembles the block.
// If there is no brace, returns the block as-is (after whitespace normalize by caller).
func canonicalizeBlock(block string, depth int) string {
	// Find first '{' outside strings/backticks.
	open := indexFirstOpenBrace(block)
	if open < 0 {
		return block
	}

	// Find its matching '}' (depth-aware, strings-safe).
	closeIdx := findMatchingCloseBrace(block, open)
	if closeIdx < 0 {
		// Unbalanced; fallback to original to avoid mangling.
		return block
	}

	inner := block[open+1 : closeIdx]
	innerCanon := canonicalizeRecursive(inner, depth+1)

	// Reassemble: keep exactly the same outer text, replace inner with canonical form.
	return block[:open+1] + innerCanon + block[closeIdx:]
}

// indexFirstOpenBrace returns the index of the first '{' not inside quotes/backticks.
func indexFirstOpenBrace(s string) int {
	inDq := false // "
	inBt := false // `
	esc := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if inBt {
			if ch == '`' {
				inBt = false
			}
			continue
		}
		if inDq {
			if esc {
				esc = false
				continue
			}
			if ch == '\\' {
				esc = true
				continue
			}
			if ch == '"' {
				inDq = false
			}
			continue
		}

		// Not in string
		if ch == '`' {
			inBt = true
			continue
		}
		if ch == '"' {
			inDq = true
			continue
		}
		if ch == '{' {
			return i
		}
	}
	return -1
}

// findMatchingCloseBrace finds the matching '}' for the '{' at openIdx,
// counting only braces that are outside quotes/backticks.
func findMatchingCloseBrace(s string, openIdx int) int {
	inDq := false
	inBt := false
	esc := false
	depth := 0

	for i := openIdx; i < len(s); i++ {
		ch := s[i]

		if inBt {
			if ch == '`' {
				inBt = false
			}
			continue
		}
		if inDq {
			if esc {
				esc = false
				continue
			}
			if ch == '\\' {
				esc = true
				continue
			}
			if ch == '"' {
				inDq = false
			}
			continue
		}

		switch ch {
		case '`':
			inBt = true
		case '"':
			inDq = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// braceDeltaIgnoringStrings returns net brace count change for the line,
// ignoring any braces that appear inside quotes/backticks.
func braceDeltaIgnoringStrings(line string) int {
	inDq := false
	inBt := false
	esc := false
	delta := 0

	for i := 0; i < len(line); i++ {
		ch := line[i]

		if inBt {
			if ch == '`' {
				inBt = false
			}
			continue
		}
		if inDq {
			if esc {
				esc = false
				continue
			}
			if ch == '\\' {
				esc = true
				continue
			}
			if ch == '"' {
				inDq = false
			}
			continue
		}

		switch ch {
		case '`':
			inBt = true
		case '"':
			inDq = true
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

func normalizeWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	prevBlank := false
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		blank := strings.TrimSpace(ln) == ""
		if blank && prevBlank {
			continue
		}
		out = append(out, ln)
		prevBlank = blank
	}
	return strings.Join(out, "\n")
}
