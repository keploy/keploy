package Normalise

import (
	"fmt"

	"go.uber.org/zap"
)

// newNormaliser initializes a new normaliser instance.
func NewNormaliser(logger *zap.Logger) Normaliser {
	return &normaliser{
		logger: logger,
	}
}

type normaliser struct {
	logger *zap.Logger
}

func (n *normaliser) Normalise(path string) {
	fmt.Println("Normalising testcases at path:", path)

}
