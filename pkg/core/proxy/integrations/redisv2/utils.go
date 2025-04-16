package redisv2

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/davecgh/go-spew/spew"
	"go.keploy.io/server/v2/pkg/models"
)

func findProtocolVersion(reqBuf []byte) (int, error) {
	var redisProtocolVersion int
	bufStr := string(reqBuf)
	if len(bufStr) > 0 {
		// Check if 'ping' is in the buffer, version 2
		if strings.Contains(bufStr, "ping") {
			redisProtocolVersion = 2
		} else if strings.Contains(bufStr, "hello") {
			// If "hello" is found, extract the next value after it to determine the version
			// Find the start of the next value (after "hello" and CRLF characters)
			startIndex := strings.Index(bufStr, "hello") + len("hello") + 6 // Skip "hello" and CRLF
			if startIndex < len(bufStr) {
				// Extract the value after "hello" to determine the protocol version
				version, err := strconv.Atoi(bufStr[startIndex : startIndex+1]) // Adjust depending on the expected protocol version format
				redisProtocolVersion = version
				if err != nil {
					// Handle error if conversion fails
					return 0, fmt.Errorf("failed to convert protocol version to int: %w", err)
				}
			} else {
				// Handle case where version after "hello" is not found
				return 0, fmt.Errorf("no protocol version found after hello")
			}
		}
	}
	return redisProtocolVersion, nil
}

func removeCRLF(data string) string {
	return strings.ReplaceAll(data, "\r\n", "")
}

// Process an array type
func processArray(bufStr string) []models.RedisArrayBody {
	// Remove CRLF characters first
	// bufStr = removeCRLF(bufStr)

	// Slice the string from index 2 (remove the first two characters, e.g. "*2\r\n")
	// bufStr = bufStr[2:]

	// Initialize a slice to store the result
	var result []models.RedisArrayBody

	// Split the array by "$" to process each element
	dataParts := strings.Split(bufStr, "$")
	spew.Dump("first datapaerts",dataParts)
	for i := 1; i < len(dataParts); i += 1 {
		// Extract the size from the first part, e.g., "$3" means size 3
		if len(dataParts[i]) > 0 {
			var newArrayEntry models.RedisArrayBody
			// fmt.Println(dataParts[i],"kkllllll")
			// sizeStr := dataParts[i][:strings.Index(dataParts[i], "\r\n")] // Extract the size part, e.g., "$3"
			re := regexp.MustCompile(`\d+`)
			loc := re.FindString(dataParts[i])
			fmt.Println("here is loc",loc)
			// size, err := strconv.Atoi(sizeStr)
			// if err != nil {
			// 	fmt.Println("Error parsing size:", err)
			// 	continue
			// }
			
			// Extract the value (skip CRLF) and get the element
			// fmt.Println(dataParts)
			// fmt.Println(dataParts[i])
			element := dataParts[i:] // The actual data part after '$'
			arrayEntryLength,err := strconv.Atoi(loc)
			if err !=nil{
				return nil
			}
			// fmt.Println("element",element[0])
			newArrayEntry.Length = arrayEntryLength
			// removeBeforeFirstCRLF(element[0])
			newArrayEntry.Value = removeCRLF(removeBeforeFirstCRLF(element[0]))
			// Format the result as YAML
			result = append(result, newArrayEntry)
		}
	}

	// Join and return the formatted YAML string
	return result
}

// Process a map type
func processMap(bufStr string) map[string]models.RedisBodyType {
	fmt.Println("here is bufStr in map",bufStr)
	result := make(map[string]models.RedisBodyType)
	// Example: "%2\r\n$3\r\nkey\r\n$5\r\nvalue\r\n"

	dataParts := strings.Split(bufStr, "$")
	for i := 1; i < len(dataParts)-1; i += 2 {
		// Skip the size part (e.g., "$3" or "$5")
		key := dataParts[i][1:]     // The actual key after '$'
		value := dataParts[i+1][1:] // The actual value after '$'
		result[key] = models.RedisBodyType{
			Type: "string",
			Data: value,
		}
	}
	return result
}

// Handle data by type (array, map, or string)
func handleDataByType(dataType, data string) interface{} {
	switch dataType {
	case "array":
		// Process array data and convert it into a readable format (e.g., split by $n and data)
		return processArray(data)
		// return data
	case "map":
		// Process map data and extract key-value pairs
		// return processMap(data)
		return data
	case "string":
		// Just return the cleaned string
		return data
	default:
		return data
	}
}

func removeBeforeFirstCRLF(data string) string {
	// Find the index of the first occurrence of CRLF ("\r\n")
	crlfIndex := strings.Index(data, "\r\n")
	if crlfIndex == -1 {
		// If no CRLF is found, return the original data (no change)
		return data
	}
	// Slice the string starting from the position after the CRLF
	return data[crlfIndex+2:] // +2 to skip over the CRLF characters
}

func getBeforeFirstCRLF(data string) string {
	// Find the index of the first occurrence of CRLF ("\r\n")
	crlfIndex := strings.Index(data, "\r\n")
	if crlfIndex == -1 {
		// If no CRLF is found, return the original data (no change)
		return data
	}
	// Slice the string up to the position before the CRLF
	return data[:crlfIndex]
}
