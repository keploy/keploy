package mock

import (
	"fmt"
	"os"
)

// tlsPrivateKey is the private key keploy's agent presents, with the
// certificate a client sent it, when it dials the server for a client that
// authenticates with a certificate (mutual TLS): record.tlsPrivateKeyPath's
// file, read as keploy record reads it. "" when none is set. A path keploy
// cannot read is refused before anything runs, naming the setting.
func (m *mockService) tlsPrivateKey() (string, error) {
	path := m.config.Record.TLSPrivateKeyPath
	if path == "" {
		return "", nil
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("keploy could not read record.tlsPrivateKeyPath %q, the private key it presents with a client's certificate when it dials the server for that client (mutual TLS): %w", path, err)
	}
	return string(key), nil
}
