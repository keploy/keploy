package genericparser

import (
	"encoding/base64"
	// "fmt"
	"unicode"

	"github.com/agnivade/levenshtein"
	"github.com/cloudflare/cfssl/log"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
)

func PostgresDecoder(encoded string) ([]byte, error) {
	// decode the base 64 encoded string to buffer ..

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// fmt.Println(hooks.Emoji+"failed to decode the data", err)
		return nil, err
	}
	// println("Decoded data is :", string(data))
	return data, nil
}

func fuzzymatch(tcsMocks []*models.Mock, requestBuffers [][]byte, h *hooks.Hook) (bool, []models.GenericPayload) {

	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(requestBuffers) {
			for requestIndex, reqBuff := range requestBuffers {

				// bufStr := string(reqBuff)
				// if !IsAsciiPrintable(bufStr) {
				bufStr := base64.StdEncoding.EncodeToString(reqBuff)
				// }
				encoded, _ := PostgresDecoder(mock.Spec.GenericRequests[requestIndex].Message[0].Data)

				if string(encoded) == string(reqBuff) || mock.Spec.GenericRequests[requestIndex].Message[0].Data == bufStr {
					log.Debug("matched in first loop")
					tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
					h.SetTcsMocks(tcsMocks)
					return true, mock.Spec.GenericResponses
				}
			}
		}
	}
	// com := PostgresEncoder(reqBuff)
	// convert all the configmocks to string array
	// mockString := make([]string, len(tcsMocks))
	// for i := 0; i < len(tcsMocks); i++ {
	// 	mockString[i] = string(tcsMocks[i].Spec.PostgresReq.Payload)
	// }
	// // find the closest match
	// if IsAsciiPrintable(string(reqBuff)) {
	// 	fmt.Println("Inside String Match")
	// 	idx := findStringMatch(string(reqBuff), mockString)
	// 	if idx != -1 {
	// 		nMatch := tcsMocks[idx].Spec.PostgresResp.Payload
	// 		tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
	// 		h.SetConfigMocks(tcsMocks)
	// 		fmt.Println("Returning mock from String Match !!")
	// 		return true, nMatch
	// 	}
	// }
	idx := findBinaryMatch(tcsMocks, requestBuffers, h)
	if idx != -1 {
		log.Debug("matched in first loop")
		bestMatch := tcsMocks[idx].Spec.GenericResponses
		tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
		h.SetTcsMocks(tcsMocks)
		return true, bestMatch
	}
	return false, nil
}

func findBinaryMatch(tcsMocks []*models.Mock, requestBuffers [][]byte, h *hooks.Hook) int {

	mxSim := -1.0
	mxIdx := -1
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(requestBuffers) {
			for requestIndex, reqBuff := range requestBuffers {

				// bufStr := string(reqBuff)
				// if !IsAsciiPrintable(bufStr) {
				_ = base64.StdEncoding.EncodeToString(reqBuff)
				// }
				encoded, _ := PostgresDecoder(mock.Spec.GenericRequests[requestIndex].Message[0].Data)

				k := AdaptiveK(len(reqBuff), 3, 8, 5)
				shingles1 := CreateShingles(encoded, k)
				shingles2 := CreateShingles(reqBuff, k)
				similarity := JaccardSimilarity(shingles1, shingles2)
				log.Debugf(hooks.Emoji, "Jaccard Similarity:%f\n", similarity)

				if mxSim < similarity {
					mxSim = similarity
					mxIdx = idx
				}
			}
		}
	}
	return mxIdx
}

// CreateShingles produces a set of k-shingles from a byte buffer.
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

func AdaptiveK(length, kMin, kMax, N int) int {
	k := length / N
	if k < kMin {
		return kMin
	} else if k > kMax {
		return kMax
	}
	return k
}

// checks if s is ascii and printable, aka doesn't include tab, backspace, etc.
func IsAsciiPrintable(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func findStringMatch(req []string, mockString []string) int {
	minDist := int(^uint(0) >> 1) // Initialize with max int value
	bestMatch := -1
	for idx, req := range mockString {
		if !IsAsciiPrintable(mockString[idx]) {
			continue
		}

		dist := levenshtein.ComputeDistance(req, mockString[idx])
		if dist == 0 {
			return 0
		}

		if dist < minDist {
			minDist = dist
			bestMatch = idx
		}
	}
	return bestMatch
}
