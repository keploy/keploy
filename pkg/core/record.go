//go:build linux

package core

import (
	"context"

	"go.keploy.io/server/v2/pkg/models"
)

// GetIncoming starts recording incoming network calls.
// It differentiates between gRPC and other (HTTP) traffic based on the trafficType hint.
func (c *Core) GetIncoming(ctx context.Context, id uint64, opts models.IncomingOptions, trafficType string) (<-chan *models.TestCase, error) {
	// TODO: The trafficType parameter is a temporary solution.
	// A more robust mechanism for traffic differentiation might be needed,
	// possibly by inspecting initial bytes or relying on port configurations.
	if trafficType == "grpc" {
		// Call the new gRPC recording method in Hooks.
		// This method is currently a placeholder.
		return c.Hooks.RecordGRPC(ctx, id, opts)
	}
	// Default to existing HTTP/generic recording.
	return c.Hooks.Record(ctx, id, opts)
}

// GetOutgoing starts recording outgoing network calls (mocks).
// It differentiates between gRPC and other (HTTP) traffic based on the trafficType hint.
func (c *Core) GetOutgoing(ctx context.Context, id uint64, opts models.OutgoingOptions, trafficType string) (<-chan *models.Mock, error) {
	m := make(chan *models.Mock, 500) // Buffer size can be configured if needed.

	// TODO: The trafficType parameter is a temporary solution.
	if trafficType == "grpc" {
		// Call the new gRPC mock recording method in Proxy.
		// This method is currently a placeholder.
		err := c.Proxy.RecordGRPCMock(ctx, id, m, opts)
		if err != nil {
			close(m) // Ensure channel is closed on error to prevent goroutine leaks for senders.
			return nil, err
		}
	} else {
		// Default to existing HTTP/generic mock recording.
		err := c.Proxy.Record(ctx, id, m, opts)
		if err != nil {
			close(m) // Ensure channel is closed on error.
			return nil, err
		}
	}

	return m, nil
}
