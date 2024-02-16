package tools

import "context"

type Service interface {
	Update(ctx context.Context) error
}
