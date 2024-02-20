package replay

import (
	"fmt"
	"net/url"
	"strings"

	"go.keploy.io/server/v2/config"
)

type TestReportVerdict struct {
	total  int
	passed int
	failed int
}

func LeftJoinNoise(globalNoise config.GlobalNoise, tsNoise config.GlobalNoise) config.GlobalNoise {
	noise := globalNoise
	for field, regexArr := range tsNoise["body"] {
		noise["body"][field] = regexArr
	}
	for field, regexArr := range tsNoise["header"] {
		noise["header"][field] = regexArr
	}
	return noise
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
