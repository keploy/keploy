package integrations

import "go.keploy.io/server/v3/pkg/models"

// UnservedRecordingFinder is an optional MockMemDb capability for a parser
// that holds a call no recording answers yet and matches it again at each
// window change (WindowPacer), as a server holds a blocking pop on an empty
// list. It bounds the hold by what the recording holds, never by time: the
// parser holds the call only while the set still holds a recording of it
// that has not answered, and past that the call misses at once.
//
// HasUnservedRecording reports whether the staged set holds a recording of
// kind that match accepts and that has answered no request this replay
// session: one the store's trees hold (per-test, session, start-up), or one
// the agent keeps on disk until its test opens, under either switch. A
// recording that answered once is not one, whether it stays where it is (a
// reusable one) or not; one of a test that ended before a call took it is.
// match sees each recording at most once, and must not keep it. err is why
// the store cannot read them all: the parser then does not hold the call.
type UnservedRecordingFinder interface {
	HasUnservedRecording(kind models.Kind, match func(*models.Mock) bool) (bool, error)
}
