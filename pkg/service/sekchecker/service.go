package sekchecker

import "context"

type Service interface {
	Start(ctx context.Context) error
	AddCustomRule(ctx context.Context) error
	RemoveCustomRule(ctx context.Context) error
	UpdateCustomRule(ctx context.Context) error
	ListCustomRules(ctx context.Context) error
}
