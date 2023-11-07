package scram

import (
	"crypto/hmac"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/xdg-go/scram"
)

// extractClientNonce extracts the nonce value from a SCRAM authentication first message.
//
// Parameters:
//   - firstMsg: The SCRAM authentication message string, which should contain key-value pairs
//     separated by commas, e.g., "n,,n=username,r=nonce".
//
// Returns:
// - The extracted nonce value as a string.
// - An error if the nonce ("r=") cannot be found in the provided message.
func extractClientNonce(firstMsg string) (string, error) {
	// Split the string based on ","
	parts := strings.Split(firstMsg, ",")

	// Iterate over the parts to find the one starting with "r="
	for _, part := range parts {
		if strings.HasPrefix(part, "r=") {
			// Split based on "=" and get the value of "r"
			value := strings.Split(part, "=")[1]
			return value, nil
		}
	}
	return "", errors.New("nonce not found")
}

// computeHMAC computes the HMAC (Hash-based Message Authentication Code) of the provided data
// using the specified hash generation function and key.
//
// Parameters:
// - hg: A function to generate the desired hash (like SHA-1 or SHA-256).
// - key: The secret key to use for the HMAC computation.
// - data: The input data for which the HMAC is to be computed.
//
// Returns:
// - A byte slice representing the computed HMAC value.
func computeHMAC(hg scram.HashGeneratorFcn, key, data []byte) []byte {
	mac := hmac.New(hg, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func mongoPasswordDigest(username, password string) string {
	// Ignore gosec warning "Use of weak cryptographic primitive". We need to use MD5 here to
	// implement the SCRAM specification.
	/* #nosec G401 */
	h := md5.New()
	_, _ = io.WriteString(h, username)
	_, _ = io.WriteString(h, ":mongo:")
	_, _ = io.WriteString(h, password)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func extractUsername(authMessage string) (string, error) {
	parts := strings.Split(authMessage, ",")
	for _, part := range parts {
		if strings.HasPrefix(part, "n=") {
			nValue := strings.TrimPrefix(part, "n=")
			return nValue, nil
		}
	}
	return "", fmt.Errorf("no username found in the auth message")
}

func extractAuthId(input string) (string, error) {
	re := regexp.MustCompile(`n,([^,]*),`) // Regular expression to match "n,," or "n,SOMETHING,"
	matches := re.FindStringSubmatch(input)
	if len(matches) >= 2 {
		return "n," + matches[1] + ",", nil
	}
	return "", fmt.Errorf("no match found")
}
