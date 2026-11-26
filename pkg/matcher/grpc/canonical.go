package grpc

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Hard guards to prevent pathological work on giant or adversarial inputs.
//
// They bound work, never the rules: each sits above what a recorded gRPC
// message can reach, so a real body is canonicalized whatever its size. A
// message keploy records is at most 5 MiB (larger ones are not recorded),
// protobuf's own decoders stop nesting at 100, and a block is at least a
// line of text. Below them the comparison used to fall back to whitespace-
// normalized text, so the same message failed only for being large: its
// fields' order suddenly mattered.
const (
	maxCanonDepth = 100       // protobuf's default recursion limit
	maxCanonBytes = 64 << 20  // 64 MiB per side
	maxBlocks     = 4_000_000 // more than a 64 MiB body can split into lines
)

// canonicalizeTopLevelBlocks makes protoscope-like text order-insensitive at *all* levels by:
// - splitting into top-level field blocks
// - recursively canonicalizing the content of each "{...}" block
// - sorting blocks lexicographically
// - joining them back together
func CanonicalizeTopLevelBlocks(s string) string {
	return canonicalize(s, false)
}

// CanonicalizeKeepingRepeatedOrder is CanonicalizeTopLevelBlocks for a
// comparison that keeps order (test.ignoreOrdering off, or the proven
// switch): sibling blocks are ordered
// by field number alone, so different fields compare whatever order the wire
// put them in (protobuf gives that order no meaning), while the blocks of one
// field number — a repeated field's elements, or a map's entries — keep the
// order they came in. A reordered repeated field is then a difference.
//
// Protoscope text carries no schema, so a map field (whose entry order
// protobuf does not keep) cannot be told from a repeated message here: its
// entries are compared in order too, unless the caller names it
// (canonicalizeWith's unordered, which a schema gives: WithUnorderedFields).
func CanonicalizeKeepingRepeatedOrder(s string) string {
	return canonicalize(s, true)
}

func canonicalize(s string, keepRepeatedOrder bool) string {
	return canonicalizeWith(s, keepRepeatedOrder, nil)
}

// canonicalizeWith is canonicalize where unordered names, by field-number
// path ("5", "2.4"), the repeated fields whose elements compare in any order
// even when order is kept: a map's entries, which protobuf gives no order.
func canonicalizeWith(s string, keepRepeatedOrder bool, unordered map[string]bool) string {
	if len(s) > maxCanonBytes {
		// Too large to safely canonicalize – return normalized but otherwise unchanged.
		return normalizeWhitespace(s)
	}
	return canonicalizeRecursive(s, 0, keepRepeatedOrder, unordered, "")
}

func canonicalizeRecursive(s string, depth int, keepRepeatedOrder bool, unordered map[string]bool, prefix string) string {
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
		blocks[i] = canonicalizeBlock(blocks[i], depth, keepRepeatedOrder, unordered, prefix)
		blocks[i] = normalizeWhitespace(blocks[i])
	}

	// Strip leading whitespace so sort order depends on content,
	// not position-dependent indentation assigned by protoscope.
	for i := range blocks {
		blocks[i] = strings.TrimLeft(blocks[i], " \t")
	}

	if keepRepeatedOrder {
		// Order-insensitive across fields, in order within one field.
		sort.SliceStable(blocks, func(i, j int) bool {
			return blockFieldNumber(blocks[i]) < blockFieldNumber(blocks[j])
		})
		// A field named unordered (a map) has its elements sorted, as when
		// order is ignored: protobuf keeps no order for them.
		for i := 0; i < len(blocks); {
			n := blockFieldNumber(blocks[i])
			j := i + 1
			for j < len(blocks) && blockFieldNumber(blocks[j]) == n {
				j++
			}
			if n >= 0 && unordered[fieldPath(prefix, n)] {
				sort.Strings(blocks[i:j])
			}
			i = j
		}
		return strings.Join(blocks, "\n")
	}
	// Order-insensitive among siblings
	sort.Strings(blocks)
	return strings.Join(blocks, "\n")
}

