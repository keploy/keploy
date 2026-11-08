package models

import "context"

// testEndReadKey is the unexported context key WithTestEndRead sets.
type testEndReadKey struct{}

// TestEndReadQuery is the query parameter by which keploy test's read of a
// test's misses (WithTestEndRead) names that test to its agent over HTTP:
// GET /mockerrors?testend=<name>.
const TestEndReadQuery = "testend"

// WithTestEndRead marks ctx as keploy test's read of test's misses, made once
// test has its answer or keploy gave up on it: the read every test's end
// makes, with the proven switch on or off. The agent first moves the run's
// place to test's end (integrations.RunPlacer), so an answer recorded after
// test ended, before the next test began, is due from then on, as a test's
// end under exact serving already does. An empty test marks nothing.
func WithTestEndRead(ctx context.Context, test string) context.Context {
	if test == "" {
		return ctx
	}
	return context.WithValue(ctx, testEndReadKey{}, test)
}

// TestEndRead is the test whose end ctx was marked as reading
// (WithTestEndRead), and whether it was marked.
func TestEndRead(ctx context.Context) (string, bool) {
	test, _ := ctx.Value(testEndReadKey{}).(string)
	return test, test != ""
}
