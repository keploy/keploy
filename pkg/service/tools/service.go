// Package tools provides utility functions for the service package.
package tools

import (
	"context"
)

type Service interface {
	Update(ctx context.Context) error
	CreateConfig(ctx context.Context, filePath string, config string) error
}

type teleDB interface {
}