// blockFieldNumber is the field number a block starts with ("3: {" is 3), or
// -1 for a block that starts with none (it keeps its place among those).
func blockFieldNumber(block string) int {
	i := 0
	for i < len(block) && block[i] >= '0' && block[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(block) || block[i] != ':' {
		return -1
	}
	n, err := strconv.Atoi(block[:i])
	if err != nil {
		return -1
	}
	return n
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
func canonicalizeBlock(block string, depth int, keepRepeatedOrder bool, unordered map[string]bool, prefix string) string {
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
	innerCanon := canonicalizeRecursive(inner, depth+1, keepRepeatedOrder, unordered, fieldPath(prefix, blockFieldNumber(strings.TrimLeft(block, " \t"))))

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

// stripProtoscopeNoise removes from protoscope text every field a body noise
// key names by its field-number path: "8.2.1" is field 1 of field 2 of field
// 8, and names it in every element of a repeated field on the way. A key with
// patterns removes the field only where its value matches one of them; a key
// with none removes it always. Keys that are not field-number paths (a JSON
// path, which the decoded comparison applies) are left to that comparison.
//
// A key whose patterns do not all compile removes nothing: a typo must never
// widen noise to "always". Those keys are returned, for the caller to say so.
func stripProtoscopeNoise(s string, bodyNoise map[string][]string) (string, []string) {
	paths, invalid := protoscopeNoisePaths(bodyNoise)
	if len(paths) == 0 {
		return s, invalid
	}
	return stripNoisyBlocks(s, paths, 0), invalid
}

type noisePath struct {
	fields   []int
	patterns []*regexp.Regexp
}

func protoscopeNoisePaths(bodyNoise map[string][]string) (out []noisePath, invalid []string) {
	for key, pats := range bodyNoise {
		trimmed := strings.TrimPrefix(strings.TrimPrefix(key, "body."), "decoded_data.")
		parts := strings.Split(trimmed, ".")
		fields := make([]int, 0, len(parts))
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n <= 0 {
				fields = nil
				break
			}
			fields = append(fields, n)
		}
		if len(fields) == 0 {
			continue
		}
		np := noisePath{fields: fields}
		bad := false
		for _, p := range pats {
			if p == "" {
				continue
			}
			re, err := regexp.Compile(p)
			if err != nil {
				bad = true
				break
			}
			np.patterns = append(np.patterns, re)
		}
		if bad {
			invalid = append(invalid, key)
			continue
		}
		out = append(out, np)
	}
	sort.Strings(invalid)
	return out, invalid
}

func stripNoisyBlocks(s string, paths []noisePath, depth int) string {
	if depth > maxCanonDepth || len(s) > maxCanonBytes {
		return s
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}
	blocks := splitTopLevelBlocks(trimmed)
	kept := blocks[:0]
	for _, block := range blocks {
		n := blockFieldNumber(strings.TrimLeft(block, " \t"))
		if n < 0 {
			kept = append(kept, block)
			continue
		}
		var deeper []noisePath
		drop := false
		for _, p := range paths {
			if p.fields[0] != n {
				continue
			}
			if len(p.fields) == 1 {
				if noiseMatches(p.patterns, block) {
					drop = true
					break
				}
				continue
			}
			deeper = append(deeper, noisePath{fields: p.fields[1:], patterns: p.patterns})
		}
		if drop {
			continue
		}
		if len(deeper) > 0 {
			if open := indexFirstOpenBrace(block); open >= 0 {
				if closeIdx := findMatchingCloseBrace(block, open); closeIdx > open {
					inner := stripNoisyBlocks(block[open+1:closeIdx], deeper, depth+1)
					block = block[:open+1] + "\n" + inner + "\n" + block[closeIdx:]
				}
			}
		}
		kept = append(kept, block)
	}
	return strings.Join(kept, "\n")
}

// noiseMatches reports whether a field's block is noise: always with no
// patterns, else when its value matches one of them. The value is read as a
// user writes a pattern for it (protoscopeValue).
func noiseMatches(patterns []*regexp.Regexp, block string) bool {
	if len(patterns) == 0 {
		return true
	}
	value := protoscopeValue(block)
	for _, re := range patterns {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}

// protoscopeValue is the value of a "N: value" block as a pattern sees it:
// comments ("# 0x0i64") dropped; a string -- which protoscope writes as one
// or more quoted pieces in braces, splitting a long one across lines -- as
// the joined unquoted text; a number without protoscope's width suffix
// ("1.5i32" is 1.5). Anything else (a nested message) is its text.
func protoscopeValue(block string) string {
	v := strings.TrimSpace(block)
	if i := strings.Index(v, ":"); i >= 0 {
		v = strings.TrimSpace(v[i+1:])
	}
	v = strings.TrimSpace(stripProtoscopeComments(v))
	if strings.HasPrefix(v, "{") && strings.HasSuffix(v, "}") {
		if joined, ok := joinQuotedPieces(strings.TrimSpace(v[1 : len(v)-1])); ok {
			return joined
		}
	}
	if !strings.ContainsAny(v, " \t\n{") {
		for _, suf := range []string{"i64", "i32", "z"} {
			if t := strings.TrimSuffix(v, suf); t != v && t != "" && strings.IndexFunc(t, func(r rune) bool {
				return (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' && r != 'e' && r != 'E'
			}) < 0 {
				return t
			}
		}
	}
	return v
}

// stripProtoscopeComments drops "#" to the end of each line, outside quotes.
func stripProtoscopeComments(s string) string {
	var b strings.Builder
	inDq, esc := false, false
	skipping := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if skipping {
			if c == '\n' {
				skipping = false
				b.WriteByte(c)
			}
			continue
		}
		if inDq {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inDq = false
			}
			continue
		}
		switch c {
		case '"':
			inDq = true
			b.WriteByte(c)
		case '#':
			skipping = true
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// joinQuotedPieces reads s as only quoted strings separated by whitespace
// and returns them unquoted and joined; ok is false for anything else.
func joinQuotedPieces(s string) (string, bool) {
	var out strings.Builder
	rest := strings.TrimSpace(s)
	if rest == "" {
		return "", false
	}
	for rest != "" {
		if rest[0] != '"' {
			return "", false
		}
		end := -1
		for i := 1; i < len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if rest[i] == '"' {
				end = i
				break
			}
		}
		if end < 0 {
			return "", false
		}
		piece, err := strconv.Unquote(rest[:end+1])
		if err != nil {
			return "", false
		}
		out.WriteString(piece)
		rest = strings.TrimSpace(rest[end+1:])
	}
	return out.String(), true
}

// fieldPath is the field-number path of field n under prefix ("" at the top).
func fieldPath(prefix string, n int) string {
	if prefix == "" {
		return strconv.Itoa(n)
	}
	return prefix + "." + strconv.Itoa(n)
}
