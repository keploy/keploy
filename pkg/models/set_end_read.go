package models

import "context"

// setEndReadKey is the unexported context key WithSetEndRead sets.
type setEndReadKey struct{}

// SetEndReadQuery is the query parameter, set to "1", by which keploy test's
// read of a test set's end (WithSetEndRead) reaches its agent over HTTP:
// GET /mockerrors?setend=1.
const SetEndReadQuery = "setend"

// WithSetEndRead marks ctx as keploy test's read of a test set's end: the
// misses of the calls its application made after the set's last test, read
// once that test is over (the replay's unclaimed calls, proven switch). The
// agent then has every parser that holds a call for a test of the set file
// its miss before the read (integrations.SetEndFiler): no test of the set is
// left to answer it. Every other read of the misses, a test's end among them,
// is unmarked.
func WithSetEndRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, setEndReadKey{}, true)
}

// SetEndRead reports whether ctx is marked as a test set's end read
// (WithSetEndRead).
func SetEndRead(ctx context.Context) bool {
	marked, _ := ctx.Value(setEndReadKey{}).(bool)
	return marked
}
