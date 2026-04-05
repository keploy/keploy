package utils

import (
	"context"
	"fmt"
	"net"
	"time"

	"go.uber.org/zap"
)

func WaitForPort(ctx context.Context, host string, port uint32, interval time.Duration, logger *zap.Logger) error {
	if host == "" {
		host = "localhost"
	}

	address := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	waitMsgLogged := false
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		conn, err := net.DialTimeout("tcp", address, interval)
		if err == nil {
			_ = conn.Close()
			logger.Info(fmt.Sprintf("Application detected on port %d. Resuming...", port))
			return nil
		}

		if !waitMsgLogged {
			logger.Info(fmt.Sprintf("Waiting for application on port %d...", port))
			waitMsgLogged = true
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
