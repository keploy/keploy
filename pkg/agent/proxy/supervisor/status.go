package supervisor

// Status describes how a parser's Run ended.
type Status int

const (
	// StatusOK means the parser returned a nil error.
	StatusOK Status = iota
	// StatusError means the parser returned a non-nil error (and did not panic).
	StatusError
	// StatusPanicked means the parser panicked; the supervisor recovered.
	StatusPanicked
	// StatusHung means the activity watchdog declared the parser stuck.
	StatusHung
	// StatusMemCap means the parser's per-connection memory cap was exceeded.
	StatusMemCap
	// StatusCanceled means the outer context was cancelled before the
	// parser returned.
	StatusCanceled
)

// String returns a short lowercase label suitable for logs and metrics.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusError:
		return "error"
	case StatusPanicked:
		return "panicked"
	case StatusHung:
		return "hung"
	case StatusMemCap:
		return "mem_cap"
	case StatusCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

// Result is returned from Supervisor.Run. Callers use it to decide what
// to do with the connection next: a clean StatusOK / StatusError lets
// the caller close cleanly, while FallthroughToPassthrough asks the
// caller to hand the real sockets to the raw relay path rather than
// tearing them down.
type Result struct {
	// Status is the reason Run returned.
	Status Status

	// Err is the parser's returned error, or a wrapped panic value.
	// It is nil for StatusOK.
	Err error

	// FallthroughToPassthrough is true when the caller should hand
	// the real sockets to raw passthrough rather than closing them.
	// Set for StatusPanicked, StatusHung, StatusMemCap. An ordinary
	// parser-returned error (StatusError) does NOT by itself force
	// fallthrough: the caller picks, because some protocols treat
	// decode failures as "close cleanly" while others want fallback.
	FallthroughToPassthrough bool
}
