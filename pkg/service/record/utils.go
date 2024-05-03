package record

import (
	"context"

	"net"

	"time"
)

func waitForPort(ctx context.Context, host, port string) error {
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
