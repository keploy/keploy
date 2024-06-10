package record

import (
	"bufio"
	"context"
	"fmt"
	"go.keploy.io/server/v2/pkg/models"
	"net"
	"net/url"
	"os"

	"strings"
	"time"
)

func readMappings(filename string) (map[string]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	mappings := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		parts := strings.Split(line, ":")
		if len(parts) != 2 {
			fmt.Println("This is not in the correct format.")
			continue
		}
		mappings[parts[0]] = parts[1]
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return mappings, nil
}

func deleteElement(tcs []*models.TestCase, index string, val string) []*models.TestCase {
	if tcs == nil {
		return nil
	}
	var result []*models.TestCase
	for _, v := range tcs {
		if v.Name != index && v.Name != strings.TrimLeft(val, " ") {
			result = append(result, v)
		}
	}
	return result
}

func appendValues(tcs []*models.TestCase, key string, val string) []*models.TestCase {
	var newValues []*models.TestCase
	for _, tc := range tcs {
		if tc.Name == key || tc.Name == strings.TrimLeft(val, " ") {
			newValues = append(newValues, tc)
		}
	}
	return newValues
}

func extractHostAndPort(curlCmd string) (string, string, error) {
	// Split the command string to find the URL
	parts := strings.Split(curlCmd, " ")
	for _, part := range parts {
		if strings.HasPrefix(part, "http") {
			u, err := url.Parse(part)
			if err != nil {
				return "", "", err
			}
			host := u.Hostname()
			port := u.Port()
			if port == "" {
				if u.Scheme == "https" {
					port = "443"
				} else {
					port = "80"
				}
			}
			return host, port, nil
		}
	}
	return "", "", fmt.Errorf("no URL found in CURL command")
}

func waitForPort(ctx context.Context, host string, port string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:

			conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 1*time.Second)
			if err == nil {
				err := conn.Close()
				if err != nil {
					return err
				}
				return nil
			}
		}
	}
}
