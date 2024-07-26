//go:build linux

package connection

import (
	"context"
	"fmt"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_next_factor_request.html

func DecodeAuthNextFactor(_ context.Context, _ []byte) error {
	return fmt.Errorf("multi factor authentication is not supported")
}
