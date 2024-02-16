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

func (n *normaliser) Normalise() {
	fmt.Println("Normalising testcases")
	// Logic to normalise testcases
}
