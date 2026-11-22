package models

import "context"

// mockDocReceiverKey is the unexported context key under which a recorder
// installs the receiver for the document a MockDB writes for a mock.
type mockDocReceiverKey struct{}

// MockDocReceiver receives the document a MockDB wrote for mock: the bytes it
// appended to the mocks file for it, in format ("yaml" or "json", the
// storageFormat spelling). doc is valid only for the call.
type MockDocReceiver func(mock *Mock, doc []byte, format string)

// WithMockDocReceiver returns a context on which a MockDB's InsertMock hands
// receive the document it wrote for the mock. The recorders use it to give the
// AfterMockInsert hooks that document (record.MockContext.Encoded), so a hook
// that keeps a copy of the mocks file does not encode every mock a second time.
//
// It travels in the context rather than as a method a MockDB may implement
// because MockDBs are wrapped by embedding: a wrapper that overrides
// InsertMock (k8s-proxy counts every mock that way) would have such a method
// promoted from the MockDB it embeds, and a recorder calling it would go
// around the wrapper. On the context, the wrapper's InsertMock runs, and
// passes ctx on to the MockDB that writes.
//
// A MockDB calls receive (through HandMockDoc) once it has written the
// document (MockYaml: written and flushed), and only then: not for a mock it
// failed to write, nor for one it writes in the background (gob). It calls it
// before InsertMock returns, on the goroutine InsertMock runs on, so the
// recorder reads what it was handed without synchronizing. MockYaml calls it
// with the file unlocked. It names the mock the document is for, so a document
// a wrapping MockDB writes for another mock on the same context is not taken
// for this one's; and a wrapper that writes the same mock twice (to two
// stores) hands two, which the recorder cannot tell apart, so it hands the
// hooks neither. receive copies what it keeps. A nil receive is ignored.
func WithMockDocReceiver(ctx context.Context, receive MockDocReceiver) context.Context {
	if receive == nil {
		return ctx
	}
	return context.WithValue(ctx, mockDocReceiverKey{}, receive)
}

// HandMockDoc passes doc, the document a MockDB has just written for mock in
// format, to the receiver WithMockDocReceiver installed on ctx. Without one it
// does nothing.
func HandMockDoc(ctx context.Context, mock *Mock, doc []byte, format string) {
	if receive, ok := ctx.Value(mockDocReceiverKey{}).(MockDocReceiver); ok {
		receive(mock, doc, format)
	}
}
