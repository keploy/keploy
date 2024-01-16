package httpparser

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"github.com/fatih/color"
)

var (
	keyNotFoundColor  = color.New(color.FgHiBlue).SprintFunc()
	typeMismatchColor = color.New(color.FgYellow).SprintFunc()
	differenceColor   = color.New(color.FgRed, color.Bold).SprintFunc()
)

// to be used in mock assertion
func assertJSONReqBody(jsonBody1, jsonBody2 string) ([]string, error) {
	var data1, data2 map[string]interface{}
	var diffs []string

	if err := json.Unmarshal([]byte(jsonBody1), &data1); err != nil {
		return nil, fmt.Errorf("error unmarshalling JSON body 1: %v", err)
	}

	if err := json.Unmarshal([]byte(jsonBody2), &data2); err != nil {
		return nil, fmt.Errorf("error unmarshalling JSON body 2: %v", err)
	}

	// Recursive function to compare nested structures
	var compare func(interface{}, interface{}, string)
	compare = func(value1, value2 interface{}, path string) {
		switch v1 := value1.(type) {
		case map[string]interface{}:
			if v2, ok := value2.(map[string]interface{}); ok {
				for key := range v1 {
					newPath := fmt.Sprintf("%s.%s", path, key)
					if val2, ok := v2[key]; !ok {
						diffs = append(diffs, keyNotFoundColor(fmt.Sprintf("Key '%s' not found in second JSON body. Value in the first JSON body: %v", newPath, value1)))
					} else {
						compare(v1[key], val2, newPath)
					}
				}
			} else {
				diffs = append(diffs, typeMismatchColor(fmt.Sprintf("Type mismatch at '%s'. Expected map, actual %T. Value in the first JSON body: %v", path, value2, value1)))
			}
		default:
			// differenceColor(fmt.Sprintf("%s: Expected: %v, Actual: %v", path, value1, value2))
			if !reflect.DeepEqual(value1, value2) {
				diffs = append(diffs, keyNotFoundColor(fmt.Sprintf("%s: ", path))+typeMismatchColor(fmt.Sprintf(" Expected: %v", value1))+differenceColor(fmt.Sprintf(", Actual: %v", value2)))
			}
		}
	}

	compare(data1, data2, "")

	return diffs, nil
}

func ExtractChunkLength(chunkStr string) (int, error) {
	// fmt.Println("chunkStr::::", chunkStr)

	var Totalsize int = 0
	for {
		// Find the position of the first newline
		pos := strings.Index(chunkStr, "\r\n")
		if pos == -1 {
			break
		}

		// Extract the chunk size string
		sizeStr := chunkStr[:pos]

		// Parse the hexadecimal size
		var size int
		_, err := fmt.Sscanf(sizeStr, "%x", &size)
		if err != nil {
			fmt.Println("Error parsing size:", err)
			break
		}

		fmt.Printf("Chunk size: %d\n", size)
		Totalsize = Totalsize + size

		// Check for the last chunk (size 0)
		if size == 0 {
			break
		}

		// Skip past this chunk in the string
		chunkStr = chunkStr[pos+2+size*2+2:] // +2 for \r\n after size, +size*2 for the chunk data, +2 for \r\n after data
	}
	return Totalsize, nil
}

// countHTTPChunks takes a buffer containing a chunked HTTP response and returns the total number of chunks.
func countHTTPChunks(buffer []byte) (int, error) {
	reader := bufio.NewReader(bytes.NewReader(buffer))
	chunkCount := 0

	for {
		// Read the next chunk size line.
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			// End of the buffer, break the loop.
			break
		}
		if err != nil {
			return 0, err // Handle the error.
		}

		// Strip the line of \r\n and check if it's empty.
		sizeStr := strings.TrimSpace(line)
		if sizeStr == "" {
			continue // Skip empty lines.
		}

		// Parse the hexadecimal number.
		size, err := strconv.ParseInt(sizeStr, 16, 64)
		if err != nil {
			return 0, err // Handle the error.
		}

		if size == 0 {
			// Last chunk is of size 0.
			break
		}

		// Skip the chunk data and the trailing \r\n.
		_, err = reader.Discard(int(size + 2))
		if err != nil {
			return 0, err // Handle the error.
		}

		chunkCount++
	}

	return chunkCount, nil
}
