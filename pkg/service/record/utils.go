package record

import (
	"context"
	"fmt"
	"net"
	"net/url"

	"strings"
	"time"
)

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
func replaceHostToIP(currentURL string, ipAddress string) (string, error) {
	// Parse the current URL
	parsedURL, err := url.Parse(currentURL)

	if err != nil {
		// Return the original URL if parsing fails
		return currentURL, err
	}

	if ipAddress == "" {
		return currentURL, fmt.Errorf("failed to replace url in case of docker env")
	}

	// Replace hostname with the IP address
	parsedURL.Host = strings.Replace(parsedURL.Host, parsedURL.Hostname(), ipAddress, 1)
	// Return the modified URL
	return parsedURL.String(), nil
}
