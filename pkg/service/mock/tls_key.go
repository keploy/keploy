package mock

import (
	"fmt"
	"os"

	"go.keploy.io/server/v3/pkg/kubeconfig"
	"go.keploy.io/server/v3/utils"
)

// tlsPrivateKey is the private keys keploy's agent may present, with the
// certificate a client sent it, when it dials the server for a client that
// authenticates with a certificate (mutual TLS): record.tlsPrivateKeyPath's
// file, read as keploy record reads it, then, when the test command runs on
// this machine, the client keys of the kubeconfig's users (kubectl and helm
// send their user's certificate). "" when there is none. A path keploy
// cannot read is refused before anything runs, naming the setting.
func (m *mockService) tlsPrivateKey() (string, error) {
	key := ""
	if path := m.config.Record.TLSPrivateKeyPath; path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("keploy could not read record.tlsPrivateKeyPath %q, the private key it presents with a client's certificate when it dials the server for that client (mutual TLS): %w", path, err)
		}
		key = string(b)
	}
	return kubeconfig.WithClientKeys(m.logger, key, utils.IsDockerCmd(utils.CmdType(m.config.CommandType))), nil
}
