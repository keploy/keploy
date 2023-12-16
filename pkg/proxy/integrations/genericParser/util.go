package genericparser

import (
	"encoding/base64"
	// "fmt"
	"unicode"
	"sort"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.keploy.io/server/pkg/proxy/util"
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
	sort.Slice(tcsMocks, func(i, j int) bool {
		return tcsMocks[i].Spec.ReqTimestampMock.Before(tcsMocks[j].Spec.ReqTimestampMock)
	})
	for idx, mock := range tcsMocks {
		if len(mock.Spec.GenericRequests) == len(requestBuffers) {
			matched := true // Flag to track if all requests match

			for requestIndex, reqBuff := range requestBuffers {
				bufStr := string(reqBuff)
				if !IsAsciiPrintable(string(reqBuff)) {
					bufStr = base64.StdEncoding.EncodeToString(reqBuff)
				}

				encoded := []byte(mock.Spec.GenericRequests[requestIndex].Message[0].Data)
				if !IsAsciiPrintable(mock.Spec.GenericRequests[requestIndex].Message[0].Data) {
					encoded, _ = PostgresDecoder(mock.Spec.GenericRequests[requestIndex].Message[0].Data)
				}

				// Compare the encoded data
				if string(encoded) != string(reqBuff) || mock.Spec.GenericRequests[requestIndex].Message[0].Data != bufStr {
					matched = false
					break // Exit the loop if any request doesn't match
				}
			}

			if matched {
				tcsMocks = append(tcsMocks[:idx], tcsMocks[idx+1:]...)
				h.SetTcsMocks(tcsMocks)
				return true, mock.Spec.GenericResponses
			}
		}
	}

	return false, nil
}

// checks if s is ascii and printable, aka doesn't include tab, backspace, etc.
func IsAsciiPrintable(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || (!unicode.IsPrint(r) && r != '\r' && r != '\n') {
			return false
		}
	}
	return true
}