// Package util provides utility functions for the integration package.
package util

import (
	"encoding/base64"
	"unicode"
)

func IsASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

func DecodeBase64(encoded string) ([]byte, error) {
	// Decode the base64 encoded string to buffer
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func EncodeBase64(decoded []byte) string {
	// Encode the []byte string to encoded string
	return base64.StdEncoding.EncodeToString(decoded)
}

// Functions related to fuzzy matching

func AdaptiveK(length, min, max, N int) int {
	k := length / N
	if k < min {
		return min
	} else if k > max {
		return max
	}
	return k
}

func CreateShingles(data []byte, k int) map[string]struct{} {
	shingles := make(map[string]struct{})
	for i := 0; i < len(data)-k+1; i++ {
		shingle := string(data[i : i+k])
		shingles[shingle] = struct{}{}
	}
	return shingles
}

// JaccardSimilarity computes the Jaccard similarity between two sets of shingles.
func JaccardSimilarity(setA, setB map[string]struct{}) float64 {
	intersectionSize := 0
	for k := range setA {
		if _, exists := setB[k]; exists {
			intersectionSize++
		}
	}

	unionSize := len(setA) + len(setB) - intersectionSize

	if unionSize == 0 {
		return 0.0
	}
	return float64(intersectionSize) / float64(unionSize)
}
