package secure

import "context"

type Service interface {
	Start(ctx context.Context) error
	AddCustomCheck(ctx context.Context) error
	RemoveCustomCheck(ctx context.Context) error
	UpdateCustomCheck(ctx context.Context) error
	ListChecks(ctx context.Context) error
}
