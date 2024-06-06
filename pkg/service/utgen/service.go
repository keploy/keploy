package utgen

import (
	"context"
)

type Service interface {
	Start(ctx context.Context) error
}
