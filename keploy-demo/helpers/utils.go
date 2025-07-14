package helpers

// AdderImpl implements the main.Adder interface.
type AdderImpl struct{}

// Add adds two integers and returns the sum.
func (AdderImpl) Add(a, b int) int {
	return a + b
}
