package Update

import "context"

// Updater defines the contract for updating keploy.
type Updater interface {
	Update(ctx context.Context) error
}
