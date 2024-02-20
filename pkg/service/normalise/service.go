package normalise

// Normaliser is an interface for normalising testcases.
type Normaliser interface {
	Normalise(path string, testSet string, testCases string)
}
