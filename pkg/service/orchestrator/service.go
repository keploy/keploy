// Package orchestrator acts as a main brain for both the record and replay services
package orchestrator

import "context"

type Service interface {
	ReRecord(ctx context.Context) error
}
