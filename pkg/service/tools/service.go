package tools

import "context"

// Updater defines the contract for updating keploy.
type Tools interface {
	Update(ctx context.Context) error
}
