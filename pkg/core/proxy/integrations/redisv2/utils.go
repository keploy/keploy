package redisv2

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/resp"
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
func processArray(bufStr string) ([]models.RedisElement, error) {
	// Initialize a slice to store the result
	var result []models.RedisElement

	// Create a reader for the RESP data
	reader := resp.NewReader(strings.NewReader(bufStr))

	// Read the value from the RESP data
	v, _, err := reader.ReadValue()
	if err != nil {
		return nil, fmt.Errorf("error reading RESP value: %w", err)
	}

	// Check if the value is an array
	if v.Type() == resp.Array {
		// Iterate over each element in the array
		for _, elem := range v.Array() {
			var newArrayEntry models.RedisElement

			// Handle different types of elements in the array (strings, integers, etc.)
			switch elem.Type() {
			case resp.BulkString:
				newArrayEntry.Length = len(elem.String())
				newArrayEntry.Value = elem.String()
			case resp.Integer:
				newArrayEntry.Length = len(fmt.Sprintf("%d", elem.Integer()))
				newArrayEntry.Value = elem.Integer()
			case resp.SimpleString:
				newArrayEntry.Length = 1
				newArrayEntry.Value = elem.String()
			case resp.Array:
				// Recursively process nested arrays
				nestedArray, err := processArray(elem.String()) // Call processArray recursively
				if err != nil {
					return nil, err
				}
				newArrayEntry.Length = len(nestedArray)
				newArrayEntry.Value = nestedArray
			default:
				newArrayEntry.Length = 0
				newArrayEntry.Value = nil
			}

			// Append the processed entry to the result
			result = append(result, newArrayEntry)
		}
	} else {
		return nil, fmt.Errorf("expected RESP Array, but got %s", v.Type())
	}

	return result, nil
}

// Process a map type
func processMap(bufStr string) []models.RedisMapBody {
	// Remove CRLF characters first
	// bufStr = removeCRLF(bufStr)

	// Initialize a slice to store the result
	var result []models.RedisMapBody

	dataParts := splitByMultipleDelimiters(bufStr)
	for i := 1; i < len(dataParts); i += 1 { // Move in steps of 2 (key, then value)
		// Extract the size from the first part, e.g., "$3" means size 3
		if len(dataParts[i]) > 0 {
			var newMapEntry models.RedisMapBody

			// Using regex to capture the size part (the number before \r\n)
			re := regexp.MustCompile(`\d+`)
			loc := re.FindString(dataParts[i]) // Extract the size from "$n"
			fmt.Println("Extracted Size:", loc)

			// Convert the size to an integer
			keyLength, err := strconv.Atoi(loc)
			if err != nil {
				fmt.Println("Error parsing size:", err)
				continue
			}

			// fmt.Println("check1")
			// spew.Dump(dataParts[i])
			// fmt.Println("check2")
			// spew.Dump(dataParts[i+1])
			// fmt.Println("check3")

			// Extract the key (skip CRLF) and get the actual data part after '$'
			// TODO: work for numbers also and those that do not have $ for size
			key := dataParts[i] // The actual key part after "$n"
			fmt.Println("to check number", key)
			// for numbers we do not do the below we just remove :
			if key[0] == ':' {
				key = key[1:] // Remove the colon if it exists at the beginning
			} else {
				key = removeBeforeFirstCRLF(key) // Remove CRLF from the key
			}
			key = removeCRLF(key)

			// Assign the key to the map entry
			newMapEntry.Key = models.RedisElement{
				Length: keyLength,
				Value:  key,
			}

			// Move to the next data part (which will be the value)
			i++

			// Extract the value
			if i+1 < len(dataParts) {
				fmt.Println("to check number of value", dataParts[i])
				var valueSizeStr string
				if dataParts[i][0] == ':' {
					valueSizeStr = dataParts[i][1:]
				} else {
					valueSizeStr = removeBeforeFirstCRLF(dataParts[i]) // This is the size of the value
				}
				valueSizeStr = removeCRLF(valueSizeStr)
				// valueSize, err := strconv.Atoi(valueSizeStr)
				// if err != nil {
				// 	fmt.Println("Error parsing value size:", err)
				// 	continue
				// }

				// Extract the actual value
				// value := dataParts[i] // The value part after "$n"
				// value = removeCRLF(value)

				// Assign the value to the map entry
				newMapEntry.Value = models.RedisElement{
					Length: len(valueSizeStr),
					Value:  valueSizeStr,
				}
			}

			// Append the map entry to the result
			result = append(result, newMapEntry)
		}
	}
	// Return the result
	return result
}

// Handle data by type (array, map, or string)
func handleDataByType(dataType, data string) interface{} {
	switch dataType {
	case "array":
		// Process array data and convert it into a readable format (e.g., split by $n and data)
		val, err := processArray(data)
		if err != nil {
			return fmt.Errorf("invalid array processing")
		}
		return val
	case "map":
		// Process map data and extract key-value pairs
		return processMap(data)
		// return data
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

func splitByMultipleDelimiters(input string) []string {
	// Initialize a slice to hold the result
	var result []string

	// Initialize a temporary string to accumulate data before the delimiter
	var currentString string

	// Iterate through the string and find delimiters
	for _, r := range input {
		// If a delimiter is found, we add the delimiter to the current string
		if r == '$' || r == ':' || r == '*' || r == '%' {
			// If there's any accumulated string, add it first
			if len(currentString) > 0 {
				result = append(result, currentString)
				currentString = "" // Reset the current string
			}
			// Add the delimiter to the current string
			currentString += string(r)

		} else {
			// Otherwise, accumulate the characters
			currentString += string(r)
		}
	}

	// If there's any remaining data after the last delimiter, add it
	if len(currentString) > 0 {
		result = append(result, currentString)
	}

	return result
}
