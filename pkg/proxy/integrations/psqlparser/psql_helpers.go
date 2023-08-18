package psqlparser

import (
	"encoding/base64"
	"fmt"
	"net"

	// "time"
	// "sync"
	// "strings"

	// "encoding/json"

	// "fmt"
	"go.keploy.io/server/pkg/proxy/util"
	// "bytes"
	"encoding/binary"
	"errors"
	"io"
	"unicode"

	"github.com/agnivade/levenshtein"
	"go.keploy.io/server/pkg/hooks"
	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

func remainingBits(superset, subset []byte) []byte {
	// Find the length of the smaller array (subset).
	subsetLen := len(subset)

	// Initialize a result buffer to hold the differences.
	var difference []byte

	// Iterate through each byte in the 'subset' array.
	for i := 0; i < subsetLen; i++ {
		// Compare the bytes at the same index in both arrays.
		// If they are different, append the byte from 'superset' to the result buffer.
		if superset[i] != subset[i] {
			difference = append(difference, superset[i])
		}
	}

	// If 'superset' is longer than 'subset', append the remaining bytes to the result buffer.
	if len(superset) > subsetLen {
		difference = append(difference, superset[subsetLen:]...)
	}

	return difference
}

func startProxy(buffer []byte, clientConn, destConn net.Conn, logger *zap.Logger, h *hooks.Hook) []*models.Mock {

	logger.Info(Emoji + "Encoding outgoing Postgres call !!")
	// write the request message to the postgres server
	_, err := destConn.Write(buffer)
	if err != nil {
		logger.Error(Emoji+"failed to write the request buffer to postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
		return nil
	}

	// // read reply message from the postgres server
	responseBuffer, err := util.ReadBytes(destConn)
	if err != nil {
		logger.Error(Emoji+"failed to read reply from the postgres server", zap.Error(err), zap.String("postgres server address", destConn.RemoteAddr().String()))
		return nil
	}

	// write the reply to postgres client
	_, err = clientConn.Write(responseBuffer)
	if err != nil {
		logger.Error(Emoji+"failed to write the reply message to postgres client", zap.Error(err))
		return nil
	}

	clientToDest := make(chan []byte)
	destToClient := make(chan []byte)

	// Client to Destination communication
	go func() {
		defer clientConn.Close()
		for data := range clientToDest {
			_, err := destConn.Write(data)
			if err != nil {
				fmt.Printf("Error writing to destination server: %v", err)
				return
			}
		}
	}()

	// Destination to Client communication
	go func() {
		defer destConn.Close()
		for {
			bytesRead, err := destConn.Read(buffer)
			if err != nil {
				if err != io.EOF {
					fmt.Printf("Error reading from destination server: %v", err)
				}
				return
			}
			destToClient <- buffer[:bytesRead]
		}
	}()

	// Main loop for client-destination server communication
	for {
		bytesRead, err := clientConn.Read(buffer)
		if err != nil {
			if err != io.EOF {
				fmt.Printf("Error reading from client: %v", err)
			}
			return nil
		}
		clientToDest <- buffer[:bytesRead]

		// Read from destToClient channel and send back to client
		response := <-destToClient
		_, err = clientConn.Write(response)
		if err != nil {
			fmt.Printf("Error writing to client: %v", err)
			return nil
		}
	}
}

func PostgresDecoder(encoded string) ([]byte, error) {
	// decode the base 64 encoded string to buffer ..

	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		fmt.Println(Emoji+"failed to decode the data", err)
		return nil, err
	}
	// println("Decoded data is :", string(data))
	return data, nil
}

func PostgresEncoder(buffer []byte) string {
	// encode the buffer to base 64 string ..
	encoded := base64.StdEncoding.EncodeToString(buffer)
	return encoded
}

func IdentifyPacket(data []byte) (models.Packet, error) {
	// At least 4 bytes are required to determine the length
	if len(data) < 4 {
		return nil, errors.New("data too short")
	}

	// Read the length (first 32 bits)
	length := binary.BigEndian.Uint32(data[:4])

	// If the length is followed by the protocol version, it's a StartupPacket
	if length > 4 && len(data) >= int(length) && binary.BigEndian.Uint32(data[4:8]) == models.ProtocolVersionNumber {
		return &models.StartupPacket{
			Length:          length,
			ProtocolVersion: binary.BigEndian.Uint32(data[4:8]),
		}, nil
	}

	// If we have an ASCII identifier, then it's likely a regular packet. Further validations can be added.
	if len(data) > 5 && len(data) >= int(length)+1 {
		return &models.RegularPacket{
			Identifier: data[4],
			Length:     length,
			Payload:    data[5 : length+1],
		}, nil
	}

	return nil, errors.New("unknown packet type or data too short for declared length")
}

// improve the matching by the headers , and the body of the request. Use pgproto3 library to encode and decode
func tempMatching(configMocks, tcsMocks []*models.Mock, reqBuff []byte, h *hooks.Hook) (bool, string) {
	//first check if the request is a startup packet
	com := PostgresEncoder(reqBuff)

	for idx, mock := range configMocks {
		encoded, _ := PostgresDecoder(mock.Spec.PostgresReq.Payload)
		// if _, ok := maps[mock.Spec.PostgresResp.Payload]; ok {
		// 	continue
		// }
		if string(encoded) == string(reqBuff) || mock.Spec.PostgresReq.Payload == com {
			fmt.Println("matched in first loop")

			configMocks = append(configMocks[:idx], configMocks[idx+1:]...)
			h.SetConfigMocks(configMocks)
			return true, mock.Spec.PostgresResp.Payload
		}
		i := 0
		// instead of this match with actual data
		for i = 0; i < len(com); i++ {
			if com[i] != mock.Spec.PostgresReq.Payload[i] {
				break
			}
		}
		if i >= 8 {
			fmt.Println("matched in second loop")
			configMocks = append(configMocks[:idx], configMocks[idx+1:]...)
			h.SetConfigMocks(configMocks)
			return true, mock.Spec.PostgresResp.Payload
		}
	}

	// fmt.Println("encoded request is :",com)
	// from mocks
	for _, mock := range tcsMocks {
		encoded, _ := PostgresDecoder(mock.Spec.PostgresReq.Payload)
		if string(encoded) == string(reqBuff) {
			return true, mock.Spec.PostgresResp.Payload
		}
		i := 0
		for i = 0; i < len(com); i++ {
			if com[i] == mock.Spec.PostgresReq.Payload[i] {
				fmt.Println("matched in second loop")
			}
		}
		if i >= len(com)/2 {
			return true, mock.Spec.PostgresResp.Payload
		}
	}

	return false, ""
}

type Request struct {
	Data     []byte
	IsString bool
}

func findStringMatch(req string, mockString []string) int {
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

func IsAsciiPrintable(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func Fuzzymatch(configMocks, tcsMocks []*models.Mock, reqBuff []byte, h *hooks.Hook) (bool, string) {
	com := PostgresEncoder(reqBuff)
	for idx, mock := range configMocks {
		encoded, _ := PostgresDecoder(mock.Spec.PostgresReq.Payload)

		if string(encoded) == string(reqBuff) || mock.Spec.PostgresReq.Payload == com {
			fmt.Println("matched in first loop")
			configMocks = append(configMocks[:idx], configMocks[idx+1:]...)
			h.SetConfigMocks(configMocks)
			return true, mock.Spec.PostgresResp.Payload
		}
	}
	// convert all the configmocks to string array
	mockString := make([]string, len(configMocks))
	for i := 0; i < len(configMocks); i++ {
		mockString[i] = string(configMocks[i].Spec.PostgresReq.Payload)
	}
	// find the closest match
	if IsAsciiPrintable(string(reqBuff)) {
		fmt.Println("Inside String Match")
		idx := findStringMatch(string(reqBuff), mockString)
		if idx != -1 {
			nMatch := configMocks[idx].Spec.PostgresResp.Payload
			configMocks = append(configMocks[:idx], configMocks[idx+1:]...)
			h.SetConfigMocks(configMocks)
			fmt.Println("Returning mock from String Match !!")
			return true, nMatch
		}
	}
	idx := findBinaryMatch(configMocks, reqBuff, h)
	if idx != -1 {
		nMatch := configMocks[idx].Spec.PostgresResp.Payload
		configMocks = append(configMocks[:idx], configMocks[idx+1:]...)
		h.SetConfigMocks(configMocks)
		return true, nMatch
	}
	return false, ""
}

func findBinaryMatch(configMocks []*models.Mock, reqBuff []byte, h *hooks.Hook) int {

	mxSim := -1.0
	mxIdx := -1
	// find the fuzzy hash of the mocks
	for idx, mock := range configMocks {
		encoded, _ := PostgresDecoder(mock.Spec.PostgresReq.Payload)
		k := AdaptiveK(len(reqBuff), 3, 8, 5)
		shingles1 := CreateShingles(encoded, k)
		shingles2 := CreateShingles(reqBuff, k)
		similarity := JaccardSimilarity(shingles1, shingles2)
		fmt.Printf("Jaccard Similarity: %f\n", similarity)
		if mxSim < similarity {
			mxSim = similarity
			mxIdx = idx
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
