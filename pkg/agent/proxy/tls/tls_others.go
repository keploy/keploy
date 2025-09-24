//go:build !linux

package tls

func ExtractCertToTemp() (string, error) {
	return "", nil
}
