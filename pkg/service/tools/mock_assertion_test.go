package tools

// Compile-time proof the mock satisfies the interface it mocks. Without
// this, adding a method to TestSetConfig leaves the mock silently behind
// until some future test tries to use it.
var _ TestSetConfig = (*MockTestSetConfig)(nil)
