// Trimmed from github.com/zricethezav/gitleaks/v8 v8.28.0, packages detect and
// report. MIT License, Copyright (c) 2019 Zachary Rice: see LICENSE in this
// directory.

package secretscan

import (
	"math"
	"strings"
)

// Finding is a secret a rule found. Lines count from 0, columns from 1; the
// position is that of Match, of which Secret is a part.
type Finding struct {
	RuleID      string
	Description string

	StartLine   int
	EndLine     int
	StartColumn int
	EndColumn   int

	// Line is the line or lines that hold Match.
	Line string

	Match  string
	Secret string

	Entropy float32
	Tags    []string
}

// shannonEntropy returns the Shannon entropy of data, in bits per character
// (https://en.wiktionary.org/wiki/Shannon_entropy).
func shannonEntropy(data string) (entropy float64) {
	if data == "" {
		return 0
	}

	charCounts := make(map[rune]int)
	for _, char := range data {
		charCounts[char]++
	}

	invLength := 1.0 / float64(len(data))
	for _, count := range charCounts {
		freq := float64(count) * invLength
		entropy -= freq * math.Log2(freq)
	}

	return entropy
}

// filter drops a finding of a generic rule when a finding of a rule that is
// not generic, on the same line, holds its secret.
func filter(findings []Finding) []Finding {
	var kept []Finding
	for _, f := range findings {
		include := true
		if strings.Contains(strings.ToLower(f.RuleID), "generic") {
			for _, fPrime := range findings {
				if f.StartLine == fPrime.StartLine &&
					f.RuleID != fPrime.RuleID &&
					strings.Contains(fPrime.Secret, f.Secret) &&
					!strings.Contains(strings.ToLower(fPrime.RuleID), "generic") {
					include = false
					break
				}
			}
		}
		if include {
			kept = append(kept, f)
		}
	}
	return kept
}
