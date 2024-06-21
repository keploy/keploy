//go:build linux

package orchestrator

import "context"

type Service interface {
	ReRecord(ctx context.Context) error
}
