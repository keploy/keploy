package Normalise

// Normaliser is an interface for normalising testcases.
type Normaliser interface {
	Normalise(path string)
}
