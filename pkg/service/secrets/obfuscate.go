package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"unicode"
)

// ObfuscationEngine replaces secrets with deterministic junk of the same charset
// and length. Uses HMAC(seed, value) for determinism — same input produces same
// output within a recording session, keeping cross-references between test cases
// and mocks consistent.
type ObfuscationEngine struct {
	seed []byte
}

// NewObfuscationEngine creates an engine with a random per-session seed.
// Returns an error if the OS entropy pool is unavailable.
func NewObfuscationEngine() (*ObfuscationEngine, error) {
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("crypto/rand.Read failed: OS entropy unavailable: %w", err)
	}
	return &ObfuscationEngine{seed: seed}, nil
}

// Process replaces the value with deterministic junk preserving charset and length.
func (o *ObfuscationEngine) Process(value string) (string, error) {
	return o.replacePreservingCharset(value), nil
}

func (o *ObfuscationEngine) replacePreservingCharset(original string) string {
	if original == "" {
		return original
	}

	mac := hmac.New(sha256.New, o.seed)
	mac.Write([]byte(original))
	hash := mac.Sum(nil)

	hashIdx := 0
	nextByte := func() byte {
		if hashIdx >= len(hash) {
			mac.Reset()
			mac.Write(hash)
			hash = mac.Sum(nil)
			hashIdx = 0
		}
		b := hash[hashIdx]
		hashIdx++
		return b
	}

	runes := []rune(original)
	result := make([]rune, len(runes))
	for i, r := range runes {
		b := nextByte()
		switch {
		case unicode.IsUpper(r):
			result[i] = rune('A' + int(b)%26)
		case unicode.IsLower(r):
			result[i] = rune('a' + int(b)%26)
		case unicode.IsDigit(r):
			result[i] = rune('0' + int(b)%10)
		default:
			result[i] = r
		}
	}
	return string(result)
}
