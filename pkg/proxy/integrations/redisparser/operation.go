package redisparser

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	separator = []byte("\r\n")
)

func DecodeRedisResponse(response string) (string, error) {
	if len(response) == 0 {
		return "", nil
	}

	reader := bufio.NewReader(strings.NewReader(response))

	// Read the first character to determine the type of response
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}

	switch line[0] {
	case '+': // Simple string
		return strings.TrimSuffix(line[1:], "\r\n"), nil
	case '-': // Error
		return "", errors.New(strings.TrimSuffix(line[1:], "\r\n"))
	case '$': // Bulk string
		lengthStr := strings.TrimSuffix(line[1:], "\r\n")
		length, err := strconv.Atoi(lengthStr)
		if err != nil {
			return "", err
		}
		data := make([]byte, length)
		_, err = reader.Read(data)
		if err != nil {
			return "", err
		}
		// Read additional two characters for trailing \r\n
		_, err = reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return string(data), nil
	case ':': // Integer
		return strings.TrimSuffix(line[1:], "\r\n"), nil
	default:
		return "", errors.New("unknown response type")
	}
}

func DecodeRedisRequest(request string) ([]string, error) {
	elements := strings.Split(request, "\r\n")

	var result []string
	var i int
	var length int
	for i = 1; i < len(elements)-1; i++ {
		element := elements[i]
		if strings.HasPrefix(element, "$") {
			fmt.Sscanf(element, "$%d", &length)
			if i+1 < len(elements) {
				result = append(result, elements[i+1])
				i++ // Skip next item
			} else {
				return nil, fmt.Errorf("invalid format")
			}
		}
	}

	return result, nil
}
