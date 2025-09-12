package models

import "context"

// TestCasePersister defines the function signature for saving a TestCase.
// By placing it in the models package, both the core and proxy packages can
// reference it without creating a circular dependency.
type TestCasePersister func(ctx context.Context, testCase *TestCase) error